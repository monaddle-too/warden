package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The jailbreak (docs/host-dogfood-plan.md, Part B): a workspace the
// owner opted into host access gives its agent runner-mediated tools that
// act on this machine as the runner's user. The runner answers the host.*
// operations only when it was started with --jailbreak (the config's
// dogfood.jailbreak, or `warden start --jailbreak`); otherwise the whole
// family is refused, whatever the chat service asks, so a bug there cannot
// reach the host. The chat service checks the workspace's own opt-in
// before it asks.
//
//   host.exec    Command, Directory (cwd), Timeout → Exec
//   host.put     Directory (sandbox path) → Path (host path): Size
//   host.get     Path (host path) → Directory (sandbox path): Size
//   host.expose  Port (a host loopback port) → Attachment (a publication
//                whose upstream is the host, served by the same loopback
//                listener a sandbox preview gets)
//   host.status  → Host (this machine's Warden instances)

// HostExecDefaultTimeout and HostExecMaxTimeout bound one host command;
// MaxHostCommand its length; HostExecOutputLimit the tail of its output
// the transcript keeps (the same as a sandbox exec); HostFileLimit what
// one host.put or host.get may move.
const (
	HostExecDefaultTimeout = 10 * time.Minute
	HostExecMaxTimeout     = time.Hour
	MaxHostCommand         = 16384
	HostExecOutputLimit    = ExecOutputLimit
	HostFileLimit          = 512 << 20
)

// HostStatus is what host.status answers: this machine, the runner's home
// directory and state directory, and the Warden instances found there
// (~/.warden and every ~/.warden-<name>).
type HostStatus struct {
	OS        string         `json:"os"`
	Arch      string         `json:"arch"`
	Home      string         `json:"home"`
	StateDir  string         `json:"stateDir"`
	Instances []HostInstance `json:"instances"`
}

// HostInstance is one Warden state directory on the host: its name
// ("default" for ~/.warden, else the suffix after ~/.warden-), the
// directory, the release it runs from (the releases/<version> the release
// link points at, "" when there is none), whether it is the instance this
// runner belongs to, and whether its launcher lock is held (a start in
// progress or running; a stale lock reads the same).
type HostInstance struct {
	Name     string `json:"name"`
	StateDir string `json:"stateDir"`
	Release  string `json:"release,omitempty"`
	This     bool   `json:"this,omitempty"`
	Running  bool   `json:"running,omitempty"`
}

// hostExecs tracks the host commands in flight, by chat, so a run's
// cancellation and the chat's Stop kill them (the request's own context
// covers the connection going away).
type hostExecs struct {
	mu    sync.Mutex
	byRun map[string]map[*hostExec]struct{}
}

type hostExec struct {
	cancel context.CancelFunc
}

func (w *Worker) hosts() *hostExecs {
	w.hostOnce.Do(func() { w.hostExecs = &hostExecs{byRun: map[string]map[*hostExec]struct{}{}} })
	return w.hostExecs
}

func (h *hostExecs) add(key string, e *hostExec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byRun[key] == nil {
		h.byRun[key] = map[*hostExec]struct{}{}
	}
	h.byRun[key][e] = struct{}{}
}

func (h *hostExecs) remove(key string, e *hostExec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.byRun[key], e)
	if len(h.byRun[key]) == 0 {
		delete(h.byRun, key)
	}
}

// cancelHostExecs kills every host command of the chat (any run), as Stop
// and a run's cancellation must; it reports how many it found.
func (w *Worker) cancelHostExecs(chatID string) int {
	h := w.hosts()
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for key, execs := range h.byRun {
		if key != chatID && !strings.HasPrefix(key, chatID+":") {
			continue
		}
		for e := range execs {
			e.cancel()
			n++
		}
	}
	return n
}

// hostOp answers one host.* request. Every op needs the jailbreak flag and
// a registered chat (the binding proves the chat service speaks for the
// chat); host.exec and the copies need no running sandbox for the host
// side, but the copies do for the guest side.
func (w *Worker) hostOp(ctx context.Context, r Request) (Response, error) {
	if !w.Jailbreak {
		return Response{}, errors.New("host access is off on this runner (warden.json dogfood.jailbreak, or warden start --jailbreak)")
	}
	if w.HostHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Response{}, errors.New("the runner has no home directory")
		}
		w.HostHome = home
	}
	switch r.Operation {
	case "host.exec":
		return w.hostExecOp(ctx, r)
	case "host.put", "host.get":
		return w.hostCopy(ctx, r)
	case "host.expose":
		w.mu.Lock()
		defer w.mu.Unlock()
		w.defaultsLocked()
		return w.exposeHostLocked(ctx, r)
	case "host.status":
		if _, err := w.hostBinding(r); err != nil {
			return Response{}, err
		}
		return Response{Host: w.hostStatus()}, nil
	}
	return Response{}, errors.New("unsupported host operation")
}

