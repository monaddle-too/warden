package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
	"warden/chat/internal/hoststats"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{1,70}$`)
var repository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var commit = regexp.MustCompile(`^[a-f0-9]{40}$`)

const MaxParallelSessions = 2

// Worker is the protocol 2 sandbox worker: every guest operation goes through
// Runtime (RuntimeDriver) and every network grant through Gate (Enforcement).
type Worker struct {
	ordinarySlots chan struct{}
	controlSlots  chan struct{}
	execSlots     chan struct{} // a person's own commands (exec.go)
	Revision      string
	Parallel      int
	Retained      int
	// Root is the private worker state directory. Executable and Template
	// are the pinned SBX executable and guest template; only the SBX runtime
	// driver reads them.
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
	RuntimeDir                    string
	ClaudePath                    string
	mu                            sync.Mutex
	managed                       *managedState
	usages                        map[string]*usageState // guest resource samples by sandbox ID (guarded by mu)
	controls                      *controlState
	progress                      *progressState // startup stage by sandbox ID (its own lock; progress.go)
	progressOnce                  sync.Once
	snapshots                     *snapshotState // registry snapshot for reads while mu is busy (snapshot.go)
	snapshotOnce                  sync.Once
	// PrepareTimeout bounds one prepare operation: sandbox creation, the
	// boot and the guest provisioning. Two minutes when unset (the sbx
	// shapes); the Kubernetes runner allows ten, since a node may have to
	// join first.
	PrepareTimeout time.Duration
	// Cluster is the driver's view of the cluster the sandboxes run in
	// (Kubernetes); nil on the sbx shapes, where the cluster operations
	// report unavailable.
	Cluster          ClusterInspector
	Gate             Enforcement
	Runtime          RuntimeDriver
	RepositorySource RepositorySource
	IdleTimeout      time.Duration
	MaxResident      int
	MemoryMB         int
	// Limits is the size offer: default, ceiling, CPU step and whether a
	// resize restarts. A zero value derives from MemoryMB and one CPU.
	Limits ResourceLimits
	Now    func() time.Time
	// PreviewListener and PreviewAddress switch the preview proxy to one
	// shared server (docs/warden-kubernetes-plan.md, decisions 5 and 10;
	// config services.runner.previews): Serve runs it on the listener,
	// which the runner service bound with mutual TLS admitting only the
	// chat, every published preview is served under /<publication ID>,
	// and the attachment URL is PreviewAddress (tls://<host>:<port>, which
	// the chat dials as https://) followed by that path. Both unset (the
	// sbx shapes, where the chat shares the host) keeps one loopback
	// listener per publication and http://127.0.0.1:<port>/ URLs.
	PreviewListener net.Listener
	PreviewAddress  string
}

func (w *Worker) parallelLimit() int {
	if w.Parallel > 0 {
		return w.Parallel
	}
	return MaxParallelSessions
}

func NewWorker(root, executable, template string) *Worker {
	return &Worker{metrics: &hoststats.Collector{Root: root}, ordinarySlots: make(chan struct{}, 4), controlSlots: make(chan struct{}, 16), execSlots: make(chan struct{}, 4), controls: &controlState{bindings: map[string]Request{}, cancel: map[string]context.CancelFunc{}, cancelled: map[string]bool{}}, Root: root, Executable: executable, Template: template}
}
func (w *Worker) Serve(ctx context.Context, l net.Listener) error {
	// A worker restart must not leave detached app servers in old guests.
	// Guest contents persist; the backend reports interrupted runs and can
	// resume them.
	if err := w.initializeManaged(ctx); err != nil {
		return err
	}
	var previews *http.Server
	if w.PreviewListener != nil {
		// Started only once the registry is loaded, so a request racing the
		// start finds the publications rather than an empty worker.
		previews = w.previewServer()
		go func(server *http.Server) { _ = server.Serve(w.PreviewListener) }(previews)
	}
	go func() {
		<-ctx.Done()
		l.Close()
		if previews != nil {
			previews.Close()
		}
	}()
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
			w.handle(ctx, c)
		}()
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
