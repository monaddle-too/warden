package policy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	gitOutputLimit  = 262144
	gitScratchLimit = 256 * 1024 * 1024
)

// GitExecutable is the trusted git binary used for reviews.
var GitExecutable = "git"

// LimitedGitLauncher, when set, is the argv prefix that re-executes this
// binary in its `git-limited` mode to apply resource limits before exec.
var LimitedGitLauncher []string

// RunLimitedGit is the `git-limited` subcommand: apply resource limits in a
// fresh child, then exec the requested program. Linux only; elsewhere the
// review runs git directly, as the Python implementation did.
func RunLimitedGit(args []string) error {
	if len(args) == 0 {
		return errors.New("git-limited requires a command")
	}
	if runtime.GOOS == "linux" {
		for _, limit := range []struct {
			resource int
			value    uint64
		}{{syscall.RLIMIT_FSIZE, 128 * 1024 * 1024}, {syscall.RLIMIT_AS, 768 * 1024 * 1024}, {syscall.RLIMIT_CPU, 60}} {
			if err := syscall.Setrlimit(limit.resource, &syscall.Rlimit{Cur: limit.value, Max: limit.value}); err != nil {
				return err
			}
		}
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, args, os.Environ())
}

// GitRunner runs one git invocation in the review scratch directory.
type GitRunner func(ctx context.Context, args []string, data []byte, network bool, limit int) ([]byte, error)

type gitReviewer struct {
	directory string
	auth      string
	upstream  string
	active    func() (bool, error)
}

func (g *gitReviewer) run(ctx context.Context, args []string, data []byte, network bool, limit int) ([]byte, error) {
	env := []string{"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "HOME=" + g.directory, "LANG=C", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0="}
	if network && g.auth != "" {
		env = append(env, "GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_1=http.extraHeader", "GIT_CONFIG_VALUE_1=Authorization: "+g.auth,
			"GIT_CONFIG_KEY_2=http.curloptResolve", "GIT_CONFIG_VALUE_2=github.com:443:"+g.upstream)
	}
	argv := append([]string{GitExecutable, "-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"-c", "http.followRedirects=false", "-c", "http.sslVerify=true", "-c", "http.proxy=",
		"-c", "core.attributesFile=/dev/null", "-c", "core.pager=cat", "-C", g.directory}, args...)
	if runtime.GOOS == "linux" && len(LimitedGitLauncher) > 0 {
		argv = append(append([]string{}, LimitedGitLauncher...), argv...)
	}
	return runBounded(ctx, argv, env, data, limit, g.active, g.directory)
}

// runBounded runs a process with bounded stdout, a 90 second deadline,
// permission rechecks and a scratch size limit, killing the whole group on
// any violation.
func runBounded(ctx context.Context, argv, env []string, data []byte, limit int, active func() (bool, error), scratch string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stderr = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	kill := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	type outcome struct {
		data []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		var result []byte
		buf := make([]byte, 65536)
		var readErr error
		for {
			n, err := stdout.Read(buf)
			result = append(result, buf[:n]...)
			if len(result) > limit {
				readErr = valueErr("review or pack exceeds size limit; split the change")
				break
			}
			if err != nil {
				if err != io.EOF {
					readErr = err
				}
				break
			}
		}
		if readErr != nil {
			kill()
		}
		waitErr := cmd.Wait()
		if readErr != nil {
			done <- outcome{nil, readErr}
			return
		}
		if waitErr != nil {
			done <- outcome{nil, valueErr("Git review failed; refresh the remote, use a non-thin push, and retry")}
			return
		}
		done <- outcome{result, nil}
	}()
	started := time.Now()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case result := <-done:
			return result.data, result.err
		case <-ticker.C:
			var violation error
			if time.Since(started) > 90*time.Second {
				violation = valueErr("Git review timed out")
			} else if active != nil {
				if ok, err := active(); err != nil || !ok {
					violation = valueErr("repository read permission expired during review")
				}
			}
			if violation == nil && scratch != "" && directorySize(scratch) > gitScratchLimit {
				violation = valueErr("repository exceeds review scratch limit")
			}
			if violation != nil {
				kill()
				<-done
				return nil, violation
			}
		case <-ctx.Done():
			kill()
			<-done
			return nil, ctx.Err()
		}
	}
}