// hostBinding resolves the request's registered chat under the lock and
// returns its sandbox (which may be stopped).
func (w *Worker) hostBinding(r Request) (*managedSandbox, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaultsLocked()
	s, _, err := w.bindingLocked(r)
	if err != nil {
		return nil, err
	}
	s.LastActivity = w.now()
	_ = w.saveManagedLocked()
	return s, nil
}

// hostShell is the login shell host commands run through: $SHELL when it
// is an executable, else /bin/sh. The -l makes it read the owner's
// profile, so Homebrew's tools resolve as they do in a terminal (the
// runner under launchd has a bare environment).
func hostShell() string {
	if shell := os.Getenv("SHELL"); filepath.IsAbs(shell) {
		if info, err := os.Stat(shell); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return shell
		}
	}
	return "/bin/sh"
}

// hostExecOp runs r.Command through the login shell in r.Directory (the
// home directory when empty), in its own process group, for at most
// r.Timeout seconds (HostExecDefaultTimeout when 0, capped at
// HostExecMaxTimeout), stdout and stderr merged and tailed to
// HostExecOutputLimit. The context (the request's connection closing, the
// chat's cancel) or the timeout kills the group.
func (w *Worker) hostExecOp(ctx context.Context, r Request) (Response, error) {
	cmdline := r.Command
	if strings.TrimSpace(cmdline) == "" || len(cmdline) > MaxHostCommand || strings.ContainsRune(cmdline, 0) {
		return Response{}, errors.New("invalid command")
	}
	dir := r.Directory
	if dir == "" {
		dir = w.HostHome
	}
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return Response{}, errors.New("cwd must be an absolute path on the host")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return Response{}, errors.New("cwd is not a directory on the host: " + dir)
	}
	timeout := time.Duration(r.Timeout) * time.Second
	if timeout <= 0 {
		timeout = HostExecDefaultTimeout
	}
	if timeout > HostExecMaxTimeout {
		timeout = HostExecMaxTimeout
	}
	if _, err := w.hostBinding(r); err != nil {
		return Response{}, err
	}
	result, err := runHostCommand(ctx, w.hosts(), runKey(r), hostShell(), cmdline, dir, timeout, HostExecOutputLimit)
	if err != nil {
		return Response{}, err
	}
	return Response{Exec: &result}, nil
}

// runHostCommand is the command itself: shell -lc cmdline in dir, its own
// session so the kill reaches what it started, a bounded tail of the
// merged output, stopped reading shortly after the exit (a background
// process it left holding the pipe does not keep the caller waiting).
func runHostCommand(ctx context.Context, tracker *hostExecs, key, shell, cmdline, dir string, timeout time.Duration, cap int) (ExecResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e := &hostExec{cancel: cancel}
	tracker.add(key, e)
	defer tracker.remove(key, e)
	pr, pw, err := os.Pipe()
	if err != nil {
		return ExecResult{}, err
	}
	cmd := exec.Command(shell, "-lc", cmdline)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The runner's own environment plus what a login shell derives; the
	// chat service's capability never reaches here (it is the chat's).
	cmd.Env = os.Environ()
	started := time.Now()
	if err = cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return ExecResult{}, fmt.Errorf("could not start the host shell: %w", err)
	}
	pw.Close()
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	tail := &tailBuffer{cap: cap}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 65536)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				tail.write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	timedOut, cancelled := false, false
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-timer.C:
		timedOut = true
		kill()
		waitErr = <-waitDone
	case <-ctx.Done():
		cancelled = true
		kill()
		waitErr = <-waitDone
	}
	// What the command printed is in the pipe already; a process it left
	// behind holding the write end is not waited for beyond a moment.
	select {
	case <-readDone:
	case <-time.After(300 * time.Millisecond):
	}
	pr.Close()
	<-readDone
	code := 0
	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			code = exit.ExitCode()
			if code < 0 {
				code = -1
			}
		} else {
			code = -1
		}
	}
	if timedOut || cancelled {
		code = -1
	}
	if cancelled {
		return ExecResult{}, errors.New("the command was cancelled")
	}
	text, bytes := tail.text()
	return ExecResult{Output: text, ExitCode: code, TimedOut: timedOut, Bytes: bytes, DurationMS: time.Since(started).Milliseconds()}, nil
}

// tailBuffer keeps the last cap*2 bytes of what is written to it and
// counts the total, so an endless printer costs bounded memory; text
// returns the last cap characters with a note when something was cut.
type tailBuffer struct {
	mu    sync.Mutex
	cap   int
	buf   []byte
	total int64
	cut   bool
}

func (t *tailBuffer) write(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += int64(len(p))
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap*4 {
		t.cut = true
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.cap*2:]...)
	}
}

