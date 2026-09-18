package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Checkpoint is the runner's record of one workspace snapshot: the state
// of a sandbox's workspace before the user turn ID names (the message's
// ID, which is the checkpoint's). The snapshot is a git tree of the
// workspace — tracked and untracked files, ignored ones and .warden/ left
// out — held by a root commit under refs/warden/checkpoints/<ID> in the
// workspace's own repository (Store "repository") or, for a workspace
// that is not one, in a private git directory beside it (Store
// "private", checkpointPrivateDir). Changed is false when the tree
// equalled the previous checkpoint's, in which case the ref names that
// checkpoint's commit and no new objects were written.
type Checkpoint struct {
	ID      string    `json:"id"`
	ChatID  string    `json:"chatID"`
	Commit  string    `json:"commit"`
	Tree    string    `json:"tree"`
	Store   string    `json:"store"`
	Changed bool      `json:"changed"`
	At      time.Time `json:"at"`
}

// WorkspaceChanges is the workspace now against a checkpoint: one unified
// diff (git's, per file) and the counts per file. Truncated marks a diff
// cut at maxWorkspaceDiff; the counts are still complete.
type WorkspaceChanges struct {
	Base      string       `json:"base"`
	Files     []ReviewFile `json:"files"`
	Diff      string       `json:"diff"`
	Truncated bool         `json:"truncated,omitempty"`
}

// WorkspaceRestore is what a restore changed in the workspace: the paths
// written back from the checkpoint and the ones removed because the
// checkpoint did not have them.
type WorkspaceRestore struct {
	Checkpoint Checkpoint `json:"checkpoint"`
	Restored   []string   `json:"restored"`
	Removed    []string   `json:"removed"`
}

// maxCheckpoints bounds the records kept per sandbox; the oldest go first,
// their refs with them.
const maxCheckpoints = 500

// maxWorkspaceDiff bounds the diff text one diff answer carries.
const maxWorkspaceDiff = 2 << 20

// checkpointPrivateDir is the git directory a non-repository workspace's
// checkpoints live in: beside the workspace, never inside it, so the
// agent's listing of its workspace does not show it and a snapshot does
// not contain itself.
const checkpointPrivateDir = "/home/agent/.warden-checkpoints.git"

var checkpointID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// checkpointCommon is what the three scripts share: a fixed git
// environment (no hooks, no user configuration, fixed identity and dates
// so equal trees make equal commits) and the choice of store. A
// workspace whose root holds .git is a repository and keeps its
// checkpoints in it; anything else uses the private directory, created
// bare with core.bare off so the workspace serves as its work tree,
// with the well-known dependency and build directories excluded (as the
// paths completion skips them). snapshot() writes the workspace as it
// is into a temporary index (a copy of the agent's, so unchanged files
// cost a stat) and answers the tree; nothing of the agent's changes.
const checkpointCommon = `import json,os,shutil,stat,subprocess,sys,tempfile
root=sys.argv[1]
env={'PATH':'/usr/local/bin:/usr/bin:/bin','HOME':'/nonexistent','GIT_CONFIG_NOSYSTEM':'1','GIT_CONFIG_GLOBAL':'/dev/null','GIT_TERMINAL_PROMPT':'0','GIT_OPTIONAL_LOCKS':'0','GIT_AUTHOR_NAME':'Warden','GIT_AUTHOR_EMAIL':'warden@localhost','GIT_COMMITTER_NAME':'Warden','GIT_COMMITTER_EMAIL':'warden@localhost','GIT_AUTHOR_DATE':'2001-01-01T00:00:00Z','GIT_COMMITTER_DATE':'2001-01-01T00:00:00Z'}
def git(*args,**kw): return subprocess.check_output(['git','-c','core.hooksPath=/dev/null','-c','core.quotePath=false',*args],env=env,stderr=subprocess.DEVNULL,cwd=root,**kw)
EXCLUDE=['.warden/','node_modules/','__pycache__/','target/','dist/','build/','vendor/','.venv/','venv/','.cache/']
CAP=256*1024*1024
if os.path.exists(os.path.join(root,'.git')):
 store='repository'
 gitdir=git('rev-parse','--absolute-git-dir').decode().strip()
 assert os.path.realpath(git('rev-parse','--show-toplevel').decode().strip())==os.path.realpath(root),'workspace is inside another repository'
else:
 store='private'
 gitdir=os.path.join(os.path.dirname(root.rstrip('/')),'.warden-checkpoints.git')
 if not os.path.isdir(gitdir):
  subprocess.check_output(['git','init','-q','--bare',gitdir],env=env)
  subprocess.check_output(['git','--git-dir='+gitdir,'config','core.bare','false'],env=env)
 env['GIT_DIR']=gitdir;env['GIT_WORK_TREE']=root
 os.makedirs(os.path.join(gitdir,'info'),exist_ok=True)
 with open(os.path.join(gitdir,'info','exclude'),'w') as f: f.write('\n'.join(EXCLUDE)+'\n')
SKIP={e.rstrip('/') for e in EXCLUDE}|{'.git'}
def small():
 total=0
 for base,dirs,files in os.walk(root):
  dirs[:]=[d for d in dirs if d not in SKIP and not os.path.islink(os.path.join(base,d))]
  for f in files:
   try: st=os.lstat(os.path.join(base,f))
   except OSError: continue
   if stat.S_ISREG(st.st_mode): total+=st.st_size
   if total>CAP: return False
 return True
tmp=tempfile.mkdtemp(prefix='warden-ckpt-')
def snapshot():
 if store=='private':
  assert small(),'workspace over 256 MiB; checkpoints are off for it'
  env['GIT_INDEX_FILE']=os.path.join(gitdir,'index')
 else:
  env['GIT_INDEX_FILE']=os.path.join(tmp,'index')
  original=os.path.join(root,git('rev-parse','--git-path','index').decode().strip())
  if os.path.exists(original): shutil.copyfile(original,env['GIT_INDEX_FILE'])
 git('add','-A','--',':/',':(top,exclude).warden')
 return git('write-tree').decode().strip()
`

