package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The workspace's instruction and memory files, read and written by a
// person from the chat (parity item 13): the project instructions at the
// workspace root, the rules under .claude/rules, and the CLI's auto-memory
// directory under the agent home. Only those locations are reachable:
// a path is validated here and again, descriptor-relative, in the guest.

// MemoryFile is one such file, with its contents as text.
type MemoryFile struct {
	// Scope is "workspace" (a path under the workspace root) or "auto"
	// (a path under the CLI's auto-memory directory).
	Scope string `json:"scope"`
	// Path is the file's path relative to its scope's root.
	Path string `json:"path"`
	Size int64  `json:"size"`
	Text string `json:"text"`
	// Truncated says Text is the file's first MemoryTextCap bytes only.
	Truncated bool `json:"truncated,omitempty"`
}

// MemoryListing is what a memory-list answers.
type MemoryListing struct {
	// Root is the workspace directory the "workspace" paths are under.
	Root string `json:"root"`
	// AutoDir is the auto-memory directory the "auto" paths are under:
	// what the CLI reported for this workspace, or the directory the CLI
	// derives for it. Exists says the directory is there.
	AutoDir string       `json:"autoDir"`
	Exists  bool         `json:"autoDirExists"`
	Files   []MemoryFile `json:"files"`
}

// The bounds: one file's text in a listing, one file written, and a whole
// listing.
const (
	MemoryTextCap    = 256 << 10
	MaxMemoryFile    = 1 << 20
	memoryListingCap = 4 << 20
)

// MemoryScopes are the two roots a memory path is relative to.
const (
	MemoryScopeWorkspace = "workspace"
	MemoryScopeAuto      = "auto"
)

// workspaceMemoryFiles are the fixed instruction files at the workspace
// root the CLI (and Codex) read; rules under .claude/rules are the other
// workspace paths.
var workspaceMemoryFiles = []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", ".claude/CLAUDE.md"}

var memorySegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// memoryProject is the shape of a project directory name under
// ~/.claude/projects (memoryProjectName's output: a leading dash for an
// absolute path).
var memoryProject = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// ValidateMemoryPath reports whether path is a memory file the scope
// allows: a fixed workspace file, a markdown file under .claude/rules
// (three levels at most), or a markdown file under the auto-memory
// directory (two levels at most). Segments never start with a dot, so no
// segment is "." or ".." and no hidden file is reachable.
func ValidateMemoryPath(scope, path string) error {
	if len(path) > 512 || strings.ContainsAny(path, "\x00\n\r\\") || strings.HasPrefix(path, "/") {
		return errors.New("invalid memory path")
	}
	segments := strings.Split(path, "/")
	switch scope {
	case MemoryScopeWorkspace:
		for _, fixed := range workspaceMemoryFiles {
			if path == fixed {
				return nil
			}
		}
		if len(segments) < 3 || len(segments) > 5 || segments[0] != ".claude" || segments[1] != "rules" {
			return errors.New("a workspace memory file is CLAUDE.md, CLAUDE.local.md, AGENTS.md, .claude/CLAUDE.md or a .md file under .claude/rules")
		}
		segments = segments[2:]
	case MemoryScopeAuto:
		if len(segments) > 3 {
			return errors.New("an auto-memory file is a .md file at most two levels under the memory directory")
		}
	default:
		return errors.New("memory scope must be workspace or auto")
	}
	for _, s := range segments {
		if !memorySegment.MatchString(s) {
			return errors.New("invalid memory path")
		}
	}
	if !strings.HasSuffix(segments[len(segments)-1], ".md") {
		return errors.New("memory files are markdown (.md)")
	}
	return nil
}