func directorySize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// InspectPush fetches the approved repository into tmpfs scratch, verifies
// the pushed objects and renders a review; it returns the review and a
// canonical request body containing only reachable objects.
func InspectPush(ctx context.Context, repository string, body []byte, auth, upstream string, active func() (bool, error)) (map[string]any, []byte, error) {
	scratch := ""
	if runtime.GOOS == "linux" {
		if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
			scratch = "/dev/shm"
		}
	}
	directory, err := os.MkdirTemp(scratch, "warden-review-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(directory)
	reviewer := &gitReviewer{directory: directory, auth: auth, upstream: upstream, active: active}
	git := func(args []string, data []byte, network bool, limit int) ([]byte, error) {
		return reviewer.run(ctx, args, data, network, limit)
	}
	if _, err = git([]string{"init", "--bare", "--quiet", "--template="}, nil, false, gitOutputLimit); err != nil {
		return nil, nil, err
	}
	// Reuse exactly the approved repository identity, with independent DNS
	// pinning, upstream TLS verification and no redirect/credential helpers.
	remote := "https://github.com/" + repository + ".git"
	if _, err = git([]string{"fetch", "--quiet", "--no-tags", "--no-recurse-submodules", remote, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}, nil, true, gitOutputLimit); err != nil {
		return nil, nil, err
	}
	heads, err := git([]string{"for-each-ref", "--format=%(refname)", "refs/heads/"}, nil, false, gitOutputLimit)
	if err != nil {
		return nil, nil, err
	}
	if len(heads) > 0 {
		if _, err = git([]string{"fetch", "--quiet", "--no-tags", "--no-recurse-submodules", remote, "+HEAD:refs/warden/base"}, nil, true, gitOutputLimit); err != nil {
			return nil, nil, err
		}
	}
	update, pack, err := GitPush(body)
	if err != nil {
		return nil, nil, err
	}
	return InspectObjects(git, update, pack, body)
}