// checkpointScript snapshots the workspace for checkpoint ID (argv[2]);
// argv[3] and argv[4] are the previous checkpoint's tree and commit ("-"
// for none: the SBX exec API refuses an empty argument): an equal tree
// reuses that commit. The ref is written last.
const checkpointScript = checkpointCommon + `cid,prev_tree,prev_commit=sys.argv[2:5]
try:
 tree=snapshot()
 changed=tree!=prev_tree or prev_commit=='-'
 commit=git('commit-tree',tree,'-m','Warden checkpoint '+cid).decode().strip() if changed else prev_commit
 git('update-ref','refs/warden/checkpoints/'+cid,commit)
 print(json.dumps({'store':store,'commit':commit,'tree':tree,'changed':changed}))
finally: shutil.rmtree(tmp,ignore_errors=True)
`

// restoreScript brings the workspace back to checkpoint argv[2] (its ID)
// whose commit must be argv[3]: the workspace as it is now is snapshotted
// too, and only the paths that differ are written back from the
// checkpoint's tree (through a second temporary index), the ones the
// checkpoint lacks removed. Files git ignores, and .warden/, are outside
// both trees and stay as they are; a nested repository (a gitlink) is
// skipped. Directories emptied by a removal are removed too.
const restoreScript = checkpointCommon + `cid,commit=sys.argv[2:4]
try:
 assert git('rev-parse','--verify','--quiet','refs/warden/checkpoints/'+cid+'^{commit}').decode().strip()==commit,'checkpoint was altered in the workspace'
 now=snapshot()
 target=git('rev-parse',commit+'^{tree}').decode().strip()
 raw=git('diff-tree','-r','--no-renames','-z','--raw',now,target)
 fields=raw.split(b'\0')
 restore=[];remove=[]
 i=0
 while i+1<len(fields):
  meta=fields[i].decode();path=fields[i+1].decode('utf-8','surrogateescape');i+=2
  parts=meta.split(' ')
  old_mode,new_mode,status=parts[0].lstrip(':'),parts[1],parts[4][0]
  if new_mode=='160000' or (status=='D' and old_mode=='160000'): continue
  (remove if status=='D' else restore).append(path)
 env['GIT_INDEX_FILE']=os.path.join(tmp,'restore-index')
 git('read-tree',target)
 for path in remove:
  full=os.path.join(root,path)
  try:
   if os.path.islink(full) or not os.path.isdir(full): os.remove(full)
   else: shutil.rmtree(full)
  except FileNotFoundError: pass
  parent=os.path.dirname(full)
  while parent and os.path.realpath(parent)!=os.path.realpath(root):
   try: os.rmdir(parent)
   except OSError: break
   parent=os.path.dirname(parent)
 if restore:
  for path in restore:
   full=os.path.join(root,path)
   if os.path.isdir(full) and not os.path.islink(full): shutil.rmtree(full)
  git('checkout-index','-f','-z','--stdin',input=b'\0'.join(p.encode('utf-8','surrogateescape') for p in restore)+b'\0')
 print(json.dumps({'restored':sorted(restore),'removed':sorted(remove)}))
finally: shutil.rmtree(tmp,ignore_errors=True)
`