func (t *tailBuffer) text() (string, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := strings.ToValidUTF8(string(t.buf), "�")
	if len(text) > t.cap || t.cut {
		if len(text) > t.cap {
			text = text[len(text)-t.cap:]
		}
		text = "[output cut: only the last " + strconv.Itoa(t.cap) + " characters are kept]\n" + text
	}
	return text, t.total
}

// hostPath validates a host path for a copy: absolute, under the owner's
// home and not under this Warden's own state directory (its provider
// sign-ins, sockets and registries stay the owner's; decision 4 of the
// plan).
func (w *Worker) hostPath(p string) (string, error) {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", errors.New("the host path must be absolute")
	}
	home := filepath.Clean(w.HostHome)
	if p != home && !strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "", errors.New("the host path must be under the home directory " + home)
	}
	state := filepath.Dir(w.Root)
	if p == state || strings.HasPrefix(p, state+string(filepath.Separator)) {
		return "", errors.New("Warden's own state directory " + state + " is not reachable")
	}
	return p, nil
}

// hostCopyGuestPath validates a sandbox path for a copy: absolute, no traversal.
func hostCopyGuestPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || len(p) > 4096 || strings.ContainsAny(p, "\x00\n") {
		return "", errors.New("the sandbox path must be absolute")
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == "/" {
		return "", errors.New("the sandbox root cannot be copied")
	}
	return clean, nil
}

// hostTreeSize is the size of a host file or directory (regular files,
// symlinks not followed), refused above HostFileLimit.
func hostTreeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		if total > HostFileLimit {
			return fmt.Errorf("more than %d MiB", HostFileLimit>>20)
		}
		return nil
	})
	return total, err
}

// guestSizeScript prints the byte size of a guest path (a file or a tree,
// symlinks not followed) or "missing".
const guestSizeScript = `import os,sys
p=sys.argv[1]
if not os.path.lexists(p): print("missing"); sys.exit(0)
if os.path.islink(p) or not os.path.isdir(p): print(os.lstat(p).st_size); sys.exit(0)
t=0
for root,dirs,files in os.walk(p):
 for f in files:
  fp=os.path.join(root,f)
  try:
   st=os.lstat(fp)
  except OSError: continue
  if not os.path.islink(fp): t+=st.st_size
print(t)
`

// hostCopy answers host.put (sandbox → host: Directory to Path) and
// host.get (host → sandbox: Path to Directory) through the runtime's copy,
// the size checked first on the source side.
func (w *Worker) hostCopy(ctx context.Context, r Request) (Response, error) {
	host, err := w.hostPath(r.Path)
	if err != nil {
		return Response{}, err
	}
	guest, err := hostCopyGuestPath(r.Directory)
	if err != nil {
		return Response{}, err
	}
	name, dir, err := w.guestCommand(ctx, r, "copying files")
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if r.Operation == "host.get" {
		size, err := hostTreeSize(host)
		if err != nil {
			if os.IsNotExist(err) {
				return Response{}, errors.New("no such file or directory on the host: " + host)
			}
			return Response{}, err
		}
		parent := filepath.ToSlash(filepath.Dir(guest))
		if _, err := w.Runtime.Exec(ctx, name, dir, "sudo", "mkdir", "-p", parent); err != nil {
			return Response{}, errors.New("could not create the sandbox directory " + parent)
		}
		if err := w.Runtime.Copy(ctx, name, host, guest); err != nil {
			return Response{}, fmt.Errorf("copy into the sandbox failed: %w", err)
		}
		if strings.HasPrefix(guest, "/home/agent/") {
			_, _ = w.Runtime.Exec(ctx, name, dir, "sudo", "chown", "-R", "agent:agent", guest)
		}
		return Response{Directory: guest, Output: host, Size: size}, nil
	}
	raw, err := w.Runtime.Exec(ctx, name, dir, "python3", "-c", guestSizeScript, guest)
	if err != nil {
		return Response{}, errors.New("could not read the sandbox path")
	}
	raw = strings.TrimSpace(raw)
	if raw == "missing" {
		return Response{}, errors.New("no such file or directory in the sandbox: " + guest)
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return Response{}, errors.New("invalid sandbox size answer")
	}
	if size > HostFileLimit {
		return Response{}, fmt.Errorf("more than %d MiB", HostFileLimit>>20)
	}
	if info, err := os.Stat(filepath.Dir(host)); err != nil || !info.IsDir() {
		return Response{}, errors.New("the host directory " + filepath.Dir(host) + " does not exist")
	}
	if err := w.Runtime.CopyOut(ctx, name, guest, host); err != nil {
		return Response{}, fmt.Errorf("copy to the host failed: %w", err)
	}
	return Response{Directory: guest, Output: host, Size: size}, nil
}

