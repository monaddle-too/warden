package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// A person's own shell command in a workspace (the composer's "!cmd") and
// their note for the workspace's CLAUDE.md (the composer's "#note"). Both
// are the person acting in the sandbox beside the agent: the runner runs
// them as the sandbox's agent user, in the workspace, passing the text as
// data to a guest script (never through a host shell), and answers with
// what happened. Neither takes the registry lock while the guest works, so
// a slow command blocks neither the agent's turn nor the workspace panel.

// MaxExecCommand bounds one command line; MaxMemoryNote one CLAUDE.md
// note; ExecTimeout is how long a command may run before its process
// group is killed; ExecOutputLimit is how much of the output (its tail)
// the transcript keeps.
const (
	MaxExecCommand  = 4096
	MaxMemoryNote   = 4096
	ExecTimeout     = 60 * time.Second
	ExecOutputLimit = 30000
)

// ExecResult is what a person's command came to: its merged stdout and
// stderr (the last ExecOutputLimit characters, decoded leniently), its
// exit code (-1 when it was killed) and whether the timeout killed it.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

// execScript runs one command line with bash in the workspace, stdin
// closed, stdout and stderr merged, in its own session (process group) so
// the timeout can kill everything it started. It keeps a bounded tail of
// the output while reading (a command that prints without end costs
// bounded memory and is cut off at the limit), stops reading once the
// command exited and its pipe fell quiet for a moment (a background
// process it left holding the pipe does not keep the person waiting), and
// reports the exit code, the tail and whether the timeout struck.
const execScript = `import sys,os,json,subprocess,signal,select,time
root,cmd,timeout,cap=sys.argv[1],sys.argv[2],float(sys.argv[3]),int(sys.argv[4])
p=subprocess.Popen(['bash','-c',cmd],cwd=root,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,start_new_session=True)
fd=p.stdout.fileno()
buf=bytearray();timed=False;cut=False
deadline=time.monotonic()+timeout
quiet=None
while True:
 now=time.monotonic()
 if now>=deadline: timed=True;break
 if p.poll() is not None:
  if quiet is None: quiet=now+0.3
  elif now>=quiet: break
 r,_,_=select.select([fd],[],[],min(deadline-now,0.2))
 if not r: continue
 chunk=os.read(fd,65536)
 if not chunk: break
 buf+=chunk
 if len(buf)>cap*4:
  cut=True
  buf=buf[-cap*2:]
if timed:
 try: os.killpg(p.pid,signal.SIGKILL)
 except OSError: pass
try: p.stdout.close()
except OSError: pass
try: code=p.wait(timeout=5)
except subprocess.TimeoutExpired:
 try: os.killpg(p.pid,signal.SIGKILL)
 except OSError: pass
 code=p.wait()
if code is None or code<0: code=-1
text=buf.decode('utf-8','replace')
if len(text)>cap or cut: text='[output cut: only the last '+str(cap)+' characters are kept]\n'+text[-cap:]
print(json.dumps({'output':text,'exitCode':code,'timedOut':timed}))
`

// memoryScript appends one note to CLAUDE.md at the workspace root as a
// bullet, creating the file when missing, without following a symlink in
// its place (the workspace is the agent's) and with a newline before the
// note when the file does not end in one.
const memoryScript = `import sys,os
root,note=sys.argv[1],sys.argv[2]
dfd=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
try:
 fd=os.open('CLAUDE.md',os.O_RDWR|os.O_APPEND|os.O_CREAT|os.O_NOFOLLOW,0o644,dir_fd=dfd)
finally: os.close(dfd)
try:
 size=os.fstat(fd).st_size
 lead=''
 if size>0:
  os.lseek(fd,size-1,os.SEEK_SET)
  if os.read(fd,1)!=b'\n': lead='\n'
 os.write(fd,(lead+note+'\n').encode())
finally: os.close(fd)
`

// MemoryBullet is the note as CLAUDE.md gets it: a bullet whose
// continuation lines are indented under it.
func MemoryBullet(note string) string {
	lines := strings.Split(strings.TrimSpace(note), "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		if i == 0 {
			lines[i] = "- " + l
		} else {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "\n")
}

// guestCommand resolves a running, admitted sandbox for a person's own
// operation under the registry lock and returns what the guest call needs,
// so the call itself runs with the lock released.
func (w *Worker) guestCommand(ctx context.Context, r Request, purpose string) (name, dir string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.defaultsLocked()
	s, _, err := w.bindingLocked(r)
	if err != nil {
		return "", "", err
	}
	if s.State != "running" {
		return "", "", errors.New("sandbox is stopped; resume the chat before " + purpose)
	}
	if err = w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
		return "", "", err
	}
	// The person is at work in the workspace: it is not idle.
	s.LastActivity = w.now()
	_ = w.saveManagedLocked()
	return s.RuntimeName, s.Directory, nil
}

// execCommand answers an "exec" request: r.Command run by execScript in
// the workspace, with a context that outlives the script's own timeout by
// enough for it to report.
func (w *Worker) execCommand(ctx context.Context, r Request) (Response, error) {
	cmd := r.Command
	if strings.TrimSpace(cmd) == "" || len(cmd) > MaxExecCommand || strings.ContainsRune(cmd, 0) {
		return Response{}, errors.New("invalid command")
	}
	name, dir, err := w.guestCommand(ctx, r, "running commands")
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ExecTimeout+20*time.Second)
	defer cancel()
	raw, err := w.Runtime.Exec(ctx, name, dir, "python3", "-c", execScript, dir, cmd, strconv.Itoa(int(ExecTimeout/time.Second)), strconv.Itoa(ExecOutputLimit))
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, errors.New("the command did not finish in time")
		}
		return Response{}, errors.New("could not run the command in the sandbox")
	}
	var result ExecResult
	if err = json.Unmarshal([]byte(raw), &result); err != nil {
		return Response{}, errors.New("invalid sandbox exec response")
	}
	return Response{Exec: &result}, nil
}

// appendMemory answers a "memory-append" request: r.Bytes, already shaped
// as a bullet by the chat service (MemoryBullet), appended to CLAUDE.md at
// the workspace root.
func (w *Worker) appendMemory(ctx context.Context, r Request) (Response, error) {
	note := string(r.Bytes)
	if strings.TrimSpace(note) == "" || len(note) > MaxMemoryNote || strings.ContainsRune(note, 0) {
		return Response{}, errors.New("invalid note")
	}
	name, dir, err := w.guestCommand(ctx, r, "adding notes")
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err = w.Runtime.Exec(ctx, name, dir, "python3", "-c", memoryScript, dir, note); err != nil {
		return Response{}, errors.New("could not write CLAUDE.md in the workspace")
	}
	return Response{Directory: "CLAUDE.md"}, nil
}