// diffScript answers the workspace now against checkpoint argv[2] (its
// commit): the per-file counts and one unified diff, cut at the size in
// argv[3] on a file boundary.
const diffScript = checkpointCommon + `commit,limit=sys.argv[2],int(sys.argv[3])
try:
 base=git('rev-parse',commit+'^{tree}').decode().strip()
 now=snapshot()
 files=[]
 for line in git('diff-tree','-r','--no-renames','--numstat','-z',base,now).split(b'\0'):
  if not line: continue
  added,removed,path=line.split(b'\t',2)
  files.append({'path':path.decode('utf-8','replace'),'added':0 if added==b'-' else int(added),'removed':0 if removed==b'-' else int(removed),'binary':added==b'-'})
 diff=git('diff-tree','-r','-p','--no-renames','--no-ext-diff','--no-textconv','--no-color',base,now)
 truncated=False
 if len(diff)>limit:
  cut=diff.rfind(b'\ndiff --git ',0,limit)
  diff=diff[:cut+1] if cut>0 else b''
  truncated=True
 print(json.dumps({'files':files,'diff':diff.decode('utf-8','replace'),'truncated':truncated}))
finally: shutil.rmtree(tmp,ignore_errors=True)
`

// checkpointOp handles the four checkpoint operations. They exec into the
// guest, so the worker lock is released for the exec (a large workspace
// takes seconds to snapshot) and taken again to record the outcome, as
// the review capture does; a sandbox replaced meanwhile (a new
// generation) drops the record.
func (w *Worker) checkpointOp(ctx context.Context, s *managedSandbox, r Request) (Response, error) {
	if r.Operation == "checkpoints" {
		return Response{Checkpoints: w.checkpointsOf(s, r.ChatID)}, nil
	}
	if !checkpointID.MatchString(r.CallID) {
		return Response{}, errors.New("a checkpoint ID is required")
	}
	if s.State != "running" || !s.Created {
		return Response{}, errors.New("sandbox is stopped; start the workspace first")
	}
	if err := w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
		return Response{}, err
	}
	name, dir, generation := s.RuntimeName, s.Directory, s.Generation
	exec := func(script string, args ...string) (map[string]json.RawMessage, error) {
		w.mu.Unlock()
		defer w.mu.Lock()
		raw, err := w.Runtime.Exec(ctx, name, dir, append([]string{"python3", "-c", script, dir}, args...)...)
		if err != nil {
			return nil, err
		}
		var result map[string]json.RawMessage
		if len(raw) > 4<<20 || json.Unmarshal([]byte(raw), &result) != nil {
			return nil, errors.New("invalid sandbox checkpoint response")
		}
		return result, nil
	}
	switch r.Operation {
	case "checkpoint":
		previous := w.lastCheckpoint(s)
		result, err := exec(checkpointScript, r.CallID, orDash(previous.Tree), orDash(previous.Commit))
		if err != nil {
			return Response{}, errors.New("could not checkpoint the workspace: " + checkpointReason(err))
		}
		cp := Checkpoint{ID: r.CallID, ChatID: r.ChatID, At: w.now().UTC()}
		var changed bool
		if e1, e2, e3, e4 := json.Unmarshal(result["commit"], &cp.Commit), json.Unmarshal(result["tree"], &cp.Tree), json.Unmarshal(result["store"], &cp.Store), json.Unmarshal(result["changed"], &changed); e1 != nil || e2 != nil || e3 != nil || e4 != nil || !commit.MatchString(cp.Commit) || !commit.MatchString(cp.Tree) || (cp.Store != "repository" && cp.Store != "private") {
			return Response{}, errors.New("invalid sandbox checkpoint response")
		}
		cp.Changed = changed
		if s.Generation != generation {
			return Response{}, errors.New("workspace changed while checkpointing; retry")
		}
		w.recordCheckpoint(s, cp)
		if err := w.saveManagedLocked(); err != nil {
			return Response{}, err
		}
		return Response{Checkpoint: &cp}, nil
	case "restore":
		cp, ok := w.findCheckpoint(s, r.CallID)
		if !ok {
			return Response{}, errors.New("no checkpoint was recorded at this message")
		}
		result, err := exec(restoreScript, cp.ID, cp.Commit)
		if err != nil {
			return Response{}, errors.New("could not restore the checkpoint: " + checkpointReason(err))
		}
		restore := WorkspaceRestore{Checkpoint: cp, Restored: []string{}, Removed: []string{}}
		if e1, e2 := json.Unmarshal(result["restored"], &restore.Restored), json.Unmarshal(result["removed"], &restore.Removed); e1 != nil || e2 != nil {
			return Response{}, errors.New("invalid sandbox restore response")
		}
		s.LastActivity = w.now()
		return Response{Restore: &restore}, nil
	case "diff":
		cp, ok := w.findCheckpoint(s, r.CallID)
		if !ok {
			return Response{}, errors.New("no checkpoint was recorded at this message")
		}
		result, err := exec(diffScript, cp.Commit, "2097152")
		if err != nil {
			return Response{}, errors.New("could not diff the workspace: " + checkpointReason(err))
		}
		changes := WorkspaceChanges{Base: cp.ID, Files: []ReviewFile{}}
		if e1, e2, e3 := json.Unmarshal(result["files"], &changes.Files), json.Unmarshal(result["diff"], &changes.Diff), json.Unmarshal(result["truncated"], &changes.Truncated); e1 != nil || e2 != nil || e3 != nil || len(changes.Diff) > maxWorkspaceDiff+1024 {
			return Response{}, errors.New("invalid sandbox diff response")
		}
		return Response{Changes: &changes}, nil
	}
	return Response{}, errors.New("unsupported checkpoint operation")
}