// hostStatus lists the Warden instances on this machine.
func (w *Worker) hostStatus() *HostStatus {
	state := filepath.Dir(w.Root)
	status := &HostStatus{OS: runtime.GOOS, Arch: runtime.GOARCH, Home: w.HostHome, StateDir: state, Instances: []HostInstance{}}
	entries, _ := os.ReadDir(w.HostHome)
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name != ".warden" && !strings.HasPrefix(name, ".warden-") {
			continue
		}
		dir := filepath.Join(w.HostHome, name)
		if _, err := os.Stat(filepath.Join(dir, "warden.json")); err != nil {
			continue
		}
		inst := HostInstance{Name: strings.TrimPrefix(strings.TrimPrefix(name, ".warden-"), ".warden"), StateDir: dir, This: dir == state}
		if inst.Name == "" {
			inst.Name = "default"
		}
		if target, err := os.Readlink(filepath.Join(dir, "release")); err == nil {
			inst.Release = filepath.Base(target)
		}
		if _, err := os.Stat(filepath.Join(dir, "launcher.lock")); err == nil {
			inst.Running = lockHeld(filepath.Join(dir, "launcher.lock"))
		}
		status.Instances = append(status.Instances, inst)
	}
	sort.Slice(status.Instances, func(i, j int) bool { return status.Instances[i].StateDir < status.Instances[j].StateDir })
	return status
}

// lockHeld reports whether another process holds the launcher's lock
// file (a running Warden), by trying to take it without blocking.
func lockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// hostPubKey keys a host publication apart from the sandbox's own ports.
func hostPubKey(sandbox string, port int) string { return sandbox + ":host:" + strconv.Itoa(port) }

// exposeHostLocked answers host.expose: a publication whose upstream is
// the host's loopback port r.Port, served by the runner's loopback
// availability listener exactly like a sandbox preview (the chat service
// admits the attachment URL because the runner minted it), and an
// attachment of the chat for it. The sandbox must be running with its
// agent streaming, as for preview.attach, since the binding is the
// agent's; nothing is published in the guest.
func (w *Worker) exposeHostLocked(ctx context.Context, r Request) (Response, error) {
	s, _, err := w.runLocked(r)
	if err != nil {
		return Response{}, err
	}
	if !s.Active.Streaming {
		return Response{}, errors.New("host exposure requires an active agent stream")
	}
	if r.Path == "" {
		r.Path = "/"
	}
	if err = validatePreview(r); err != nil {
		return Response{}, err
	}
	if err = w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
		w.failEnforcementLocked(s)
		return Response{}, err
	}
	s.previewAuditAt = w.now()
	key := hostPubKey(s.ID, r.Port)
	p := w.managed.Publications[key]
	if p == nil {
		if len(w.managed.Publications) >= 256 {
			return Response{}, errors.New("retained preview publication capacity reached")
		}
		p = &publication{ID: randomID(), SandboxID: s.ID, Port: r.Port, Address: loopbackAddress, HostPort: r.Port, Upstream: UpstreamHost, State: "publishing"}
		w.managed.Publications[key] = p
	}
	if err = w.ensureProxyLocked(p); err != nil {
		return Response{}, err
	}
	p.Generation = s.Generation
	p.State = "available"
	var a *PreviewAttachment
	for _, existing := range w.managed.Attachments {
		if existing.ChatID == r.ChatID && existing.Port == r.Port && existing.Path == r.Path && existing.Upstream == UpstreamHost && existing.State != "removed" {
			a = existing
			break
		}
	}
	if a == nil {
		a = &PreviewAttachment{ID: randomID(), ChatID: r.ChatID, SandboxID: r.SandboxID, Port: r.Port, Path: r.Path, Upstream: UpstreamHost}
		w.managed.Attachments[a.ID] = a
	}
	a.Title = r.Title
	a.State = "available"
	a.URL = w.attachmentURL(p) + a.Path
	s.LastActivity = w.now()
	if err = w.saveManagedLocked(); err != nil {
		p.State = "error"
		a.State = "error"
		a.URL = ""
		return Response{}, err
	}
	copy := *a
	return Response{Attachment: &copy}, nil
}

// UpstreamHost marks a publication or attachment whose origin is the
// host's own loopback port rather than a sandbox port.
const UpstreamHost = "host"

// HostAccessPrompt is what a jailbroken workspace's agent is told beside
// its tools (chats/host.go adds it to the developer instructions).
const HostAccessPrompt = "This workspace has host access: the host_run, host_put, host_get, host_expose and host_status tools act on the owner's own computer, outside the sandbox, as the owner. Use them only for what the owner asked that needs the host (building and running Warden there, driving a second Warden instance, reading its logs); everything else stays in the sandbox. Each host call is shown to the owner and subject to their permission rules."
