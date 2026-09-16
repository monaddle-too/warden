package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"warden/chat/internal/hoststats"
	"warden/chat/internal/release"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{1,70}$`)
var repository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var commit = regexp.MustCompile(`^[a-f0-9]{40}$`)

type session struct{ ProjectID, Directory, Base, RolloutPath, RuntimeVersion string }

const MaxParallelSessions = 2

type admission struct {
	users     int
	streaming bool
}

type Worker struct {
	Legacy                     bool // An explicit separate worker mode; never routes protocol 2 to legacy execution.
	ordinarySlots              chan struct{}
	controlSlots               chan struct{}
	Revision                   string
	Parallel                   int
	Retained                   int
	activeSessions             atomic.Int64
	Root, Executable, Template string
	// Spares is how many booted, unbound guests to keep ready so a new
	// environment skips sandbox creation. They sit beside MaxResident.
	Spares       int
	spareBusy    bool      // a spare is being created (guarded by mu)
	spareRetryAt time.Time // next creation attempt after a failure (guarded by mu)
	// claudeDigest caches the SHA-256 of the host Claude executable, keyed by
	// its size:mtime fingerprint, for comparison with a guest image manifest.
	claudeDigestKey, claudeDigest string
	metrics                       *hoststats.Collector
	CodexPath                     string
	RuntimeDir                    string
	ClaudePath                    string
	mu                            sync.Mutex
	sessions                      map[string]*admission
	users                         int
	changed                       chan struct{}
	managed                       *managedState
	controls                      *controlState
	Gate                          Enforcement
	Runtime                       RuntimeDriver
	RepositorySource              RepositorySource
	IdleTimeout                   time.Duration
	MaxResident                   int
	MemoryMB                      int
	Now                           func() time.Time
}

func (w *Worker) parallelLimit() int {
	if w.Parallel > 0 {
		return w.Parallel
	}
	return MaxParallelSessions
}
func (w *Worker) retainedLimit() int {
	if w.Retained > 0 {
		return w.Retained
	}
	return 32
}

func NewWorker(root, executable, template string) *Worker {
	return &Worker{metrics: &hoststats.Collector{Root: root}, sessions: map[string]*admission{}, ordinarySlots: make(chan struct{}, 4), controlSlots: make(chan struct{}, 16), controls: &controlState{bindings: map[string]Request{}, cancel: map[string]context.CancelFunc{}, cancelled: map[string]bool{}}, changed: make(chan struct{}), Root: root, Executable: executable, Template: template, CodexPath: filepath.Join(root, "bin", "codex")}
}
func (w *Worker) Serve(ctx context.Context, l net.Listener) error {
	// A worker restart must not leave detached app servers in old VMs. SBX
	// contents persist; the backend reports interrupted runs and can resume them.
	if w.Legacy {
		if err := w.stopManaged(ctx); err != nil {
			return err
		}
		if err := w.recoverOpenAIRuns(ctx); err != nil {
			return err
		}
	} else if err := w.initializeManaged(ctx); err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	clients := make(chan struct{}, 64)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case clients <- struct{}{}:
		default:
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			defer func() { <-clients }()
			if w.Legacy {
				w.handleLegacy(ctx, c)
			} else {
				w.handle(ctx, c)
			}
		}()
	}
}
func (w *Worker) stopManaged(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	b, err := command(ctx, w.Executable, "ls", "--json").Output()
	if err != nil {
		return errors.New("cannot inspect SBX inventory; verify Docker login and policy initialization")
	}
	var list struct {
		Sandboxes []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"sandboxes"`
	}
	if err = json.Unmarshal(b, &list); err != nil {
		return err
	}
	for _, s := range list.Sandboxes {
		if strings.HasPrefix(s.Name, "ws-") && s.Status == "running" {
			if err = command(ctx, w.Executable, "stop", s.Name).Run(); err != nil {
				return err
			}
		}
	}
	return nil
}
func (w *Worker) handleLegacy(parent context.Context, c net.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(c)
	line, err := readLine(reader, 1<<20)
	var r Request
	if err == nil {
		err = json.Unmarshal(line, &r)
	}
	send := func(v Response) {
		_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_ = json.NewEncoder(c).Encode(v)
		_ = c.SetWriteDeadline(time.Time{})
	}
	if err != nil || r.Version != 1 || r.ChatID != "" || r.SandboxID != "" || r.PrincipalID != "" {
		send(Response{Error: "invalid worker protocol request"})
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if r.Operation != "stream" {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithTimeout(ctx, 5*time.Minute)
		defer deadlineCancel()
	}
	if r.Operation == "stats" {
		sample, err := w.metrics.Snapshot()
		if err != nil {
			send(Response{Error: "execution host metrics unavailable"})
			return
		}
		active := int(w.activeSessions.Load())
		send(Response{Stats: &sample, ActiveSessions: active, SessionLimit: w.parallelLimit(), Revision: w.Revision})
		return
	}
	if r.Operation == "health" {
		send(Response{Output: "sbx", Revision: w.Revision})
		return
	}
	if !identifier.MatchString(r.ProjectID) {
		send(Response{Error: "invalid project ID"})
		return
	}
	if r.Operation == "register" || r.Operation == "refresh" {
		err = w.register(ctx, r)
		if err != nil {
			send(Response{Error: err.Error()})
		} else {
			send(Response{})
		}
		return
	}
	if !identifier.MatchString(r.SessionID) {
		send(Response{Error: "invalid session ID"})
		return
	}
	s, release, err := w.acquire(ctx, r)
	if err != nil {
		send(Response{Error: err.Error()})
		return
	}
	release = sync.OnceFunc(release)
	defer release()
	if r.Operation == "stream" {
		var auth Response
		args := []string{"/tmp/workspace-codex", "app-server", "--listen", "stdio://"}
		if r.OpenAIAPIKey != "" {
			var cleanup func()
			auth, cleanup, err = w.prepareOpenAI(ctx, r)
			if err != nil {
				send(Response{Error: err.Error()})
				return
			}
			defer cleanup()
			args = append(args, "-c", `model_provider="openai"`, "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `forced_login_method="api"`, "-c", `model_providers.openai.base_url="https://api.openai.com/v1"`)
			args = append([]string{"env", "-u", "OPENAI_API_KEY", "-u", "OPENAI_BASE_URL", "-u", "CODEX_API_KEY"}, args...)
		}
		directory := r.Directory
		if directory == "" {
			directory = s.Directory
		}
		cmd := w.exec(ctx, r.SessionID, directory, args...)
		input, err := cmd.StdinPipe()
		if err != nil {
			send(Response{Error: err.Error()})
			return
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			send(Response{Error: err.Error()})
			return
		}
		// stderr can contain agent-controlled data; keep it out of service logs.
		logs := filepath.Join(w.Root, "logs")
		_ = os.MkdirAll(logs, 0700)
		log, err := os.OpenFile(filepath.Join(logs, r.SessionID+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			send(Response{Error: err.Error()})
			return
		}
		defer log.Close()
		cmd.Stderr = &limitedWriter{W: log, N: 8 << 20}
		if err = cmd.Start(); err != nil {
			send(Response{Error: "could not start sandbox app server"})
			return
		}
		auth.Directory = s.Directory
		auth.Base = s.Base
		send(auth)
		go func() { _, _ = io.Copy(input, reader); input.Close(); cancel() }()
		_, _ = io.Copy(c, output)
		cancel()
		_ = cmd.Wait()
		return
	}
	var result Response
	switch r.Operation {
	case "prepare":
		result = Response{Directory: s.Directory, Base: s.Base, RolloutPath: s.RolloutPath}
	case "fetch":
		result, err = w.fetch(ctx, r, s)
	case "git":
		if len(r.Args) == 0 || len(r.Args) > 100 {
			err = errors.New("invalid git request")
			break
		}
		directory := r.Directory
		if directory == "" {
			directory = s.Directory
		}
		result.Output, err = w.output(ctx, r.SessionID, directory, append([]string{"git"}, r.Args...)...)
	case "file", "stat":
		raw, callErr := w.output(ctx, r.SessionID, s.Directory, "python3", "-c", fileScript, s.Directory, r.Directory, r.Operation)
		err = callErr
		if err == nil {
			err = json.Unmarshal([]byte(raw), &result)
		}
	case "url-check":
		if len(r.Args) != 1 {
			err = errors.New("invalid URL check")
			break
		}
		_, err = w.output(ctx, r.SessionID, s.Directory, "python3", "-c", `import sys,urllib.request; r=urllib.request.urlopen(sys.argv[1],timeout=10); assert 200<=r.status<400`, r.Args[0])
	case "snapshot", "export", "snapshot-v2", "export-v2":
		// All staging happens in an alternate index, leaving the agent's checkout,
		// index, branch, and uncommitted changes intact. A deterministic commit binds
		// the preview to the exact content the user subsequently authorizes.
		var raw string
		directory := r.Directory
		if directory == "" {
			directory = s.Directory
		}
		script := snapshotScript
		mode := r.Operation
		if strings.HasSuffix(mode, "-v2") {
			script = snapshotV2Script
			mode = strings.TrimSuffix(mode, "-v2")
		}
		if r.PublicationHead != "" && !commit.MatchString(r.PublicationHead) || r.RemoteHead != "" && !commit.MatchString(r.RemoteHead) {
			err = errors.New("invalid publication head")
			break
		}
		raw, err = w.output(ctx, r.SessionID, directory, "bash", "-c", script, "workspace-snapshot", s.Base, mode, r.PublicationHead, r.RemoteHead)
		if err == nil {
			err = json.Unmarshal([]byte(raw), &result)
		}
		if err != nil && strings.HasSuffix(r.Operation, "-v2") {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				switch exit.ExitCode() {
				case 42:
					err = errors.New("Resolve and stage all conflicts before publishing")
				case 43:
					err = errors.New("Finish the local merge commit before publishing")
				case 44:
					err = errors.New("The PR changed on GitHub. Resume the task and merge refs/remotes/workspace/pr before updating it")
				}
			}
		}
		if err == nil && r.Expected != "" && result.Head != r.Expected {
			err = errors.New("sandbox changed since review; reload changes before creating a PR")
		}
		if err == nil && (r.Operation == "export" || r.Operation == "export-v2") {
			dir := filepath.Join(w.Root, "exports")
			err = os.MkdirAll(dir, 0700)
			if err == nil {
				err = os.WriteFile(filepath.Join(dir, r.SessionID+".bundle.tmp"), result.Bundle, 0600)
			}
			if err == nil {
				err = os.Rename(filepath.Join(dir, r.SessionID+".bundle.tmp"), filepath.Join(dir, r.SessionID+".bundle"))
			}
			if err == nil {
				err = atomicJSON(filepath.Join(dir, r.SessionID+".json"), map[string]string{"head": result.Head, "project": r.ProjectID})
			}
		}
	case "import", "import-bundle":
		if !identifier.MatchString(r.SourceSessionID) || !commit.MatchString(r.Expected) {
			err = errors.New("invalid import identity")
			break
		}
		if r.Operation == "import-bundle" {
			if r.BundleSize <= 0 || r.BundleSize > 64<<20 {
				err = errors.New("invalid import size")
				break
			}
			cmd := w.exec(ctx, r.SessionID, s.Directory, "bash", "-c", importScript, "workspace-import", r.Expected, "refs/heads/workspace-task-"+strings.ToLower(r.SourceSessionID))
			cmd.Stdin = io.LimitReader(reader, r.BundleSize)
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			if cmd.Run() != nil {
				err = errors.New("could not import checked remote snapshot")
			}
			break
		}
		var identity map[string]string
		var b []byte
		b, err = os.ReadFile(filepath.Join(w.Root, "exports", r.SourceSessionID+".json"))
		if err != nil {
			break
		}
		err = json.Unmarshal(b, &identity)
		if err != nil {
			break
		}
		if identity["head"] != r.Expected || identity["project"] != r.ProjectID {
			err = errors.New("export does not match project or commit")
			break
		}
		var bundle *os.File
		bundle, err = os.Open(filepath.Join(w.Root, "exports", r.SourceSessionID+".bundle"))
		if err != nil {
			break
		}
		defer bundle.Close()
		cmd := w.exec(ctx, r.SessionID, s.Directory, "bash", "-c", importScript, "workspace-import", r.Expected, "refs/heads/workspace-task-"+strings.ToLower(r.SourceSessionID))
		cmd.Stdin = bundle
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err = cmd.Run(); err != nil {
			err = errors.New("could not import checked task snapshot into review sandbox")
		}
	default:
		err = errors.New("unsupported worker operation")
	}
	// Completion must release admission before its response becomes visible.
	// The backend may immediately open the app-server stream after Git returns.
	release()
	if err != nil {
		send(Response{Error: err.Error()})
	} else {
		send(result)
	}
}
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		p, prefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(out)+len(p) > max {
			return nil, errors.New("request too large")
		}
		out = append(out, p...)
		if !prefix {
			return out, nil
		}
	}
}