// InspectObjects verifies and reviews pushed objects in a prepared scratch
// repository. Also used by integration tests against local repositories.
func InspectObjects(git func(args []string, data []byte, network bool, limit int) ([]byte, error), update GitUpdate, pack, body []byte) (map[string]any, []byte, error) {
	run := func(args ...string) ([]byte, error) { return git(args, nil, false, gitOutputLimit) }
	if len(pack) > 0 {
		if _, err := git([]string{"index-pack", "--stdin", "--fix-thin", "--strict"}, pack, false, gitOutputLimit); err != nil {
			return nil, nil, err
		}
	}
	kind, err := run("cat-file", "-t", update.New)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(string(kind)) != "commit" {
		return nil, nil, valueErr("branch target must be a commit")
	}
	var base string
	if update.Old != ZeroOID {
		current, err := run("rev-parse", "--verify", update.Ref)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(string(current)) != update.Old {
			return nil, nil, valueErr("remote branch changed; fetch and rebase before pushing")
		}
		// Force pushes fail closed.
		if _, err = run("merge-base", "--is-ancestor", update.Old, update.New); err != nil {
			return nil, nil, err
		}
		base = update.Old
	} else {
		refsRaw, err := run("for-each-ref", "--format=%(refname)")
		if err != nil {
			return nil, nil, err
		}
		refs := strings.Split(strings.TrimSpace(string(refsRaw)), "\n")
		hasBase := false
		for _, ref := range refs {
			if ref == update.Ref {
				return nil, nil, valueErr("branch already exists; fetch before pushing")
			}
			if ref == "refs/warden/base" {
				hasBase = true
			}
		}
		if hasBase {
			baseRaw, err := run("rev-parse", "--verify", "refs/warden/base")
			if err != nil {
				return nil, nil, err
			}
			// A new feature branch is reviewed against its actual fork point.
			mergeBase, err := run("merge-base", strings.TrimSpace(string(baseRaw)), update.New)
			if err != nil {
				return nil, nil, err
			}
			base = strings.TrimSpace(string(mergeBase))
		} else {
			empty, err := git([]string{"hash-object", "-w", "-t", "tree", "--stdin"}, []byte{}, false, gitOutputLimit)
			if err != nil {
				return nil, nil, err
			}
			base = strings.TrimSpace(string(empty))
		}
	}
	// Inspect ALL objects/commits being sent, including merged histories that
	// a tip-only diff would hide. Existing remote history is already upstream.
	outgoing := []string{update.New, "--not", "--all"}
	// Connectivity and SHA verification are done by Git, not a guest report.
	if _, err = run(append([]string{"rev-list", "--objects", "--missing=error"}, outgoing...)...); err != nil {
		return nil, nil, err
	}
	raw, err := run(append(append([]string{"log", "--format=", "--raw", "--no-renames", "--diff-merges=first-parent"}, outgoing...), "--")...)
	if err != nil {
		return nil, nil, err
	}
	if bytes.Contains(raw, []byte("160000")) {
		return nil, nil, valueErr("submodule updates require a separate workflow and are blocked")
	}
	numstat, err := run(append(append([]string{"log", "--format=", "--numstat", "--no-renames", "--diff-merges=first-parent"}, outgoing...), "--")...)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range bytes.Split(numstat, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("-\t-\t")) {
			return nil, nil, valueErr("binary changes cannot be reviewed here; publish text-only changes")
		}
	}
	patch, err := run(append(append([]string{"log", "--format=fuller", "--patch", "--diff-merges=first-parent", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color"}, outgoing...), "--")...)
	if err != nil {
		return nil, nil, err
	}
	stat, err := run("diff", "--stat", "--no-renames", base, update.New, "--")
	if err != nil {
		return nil, nil, err
	}
	commits, err := run(append([]string{"log", "--no-show-signature", "--format=%H %s"}, outgoing...)...)
	if err != nil {
		return nil, nil, err
	}
	// Repack only objects reachable from the approved tip and missing
	// upstream. Guest-supplied unreachable objects never hitch a ride to
	// GitHub. Fixed packing options make identical retries byte-identical.
	knownRaw, err := run("for-each-ref", "--format=%(objectname)")
	if err != nil {
		return nil, nil, err
	}
	revisions := update.New + "\n"
	for _, oid := range strings.Split(strings.TrimSpace(string(knownRaw)), "\n") {
		if oid != "" {
			revisions += "^" + oid + "\n"
		}
	}
	clean, err := git([]string{"-c", "pack.threads=1", "pack-objects", "--stdout", "--revs"}, []byte(revisions), false, 8*1024*1024)
	if err != nil {
		return nil, nil, err
	}
	_, offset, err := GitPacket(body, 0)
	if err != nil {
		return nil, nil, err
	}
	_, offset, err = GitPacket(body, offset)
	if err != nil {
		return nil, nil, err
	}
	canonical := append(append([]byte{}, body[:offset]...), clean...)
	review := map[string]any{"update": map[string]any{"old": update.Old, "new": update.New, "ref": update.Ref}, "base": base,
		"pack_request_sha256": sha256Hex(canonical), "patch": visibleText(patch), "stat": visibleText(stat), "commits": visibleText(commits), "truncated": false}
	return review, canonical, nil
}

// visibleText renders bytes as reviewable text: invalid UTF-8 becomes \xNN
// escapes and control or bidirectional characters become \uXXXX escapes.
func visibleText(data []byte) string {
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, "\\x%02x", data[0])
			data = data[1:]
			continue
		}
		hidden := (r < 32 && r != '\n' && r != '\t') || r == 127
		switch r {
		case 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069:
			hidden = true
		}
		if hidden {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
		data = data[size:]
	}
	return b.String()
}