// memoryListScript lists the memory files with their contents as JSON.
// Arguments: the workspace root, the agent home, the auto-memory directory
// the CLI reported ("" when unknown; used only when it is a "memory"
// directory under the home's .claude/projects). Directories are opened
// descriptor-relative with O_NOFOLLOW and files with O_NOFOLLOW, as the
// other guest scripts do, so a symlink the agent planted leads nowhere;
// a file over the text cap is cut and marked; the whole listing is bounded.
const memoryListScript = `import sys,os,json,re,stat
root,home,reported=sys.argv[1:4]
CAP=256*1024;TOTAL=4*1024*1024
FIXED=['CLAUDE.md','CLAUDE.local.md','AGENTS.md','.claude/CLAUDE.md']
SEG=re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$')
PROJECT=re.compile(r'^[A-Za-z0-9_-]{1,255}$')
out={'files':[],'total':0}
def opendir(fd,name):
 return os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
def walk(fd,parts):
 try:
  for p in parts:
   nxt=opendir(fd,p);os.close(fd);fd=nxt
 except OSError:
  os.close(fd);raise
 return fd
def readfile(dfd,name):
 f=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=dfd)
 try:
  st=os.fstat(f)
  if not stat.S_ISREG(st.st_mode): return None
  with os.fdopen(f,'rb',closefd=False) as h: raw=h.read(CAP+1)
  cut=len(raw)>CAP
  text=raw[:CAP].decode('utf-8','replace')
  return st.st_size,text,cut
 finally: os.close(f)
def add(scope,path,dfd,name):
 if out['total']>TOTAL: return
 try: r=readfile(dfd,name)
 except OSError: return
 if r is None: return
 size,text,cut=r
 out['total']+=len(text)
 out['files'].append({'scope':scope,'path':path,'size':size,'text':text,'truncated':cut})
def rules(scope,dfd,prefix,depth,limit):
 try: names=sorted(os.listdir(dfd))
 except OSError: return
 for n in names:
  if not SEG.match(n): continue
  try: st=os.stat(n,dir_fd=dfd,follow_symlinks=False)
  except OSError: continue
  if stat.S_ISDIR(st.st_mode):
   if depth<limit:
    try: sub=opendir(dfd,n)
    except OSError: continue
    try: rules(scope,sub,prefix+n+'/',depth+1,limit)
    finally: os.close(sub)
  elif n.endswith('.md'): add(scope,prefix+n,dfd,n)
rfd=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
try:
 for fixed in FIXED:
  parts=fixed.split('/')
  try:
   d=walk(os.dup(rfd),parts[:-1])
  except OSError: continue
  try: add('workspace',fixed,d,parts[-1])
  finally: os.close(d)
 try:
  d=walk(os.dup(rfd),['.claude','rules'])
  try: rules('workspace',d,'.claude/rules/',1,3)
  finally: os.close(d)
 except OSError: pass
finally: os.close(rfd)
# The auto-memory directory: the CLI names it, or derives it from the
# workspace path the way the CLI does (every character outside
# [a-zA-Z0-9_-] becomes a dash).
projects=os.path.join(home,'.claude','projects')
auto=os.path.join(projects,re.sub(r'[^a-zA-Z0-9_-]','-',root),'memory')
if reported:
 rp=os.path.normpath(reported)
 rel=os.path.relpath(rp,projects)
 segs=rel.split(os.sep)
 if not rel.startswith('..') and len(segs)==2 and segs[1]=='memory' and PROJECT.match(segs[0]): auto=rp
out['autoDir']=auto
out['autoDirExists']=False
try:
 hfd=os.open(home,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
 try:
  d=walk(os.dup(hfd),['.claude','projects',os.path.basename(os.path.dirname(auto)),'memory'])
  out['autoDirExists']=True
  try: rules('auto',d,'',1,2)
  finally: os.close(d)
 finally: os.close(hfd)
except OSError: pass
del out['total']
print(json.dumps(out))
`

// memoryWriteScript moves a staged file (copied into the agent home) to
// its place: the scope root is opened O_NOFOLLOW, intermediate directories
// are opened descriptor-relative (created, for a rule's folder or the
// auto-memory directory, when missing), and the target is opened
// O_NOFOLLOW|O_CREAT|O_TRUNC relative to its directory, so nothing follows
// a symlink the agent planted. Arguments: the scope root, the relative
// path, the staged file. The copy arrives owned by the host's uid (the
// runtime copies as root), so it is staged world-readable and put in the
// agent's home, where the agent user can remove it; /tmp is sticky.
const memoryWriteScript = `import sys,os,stat
root,path,staged=sys.argv[1:4]
parts=path.split('/')
assert parts and all((p and p not in ('.','..') and not p.startswith('.')) or p=='.claude' for p in parts)
data=open(staged,'rb').read()
try: os.unlink(staged)
except OSError: pass
fd=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
try:
 for p in parts[:-1]:
  try: nxt=os.open(p,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
  except FileNotFoundError:
   os.mkdir(p,0o755,dir_fd=fd)
   nxt=os.open(p,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
  os.close(fd);fd=nxt
 f=os.open(parts[-1],os.O_WRONLY|os.O_CREAT|os.O_TRUNC|os.O_NOFOLLOW,0o644,dir_fd=fd)
 try:
  assert stat.S_ISREG(os.fstat(f).st_mode)
  os.write(f,data)
 finally: os.close(f)
finally: os.close(fd)
`

// memoryRoots are the scope roots for a sandbox: the workspace, and the
// auto-memory directory's parent tree under the agent home. A write under
// "auto" is placed relative to the home so the memory directory itself can
// be created when the CLI has not yet; the path is prefixed accordingly.
func (w *Worker) memoryRoots(s *managedSandbox, reported string) (workspace, home, autoDir string) {
	paths := s.paths.orDefaults()
	workspace, home = s.Directory, paths.Home
	autoDir = filepath.Join(home, ".claude", "projects", memoryProjectName(workspace), "memory")
	if reported != "" {
		if rel, err := filepath.Rel(filepath.Join(home, ".claude", "projects"), filepath.Clean(reported)); err == nil {
			segs := strings.Split(rel, "/")
			if len(segs) == 2 && segs[1] == "memory" && memoryProject.MatchString(segs[0]) {
				autoDir = filepath.Clean(reported)
			}
		}
	}
	return workspace, home, autoDir
}

var memoryProjectChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// memoryProjectName is the directory name the CLI gives a workspace under
// ~/.claude/projects: every character outside [a-zA-Z0-9_-] a dash.
func memoryProjectName(workspace string) string {
	return memoryProjectChars.ReplaceAllString(workspace, "-")
}

// listMemory answers a "memory-list": the files with their contents, from
// one guest round-trip. r.Path is the auto-memory directory the chat's CLI
// reported, when known.
func (w *Worker) listMemory(ctx context.Context, r Request) (Response, error) {
	if len(r.Path) > 1024 || strings.ContainsAny(r.Path, "\x00\n\r") {
		return Response{}, errors.New("invalid memory directory")
	}
	name, dir, err := w.guestCommand(ctx, r, "reading memory files")
	if err != nil {
		return Response{}, err
	}
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	var home string
	if s != nil {
		_, home, _ = w.memoryRoots(s, r.Path)
	}
	w.mu.Unlock()
	if home == "" {
		return Response{}, errors.New("unknown sandbox")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := w.Runtime.Exec(ctx, name, dir, "python3", "-c", memoryListScript, dir, home, r.Path)
	if err != nil {
		return Response{}, errors.New("could not read the workspace's memory files")
	}
	if len(raw) > memoryListingCap+memoryListingCap/2 {
		return Response{}, errors.New("memory listing too large")
	}
	var listing MemoryListing
	if err = json.Unmarshal([]byte(raw), &listing); err != nil {
		return Response{}, errors.New("invalid sandbox memory response")
	}
	listing.Root = dir
	if listing.Files == nil {
		listing.Files = []MemoryFile{}
	}
	// The guest names the files; keep only paths this side would accept,
	// and keep every text valid UTF-8 for the JSON the chat serves.
	kept := listing.Files[:0]
	for _, f := range listing.Files {
		if ValidateMemoryPath(f.Scope, f.Path) != nil {
			continue
		}
		f.Text = strings.ToValidUTF8(f.Text, "�")
		kept = append(kept, f)
	}
	listing.Files = kept
	return Response{Memory: &listing}, nil
}

// writeMemory answers a "memory-write": r.Bytes become the file at
// r.Directory under r.Scope's root, staged on the worker host, copied into
// the guest's /tmp by the runtime and placed by memoryWriteScript. An
// empty file is allowed (a person may clear CLAUDE.md); the content must
// be text.
func (w *Worker) writeMemory(ctx context.Context, r Request) (Response, error) {
	if err := ValidateMemoryPath(r.Scope, r.Directory); err != nil {
		return Response{}, err
	}
	if len(r.Bytes) > MaxMemoryFile || !utf8.Valid(r.Bytes) || strings.ContainsRune(string(r.Bytes), 0) {
		return Response{}, errors.New("a memory file is text of at most 1 MiB")
	}
	name, dir, err := w.guestCommand(ctx, r, "editing memory files")
	if err != nil {
		return Response{}, err
	}
	w.mu.Lock()
	s := w.managed.Sandboxes[r.SandboxID]
	var root, path, home string
	if s != nil {
		var workspace, autoDir string
		workspace, home, autoDir = w.memoryRoots(s, r.Path)
		root, path = workspace, r.Directory
		if r.Scope == MemoryScopeAuto {
			// Relative to the home, so the projects tree and the memory
			// directory are created when missing.
			root = home
			path = strings.TrimPrefix(strings.TrimPrefix(autoDir, home), "/") + "/" + r.Directory
		}
	}
	w.mu.Unlock()
	if root == "" {
		return Response{}, errors.New("unknown sandbox")
	}
	base := "warden-memory-" + randomID()
	stage := filepath.Join(w.Root, "attachments")
	if err = os.MkdirAll(stage, 0700); err != nil {
		return Response{}, err
	}
	staged := filepath.Join(stage, base)
	// World-readable: the guest copy keeps the mode but not the owner (the
	// staging directory itself is the worker's, 0700).
	if err = os.WriteFile(staged, r.Bytes, 0644); err != nil {
		return Response{}, err
	}
	defer os.Remove(staged)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	guest := home + "/." + base
	if err = w.Runtime.Copy(ctx, name, staged, guest); err != nil {
		return Response{}, errors.New("could not copy the file into the sandbox")
	}
	if _, err = w.Runtime.Exec(ctx, name, dir, "python3", "-c", memoryWriteScript, root, path, guest); err != nil {
		return Response{}, fmt.Errorf("could not write %s in the sandbox", r.Directory)
	}
	return Response{Directory: r.Directory}, nil
}