type limitedWriter struct {
	W io.Writer
	N int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n > w.N {
		return 0, errors.New("command output exceeds limit")
	}
	w.N -= n
	return w.W.Write(p)
}
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	return cmd
}
func (w *Worker) exec(ctx context.Context, id, dir string, args ...string) *exec.Cmd {
	a := []string{"exec", "-i", "-w", dir, "ws-" + strings.ToLower(id)}
	a = append(a, args...)
	return command(ctx, w.Executable, a...)
}
func (w *Worker) output(ctx context.Context, id, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := w.exec(ctx, id, dir, args...)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{&out, 96 << 20}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("sandbox %s failed: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}
func (w *Worker) acquire(ctx context.Context, r Request) (session, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fail := func(e error) (session, func(), error) { return session{}, nil, e }
	waitCtx, cancelWait := context.WithTimeout(ctx, 15*time.Second)
	defer cancelWait()
	for w.sessions[r.SessionID] != nil && w.sessions[r.SessionID].users > 0 && (r.Operation == "stream" || strings.HasPrefix(r.Operation, "snapshot") || strings.HasPrefix(r.Operation, "export")) {
		if r.Operation != "prepare" && r.Operation != "stream" {
			return fail(errors.New("sandbox is busy; wait for the current run to finish"))
		}
		// Closing a stream disconnects immediately, but its remote process may
		// need a moment to exit. Queue the next run behind that cleanup.
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-changed:
		case <-waitCtx.Done():
		}
		w.mu.Lock()
		if waitCtx.Err() != nil {
			return fail(fmt.Errorf("waiting for previous sandbox run: %w", waitCtx.Err()))
		}
	}
	meta := filepath.Join(w.Root, "sessions", r.SessionID+".json")
	var s session
	b, err := os.ReadFile(meta)
	if err == nil {
		err = json.Unmarshal(b, &s)
		if err != nil || s.ProjectID != r.ProjectID {
			return fail(errors.New("session belongs to another project"))
		}
	} else if os.IsNotExist(err) {
		source := filepath.Join(w.Root, "repositories", r.ProjectID)
		if r.ProjectID != "recording" {
			if _, err = os.Stat(filepath.Join(source, ".git")); err != nil {
				return fail(errors.New("project repository is not registered with the isolation worker; connect a GitHub repository first"))
			}
		} else {
			source = "/tmp"
		}
		entries, _ := os.ReadDir(filepath.Join(w.Root, "sessions"))
		if len(entries) >= w.retainedLimit() {
			return fail(fmt.Errorf("worker has %d persistent sandboxes; export and retire unused sandboxes before creating more", w.retainedLimit()))
		}
		s = session{ProjectID: r.ProjectID, Directory: source}
		// Persist the identity before creation. A crash after SBX creates its VM
		// can then be reconciled without replacing or losing the private clone.
		if err = atomicJSON(meta, s); err != nil {
			return fail(err)
		}
	} else {
		return fail(err)
	}
	if w.sessions[r.SessionID] == nil || w.sessions[r.SessionID].users == 0 {
		if err := w.recoverOpenAI(ctx, r.SessionID); err != nil {
			return fail(err)
		}
	}
	if w.sessions[r.SessionID] == nil {
		if len(w.sessions) >= w.parallelLimit() {
			// Retire only an idle VM. Active streams in other sessions remain
			// untouched; each keeps its own process, clone and scoped secrets.
			for id, active := range w.sessions {
				if active.users == 0 {
					if err := command(ctx, w.Executable, "stop", "ws-"+strings.ToLower(id)).Run(); err != nil {
						return fail(errors.New("could not stop idle sandbox"))
					}
					delete(w.sessions, id)
					break
				}
			}
			if len(w.sessions) >= w.parallelLimit() {
				return fail(errors.New("all sandbox slots are busy; retry when a task finishes"))
			}
		}
		w.sessions[r.SessionID] = &admission{}
	}
	active := w.sessions[r.SessionID]
	if s.Base == "" && s.RuntimeVersion == "" {
		names, e := command(ctx, w.Executable, "ls", "--quiet").Output()
		if e != nil {
			return fail(errors.New("could not inspect SBX inventory"))
		}
		exists := false
		for _, name := range strings.Fields(string(names)) {
			exists = exists || name == "ws-"+strings.ToLower(r.SessionID)
		}
		if !exists {
			var fs syscall.Statfs_t
			if err = syscall.Statfs(w.Root, &fs); err != nil {
				return fail(err)
			}
			if uint64(fs.Bavail)*uint64(fs.Bsize) < 8<<30 {
				return fail(errors.New("worker requires at least 8 GiB free before creating another sandbox"))
			}
			args := []string{"create", "--no-share-skills", "--name", "ws-" + strings.ToLower(r.SessionID), "--cpus", "1", "--memory", "1536m", "--template", w.Template, "--deny-network", "169.254.169.254"}
			if r.ProjectID == "recording" {
				args = append(args, "codex")
			} else {
				args = append(args, "--clone", "codex", s.Directory)
			}
			if err = command(ctx, w.Executable, args...).Run(); err != nil {
				return fail(fmt.Errorf("SBX creation failed: %w", err))
			}
		}
		if r.ProjectID != "recording" {
			s.Base, err = w.output(ctx, r.SessionID, s.Directory, "git", "rev-parse", "HEAD")
			if err != nil || !commit.MatchString(s.Base) {
				return fail(errors.New("could not read sandbox base commit"))
			}
		}
		if err = atomicJSON(meta, s); err != nil {
			return fail(err)
		}
	}

	if (r.Operation == "prepare" || r.Operation == "stream") && active.users == 0 && s.RuntimeVersion != release.CodexVersion {
		if err = command(ctx, w.Executable, "cp", w.CodexPath, "ws-"+strings.ToLower(r.SessionID)+":/tmp/workspace-codex").Run(); err != nil {
			return fail(errors.New("could not install pinned Codex binary in sandbox"))
		}
		if err = command(ctx, w.Executable, "cp", filepath.Join(filepath.Dir(w.CodexPath), "codex-code-mode-host"), "ws-"+strings.ToLower(r.SessionID)+":/tmp/codex-code-mode-host").Run(); err != nil {
			return fail(errors.New("could not install pinned Codex code-mode host"))
		}
		if _, err = w.output(ctx, r.SessionID, s.Directory, "sudo", "chmod", "755", "/tmp/workspace-codex", "/tmp/codex-code-mode-host"); err != nil {
			return fail(err)
		}
		s.RuntimeVersion = release.CodexVersion
		if err = atomicJSON(meta, s); err != nil {
			return fail(err)
		}
	}
	if r.Operation == "prepare" && r.ThreadID != "" && s.RolloutPath == "" {
		if !identifier.MatchString(r.ThreadID) {
			return fail(errors.New("invalid thread ID"))
		}
		// Migration files are copied by the operator into this private directory.
		// Never expose the old Codex home, auth files, or other chats to a VM.
		source := filepath.Join(w.Root, "legacy-sessions", r.ThreadID+".jsonl")
		if info, e := os.Stat(source); e == nil && info.Mode().IsRegular() {
			if _, err = w.output(ctx, r.SessionID, s.Directory, "mkdir", "-p", "/home/agent/.codex/sessions/workspace-imports"); err != nil {
				return fail(err)
			}
			target := "/home/agent/.codex/sessions/workspace-imports/" + r.ThreadID + ".jsonl"
			if err = command(ctx, w.Executable, "cp", source, "ws-"+strings.ToLower(r.SessionID)+":"+target).Run(); err != nil {
				return fail(errors.New("could not import saved conversation"))
			}
			if _, err = w.output(ctx, r.SessionID, s.Directory, "sudo", "chown", "agent:agent", target); err != nil {
				return fail(err)
			}
			s.RolloutPath = target
			if err = atomicJSON(meta, s); err != nil {
				return fail(err)
			}
		}
	}
	if active.users == 0 {
		w.activeSessions.Add(1)
	}
	active.users++
	w.users++
	if r.Operation == "stream" {
		active.streaming = true
	}
	return s, sync.OnceFunc(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		active.users--
		if active.users == 0 {
			w.activeSessions.Add(-1)
		}
		w.users--
		close(w.changed)
		w.changed = make(chan struct{})
		if r.Operation == "stream" {
			active.streaming = false
		}
	}), nil
}
func atomicJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = os.WriteFile(path+".tmp", b, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
func (w *Worker) register(ctx context.Context, r Request) error {
	if !repository.MatchString(r.Repository) || strings.Contains(r.Repository, "..") {
		return errors.New("invalid GitHub repository")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	target := filepath.Join(w.Root, "repositories", r.ProjectID)
	if _, err := os.Stat(target); err == nil {
		cmd := command(ctx, "git", "-C", target, "config", "--get", "remote.origin.url")
		b, err := cmd.Output()
		if err == nil && strings.TrimSpace(string(b)) == "https://github.com/"+r.Repository+".git" {
			if r.Operation == "refresh" {
				if len(r.Args) != 1 {
					return errors.New("default branch required")
				}
				if err = command(ctx, "git", "check-ref-format", "refs/heads/"+r.Args[0]).Run(); err != nil {
					return errors.New("invalid default branch")
				}
				cmd = command(ctx, "git", "-C", target, "-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "fetch", "--no-tags", "https://github.com/"+r.Repository+".git", "refs/heads/"+r.Args[0])
				cmd.Env = gitEnvironment(r.Token)
				cmd.Stdout = io.Discard
				cmd.Stderr = io.Discard
				if err = cmd.Run(); err != nil {
					return errors.New("could not refresh GitHub default branch")
				}
				if err = command(ctx, "git", "-C", target, "-c", "core.hooksPath=/dev/null", "reset", "--hard", "FETCH_HEAD").Run(); err != nil {
					return errors.New("could not update private repository cache")
				}
			}
			return nil
		}
		return errors.New("project already has a different registered repository")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(target), "clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	cmd := command(ctx, "git", "-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "clone", "--no-local", "https://github.com/"+r.Repository+".git", temp)
	cmd.Env = gitEnvironment(r.Token)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = cmd.Run(); err != nil {
		return errors.New("could not clone selected GitHub repository")
	}
	if err = command(ctx, "git", "-C", temp, "config", "core.hooksPath", "/dev/null").Run(); err != nil {
		return err
	}
	return os.Rename(temp, target)
}

// HTTP credentials are restricted to github.com and exist only in the trusted
// child process environment. They are never written to clone config or SBX.
func gitEnvironment(token string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))}
	return env
}