// orDash is s, or "-" for none: an exec argument is never empty.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// checkpointReason keeps the guest's failure short: an assertion in the
// script names its reason in the exception, which the driver folds into
// the error; anything else is the exec failing.
func checkpointReason(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, "AssertionError: "); i >= 0 {
		return strings.TrimSpace(msg[i+len("AssertionError: "):])
	}
	return "the workspace's git command failed"
}

func (w *Worker) checkpointsOf(s *managedSandbox, chatID string) []Checkpoint {
	out := []Checkpoint{}
	for _, cp := range s.Checkpoints {
		if chatID == "" || cp.ChatID == chatID {
			out = append(out, cp)
		}
	}
	return out
}

func (w *Worker) findCheckpoint(s *managedSandbox, id string) (Checkpoint, bool) {
	for _, cp := range s.Checkpoints {
		if cp.ID == id {
			return cp, true
		}
	}
	return Checkpoint{}, false
}

// lastCheckpoint is the newest record, whose tree the next snapshot is
// compared with; a zero value when there is none or the store changed.
func (w *Worker) lastCheckpoint(s *managedSandbox) Checkpoint {
	if n := len(s.Checkpoints); n > 0 {
		return s.Checkpoints[n-1]
	}
	return Checkpoint{}
}

// recordCheckpoint appends the record (a repeated ID replaces its
// record) and drops the oldest past maxCheckpoints.
func (w *Worker) recordCheckpoint(s *managedSandbox, cp Checkpoint) {
	kept := s.Checkpoints[:0]
	for _, old := range s.Checkpoints {
		if old.ID != cp.ID {
			kept = append(kept, old)
		}
	}
	s.Checkpoints = append(kept, cp)
	if len(s.Checkpoints) > maxCheckpoints {
		s.Checkpoints = append([]Checkpoint(nil), s.Checkpoints[len(s.Checkpoints)-maxCheckpoints:]...)
	}
}
