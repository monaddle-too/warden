package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// MaxPathQuery bounds the text the composer asks to complete.
const MaxPathQuery = 1024

// pathsScript completes a partial workspace path for the composer's
// @-mentions, shell style: the segment being typed is matched as a
// case-insensitive prefix among the entries of the directory the earlier
// segments name, directories first and marked with a trailing slash. A
// query with no directory part also searches the tree for names starting
// with it (bounded: a few thousand entries, six levels, dependency and
// build directories skipped), so `@Conv` finds a component two folders
// down. Directories are opened descriptor-relative with O_NOFOLLOW, as
// imageFileScript does, so a symlink the agent planted cannot lead the
// listing outside the workspace; names with control characters are left
// out, and the answer is capped, so a workspace full of files costs one
// bounded walk and a short reply.
const pathsScript = `import sys,os,json
root,query=sys.argv[1:]
LIMIT=50;BUDGET=4000;DEPTH=6
SKIP={'.git','node_modules','__pycache__','target','dist','build','vendor','.venv','venv','.cache','.warden'}
assert len(query)<=1024 and not os.path.isabs(query)
head,_,stem=query.rpartition('/')
parts=head.split('/') if head else []
assert all(p not in ('','.','..') for p in parts)
stem_l=stem.lower()
hidden=stem.startswith('.')
def shown(name): return name.isprintable() and (hidden or not name.startswith('.'))
def listing(fd,prefix):
 dirs=[];files=[]
 with os.scandir(fd) as it:
  for e in it:
   if not shown(e.name) or not e.name.lower().startswith(stem_l): continue
   (dirs if e.is_dir(follow_symlinks=False) else files).append(e.name)
 key=str.lower
 return [prefix+n+'/' for n in sorted(dirs,key=key)]+[prefix+n for n in sorted(files,key=key)]
seen=[0]
def search(fd,prefix,depth,out):
 if depth>DEPTH or len(out)>=LIMIT: return
 subdirs=[]
 with os.scandir(fd) as it:
  for e in it:
   seen[0]+=1
   if seen[0]>BUDGET: return
   if not e.name.isprintable() or e.name.startswith('.') or e.name in SKIP: continue
   isdir=e.is_dir(follow_symlinks=False)
   if prefix and e.name.lower().startswith(stem_l):
    out.append(prefix+e.name+('/' if isdir else ''))
    if len(out)>=LIMIT: return
   if isdir: subdirs.append(e.name)
 for name in sorted(subdirs,key=str.lower):
  try: sub=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
  except OSError: continue
  try: search(sub,prefix+name+'/',depth+1,out)
  finally: os.close(sub)
  if seen[0]>BUDGET or len(out)>=LIMIT: return
fd=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
try:
 for part in parts:
  nxt=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
  os.close(fd);fd=nxt
 paths=listing(fd,head+'/' if head else '')
 if not head and stem and len(paths)<LIMIT:
  deeper=[]
  search(fd,'',1,deeper)
  paths+=sorted(deeper,key=str.lower)
finally: os.close(fd)
print(json.dumps({'paths':paths[:LIMIT]}))
`

// completePathsLocked answers a "paths" request: the listing that
// pathsScript makes for the query in r.Directory. The sandbox must be
// running, since only the guest can read its own workspace.
func (w *Worker) completePathsLocked(ctx context.Context, s *managedSandbox, r Request) (Response, error) {
	query := r.Directory
	if len(query) > MaxPathQuery || strings.ContainsAny(query, "\x00\n") {
		return Response{}, errors.New("invalid path query")
	}
	raw, err := w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "python3", "-c", pathsScript, s.Directory, query)
	if err != nil {
		// A directory the query names may not exist; that is an empty
		// completion, not a failure worth showing.
		return Response{Paths: []string{}}, nil
	}
	var result Response
	if err = json.Unmarshal([]byte(raw), &result); err != nil {
		return Response{}, errors.New("invalid sandbox paths response")
	}
	if result.Paths == nil {
		result.Paths = []string{}
	}
	return result, nil
}