const snapshotScript = `set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
index=$(mktemp); rm -f "$index"; export GIT_INDEX_FILE="$index"
bundle=$(mktemp); trap 'rm -f "$index" "$bundle"' EXIT
git -c core.hooksPath=/dev/null read-tree HEAD
git -c core.hooksPath=/dev/null add -A -- .
tree=$(git write-tree)
export GIT_AUTHOR_NAME='Workspace' GIT_AUTHOR_EMAIL='workspace@localhost' GIT_COMMITTER_NAME='Workspace' GIT_COMMITTER_EMAIL='workspace@localhost'
export GIT_AUTHOR_DATE='2001-01-01T00:00:00Z' GIT_COMMITTER_DATE='2001-01-01T00:00:00Z'
head=$(printf 'Workspace reviewed snapshot\n' | git -c core.hooksPath=/dev/null commit-tree "$tree" -p "$1")
git update-ref refs/workspace/export "$head"
if [ "$2" = export ]; then git -c core.hooksPath=/dev/null bundle create "$bundle" refs/workspace/export "^$1"; fi
python3 - "$1" "$head" "$bundle" "$2" <<'PY'
import sys,subprocess,json,base64,os
base,head,bundle,mode=sys.argv[1:]
diff=subprocess.check_output(['git','-c','core.hooksPath=/dev/null','diff','--no-ext-diff','--no-textconv','--binary',base,head,'--','.'])
if len(diff)>2*1024*1024: raise RuntimeError('Diff exceeds 2 MiB review limit')
result={'base':base,'head':head,'diff':diff.decode('utf8','replace')}
if mode=='export':
 if os.path.getsize(bundle)>64*1024*1024: raise RuntimeError('Bundle exceeds 64 MiB limit')
 result['bundle']=base64.b64encode(open(bundle,'rb').read()).decode()
print(json.dumps(result))
PY
`
const fileScript = `import sys,os,json,base64,stat
root,path,mode=sys.argv[1:]
path=os.path.realpath(os.path.join(root,path))
assert any(os.path.commonpath([path,r])==r for r in [os.path.realpath(root),'/tmp/workspace-reviews']), 'File is outside task directories'
st=os.stat(path)
result={}
if mode=='file':
 assert stat.S_ISREG(st.st_mode) and st.st_size<=16*1024*1024, 'File must be regular and at most 16 MiB'
 with open(path,'rb') as f: data=f.read(16*1024*1024+1)
 assert len(data)<=16*1024*1024
 result['bytes']=base64.b64encode(data).decode()
print(json.dumps(result))
`
const importScript = `set -euo pipefail
bundle=$(mktemp); trap 'rm -f "$bundle"' EXIT
cat > "$bundle"
git -c core.hooksPath=/dev/null fetch --no-tags "$bundle" refs/workspace/export
test "$(git rev-parse FETCH_HEAD)" = "$1"
git update-ref "$2" "$1"
`

// Keep v1 snapshot semantics available during API overlap and rollback.
var snapshotV2Script = func() string {
	script := strings.Replace(strings.Replace(snapshotScript,
		`index=$(mktemp)`, `test -z "$(git ls-files -u)" || { echo "Resolve and stage conflicts first" >&2; exit 42; }
test ! -f "$(git rev-parse --git-path MERGE_HEAD)" || { echo "Finish the merge commit first" >&2; exit 43; }
index=$(mktemp)`, 1),
		`head=$(printf 'Workspace reviewed snapshot\n' | git -c core.hooksPath=/dev/null commit-tree "$tree" -p "$1")`,
		`parent=$(git rev-parse HEAD)
if [ -n "${4:-}" ] && [ "$4" != "${3:-}" ]; then
 git merge-base --is-ancestor "$4" "$parent" || { echo "Fetch and merge the current PR branch before updating it" >&2; exit 44; }
fi
git merge-base --is-ancestor "$1" "$parent"
parents=(-p "$parent")
if [ -n "${3:-}" ]; then
 git cat-file -e "$3^{commit}"
 if [ "$(git rev-parse "$3^{tree}")" = "$tree" ] && git merge-base --is-ancestor "$parent" "$3"; then
  head=$3
 else
  if ! git merge-base --is-ancestor "$3" "$parent"; then parents+=(-p "$3"); fi
 fi
fi
if [ -z "${head:-}" ]; then head=$(printf 'Workspace reviewed snapshot\n' | git -c core.hooksPath=/dev/null commit-tree "$tree" "${parents[@]}"); fi`, 1)
	script = strings.Replace(script, "result={'base':base", "result={'base':original_base", 1)
	return strings.Replace(script, "diff=subprocess.check_output", `ref=subprocess.run(['git','rev-parse','--verify','refs/remotes/workspace/main'],capture_output=True,text=True)
original_base=base
if ref.returncode==0: base=subprocess.check_output(['git','merge-base',ref.stdout.strip(),head],text=True).strip()
diff=subprocess.check_output`, 1)
}()

// Descriptor-relative traversal prevents symlink and rename races inside a guest.
const imageFileScript = `import sys,os,json,base64,stat
root,path,mode=sys.argv[1:]
assert not os.path.isabs(path)
parts=path.split('/')
assert parts and all(p not in ('','.','..') for p in parts)
fd=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
try:
 for part in parts[:-1]:
  nxt=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
  os.close(fd);fd=nxt
 image=os.open(parts[-1],os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=fd)
 try:
  st=os.fstat(image)
  assert stat.S_ISREG(st.st_mode) and st.st_size<=8*1024*1024
  with os.fdopen(image,'rb',closefd=False) as f:raw=f.read(8*1024*1024+1)
  assert len(raw)<=8*1024*1024
  print(json.dumps({'bytes':base64.b64encode(raw).decode()}))
 finally:os.close(image)
finally:os.close(fd)
`
