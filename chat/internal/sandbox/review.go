package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxReviewBundle = 64 << 20
const maxReviewDiff = 2 << 20

type ReviewFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary"`
}
type RepositoryReview struct {
	ID           string       `json:"id"`
	ProjectID    string       `json:"projectID"`
	ChatID       string       `json:"chatID"`
	SandboxID    string       `json:"sandboxID"`
	Repository   string       `json:"repository"`
	Base         string       `json:"base"`
	Head         string       `json:"head"`
	BundleSHA256 string       `json:"bundleSHA256"`
	Diff         string       `json:"diff"`
	Files        []ReviewFile `json:"files"`
	CreatedAt    string       `json:"createdAt"`
}

func (w *Worker) reviewRoot(chat string) string { return filepath.Join(w.Root, "chat-reviews", chat) }
func (w *Worker) readReviewLocked(s *managedSandbox, r Request, id string) (*RepositoryReview, error) {
	if !validIdentity(id) {
		return nil, errors.New("invalid review identity")
	}
	raw, err := os.ReadFile(filepath.Join(w.reviewRoot(r.ChatID), id, "review.json"))
	if err != nil {
		return nil, err
	}
	var review RepositoryReview
	if json.Unmarshal(raw, &review) != nil || review.ID != id || review.ProjectID != r.ProjectID || review.ChatID != r.ChatID || review.SandboxID != s.ID || review.Repository != s.Repository || review.Base != s.Base {
		return nil, errors.New("stored review does not match this conversation environment")
	}
	return &review, nil
}
func (w *Worker) latestReviewLocked(s *managedSandbox, r Request) (Response, error) {
	raw, err := os.ReadFile(filepath.Join(w.reviewRoot(r.ChatID), "latest.json"))
	if os.IsNotExist(err) {
		return Response{}, nil
	}
	if err != nil {
		return Response{}, err
	}
	var id string
	if json.Unmarshal(raw, &id) != nil {
		return Response{}, errors.New("invalid review pointer")
	}
	review, err := w.readReviewLocked(s, r, id)
	return Response{Review: review}, err
}

// Retrying a persisted capture repairs a lost latest-pointer write, but an old
// request cannot replace a more recent review after the checkout has changed.
func (w *Worker) advanceLatestReviewLocked(s *managedSandbox, r Request, review *RepositoryReview) error {
	latest, err := w.latestReviewLocked(s, r)
	if err != nil {
		return err
	}
	if latest.Review != nil {
		previousTime, err := time.Parse(time.RFC3339Nano, latest.Review.CreatedAt)
		if err != nil {
			return err
		}
		currentTime, err := time.Parse(time.RFC3339Nano, review.CreatedAt)
		if err != nil {
			return err
		}
		if !currentTime.After(previousTime) {
			return nil
		}
	}
	return atomicJSON(filepath.Join(w.reviewRoot(r.ChatID), "latest.json"), review.ID)
}
func (w *Worker) captureReviewLocked(ctx context.Context, s *managedSandbox, r Request) (Response, error) {
	if !validIdentity(r.CallID) {
		return Response{}, errors.New("a stable review request ID is required")
	}
	if r.Repository != s.Repository || s.Repository == "" || !s.RepositoryReady || !commit.MatchString(s.Base) {
		return Response{}, errors.New("prepare this project's public repository before reviewing changes")
	}
	if previous, err := w.readReviewLocked(s, r, r.CallID); err == nil {
		return Response{Review: previous}, w.advanceLatestReviewLocked(s, r, previous)
	} else if !os.IsNotExist(err) {
		return Response{}, err
	}
	if s.Active != nil || s.Reviewing {
		return Response{}, ErrBusy
	}
	if s.State != "running" {
		return Response{}, errors.New("resume the environment before capturing a new review; saved reviews remain available")
	}
	if err := w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s.Reviewing = true
	snapshot := *s
	var stage string
	defer func() {
		if stage != "" {
			_ = os.RemoveAll(stage)
		}
	}()
	// Export and verification must not block other environments' heartbeats,
	// cancellation or status. Preparing this environment waits for the capture.
	review, err := func() (RepositoryReview, error) {
		w.mu.Unlock()
		defer w.mu.Lock()
		raw, err := w.Runtime.Exec(ctx, snapshot.RuntimeName, snapshot.Directory, "python3", "-c", reviewExportScript, snapshot.Base)
		if err != nil {
			return RepositoryReview{}, fmt.Errorf("could not snapshot repository changes; finish any merge and retry: %w", err)
		}
		var exported struct {
			Bundle []byte `json:"bundle"`
		}
		if len(raw) > 90<<20 || json.Unmarshal([]byte(raw), &exported) != nil || len(exported.Bundle) == 0 || len(exported.Bundle) > maxReviewBundle {
			return RepositoryReview{}, errors.New("invalid review transfer or bundle exceeds 64 MiB")
		}
		root := w.reviewRoot(r.ChatID)
		if err = os.MkdirAll(root, 0700); err != nil {
			return RepositoryReview{}, err
		}
		stage, err = os.MkdirTemp(root, ".snapshot-")
		if err != nil {
			return RepositoryReview{}, err
		}
		bundle := filepath.Join(stage, "snapshot.bundle")
		if err = os.WriteFile(bundle, exported.Bundle, 0600); err != nil {
			return RepositoryReview{}, err
		}
		baseBundle, err := w.reviewBaseBundle(ctx, snapshot)
		if err != nil {
			return RepositoryReview{}, err
		}
		review, err := verifyReviewBundle(ctx, stage, bundle, snapshot.Base, baseBundle)
		if err != nil {
			return RepositoryReview{}, err
		}
		digest := sha256.Sum256(exported.Bundle)
		review.BundleSHA256 = hex.EncodeToString(digest[:])
		return review, nil
	}()
	s.Reviewing = false
	if err != nil {
		return Response{}, err
	}
	if s.State != "running" || s.Generation != snapshot.Generation || s.Base != snapshot.Base || s.Repository != snapshot.Repository {
		return Response{}, errors.New("environment changed while capturing review; retry after resuming")
	}
	if err = w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
		return Response{}, err
	}
	review.ID, review.ProjectID, review.ChatID, review.SandboxID = r.CallID, r.ProjectID, r.ChatID, s.ID
	review.Repository = s.Repository
	createdAt := w.now().UTC()
	latest, err := w.latestReviewLocked(s, r)
	if err != nil {
		return Response{}, err
	}
	if latest.Review != nil {
		previousTime, err := time.Parse(time.RFC3339Nano, latest.Review.CreatedAt)
		if err != nil {
			return Response{}, err
		}
		if !createdAt.After(previousTime) {
			createdAt = previousTime.Add(time.Nanosecond)
		}
	}
	review.CreatedAt = createdAt.Format(time.RFC3339Nano)
	if err = atomicJSON(filepath.Join(stage, "review.json"), review); err != nil {
		return Response{}, err
	}
	if err = os.Rename(stage, filepath.Join(w.reviewRoot(r.ChatID), r.CallID)); err != nil {
		return Response{}, err
	}
	if err = w.advanceLatestReviewLocked(s, r, &review); err != nil {
		return Response{}, err
	}
	s.LastActivity = w.now()
	if err = w.saveManagedLocked(); err != nil {
		return Response{}, err
	}
	return Response{Review: &review}, nil
}

// Cache the credential-free public base independently of agent output. Older
// preparations discard their import bundle, so recover the exact pinned commit
// on the first capture. Later captures need transfer only changed Git objects.
func (w *Worker) reviewBaseBundle(ctx context.Context, s managedSandbox) (string, error) {
	root := filepath.Join(w.Root, "repository-bundles", s.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	bundle := filepath.Join(root, "review-base.bundle")
	if info, err := os.Stat(bundle); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 256<<20 {
			return "", errors.New("invalid cached public review base")
		}
		return bundle, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	source := w.RepositorySource
	if source == nil {
		source = publicGitHubSource{}
	}
	stage := bundle + "." + randomID()
	defer os.Remove(stage)
	base, err := source.Bundle(ctx, s.Repository, s.Base, stage)
	if err != nil {
		return "", err
	}
	if base != s.Base {
		return "", errors.New("public review base does not match the prepared commit")
	}
	if err = os.Rename(stage, bundle); err != nil {
		return "", err
	}
	return bundle, nil
}

// Parse the untrusted transfer only in a private bare repository. No checkout,
// hooks, credential helpers, repository scripts or network remotes are used.
// The starting commit was recorded by trusted public repository preparation.
func verifyReviewBundle(ctx context.Context, root, bundle, base, baseBundle string, inspect ...func(string) error) (RepositoryReview, error) {
	review := RepositoryReview{Base: base, Files: []ReviewFile{}}
	if !commit.MatchString(base) {
		return review, errors.New("invalid trusted review base")
	}
	gitDir := filepath.Join(root, "verify.git")
	defer os.RemoveAll(gitDir)
	git := func(args ...string) ([]byte, error) {
		args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "init.templateDir=", "-c", "credential.helper=", "-c", "core.quotePath=true", "-c", "protocol.ext.allow=never", "-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true", "--git-dir=" + gitDir}, args...)
		cmd := command(ctx, "git", args...)
		cmd.Env = publicGitEnvironment()
		var out bytes.Buffer
		cmd.Stdout = &limitedWriter{W: &out, N: maxReviewDiff}
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			return nil, errors.New("could not verify repository snapshot or diff exceeds 2 MiB")
		}
		return out.Bytes(), nil
	}
	if _, err := git("init", "--bare", gitDir); err != nil {
		return review, err
	}
	if _, err := git("bundle", "verify", baseBundle); err != nil {
		return review, err
	}
	if _, err := git("fetch", "--no-tags", baseBundle, base+":refs/panta/base"); err != nil {
		return review, err
	}
	actualBase, err := git("rev-parse", "refs/panta/base")
	if err != nil || strings.TrimSpace(string(actualBase)) != base {
		return review, errors.New("cached public bundle does not contain the trusted starting commit")
	}
	heads, err := git("bundle", "list-heads", bundle)
	if err != nil {
		return review, err
	}
	fields := strings.Fields(string(heads))
	if len(fields) != 2 || !commit.MatchString(fields[0]) || fields[1] != "refs/panta/review" {
		return review, errors.New("review bundle must contain exactly one snapshot ref")
	}
	review.Head = fields[0]
	if _, err = git("bundle", "verify", bundle); err != nil {
		return review, err
	}
	if _, err = git("fetch", "--no-tags", bundle, "refs/panta/review:refs/panta/review"); err != nil {
		return review, err
	}
	parents, err := git("rev-list", "--parents", "-n", "1", review.Head)
	if err != nil {
		return review, err
	}
	if strings.TrimSpace(string(parents)) != review.Head+" "+base {
		return review, errors.New("review snapshot is not based on the trusted starting commit")
	}
	diff, err := git("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--full-index", base, review.Head, "--")
	if err != nil {
		return review, err
	}
	if !utf8.Valid(diff) {
		return review, errors.New("review diff contains unsupported non-UTF-8 text")
	}
	review.Diff = string(diff)
	stats, err := git("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--numstat", "-z", base, review.Head, "--")
	if err != nil {
		return review, err
	}
	for _, line := range bytes.Split(stats, []byte{0}) {
		if len(line) == 0 {
			continue
		}
		parts := bytes.SplitN(line, []byte{'\t'}, 3)
		if len(parts) != 3 || !utf8.Valid(parts[2]) || len(review.Files) >= 2000 {
			return review, errors.New("unsupported review file list")
		}
		file := ReviewFile{Path: string(parts[2]), Binary: string(parts[0]) == "-"}
		if !file.Binary {
			file.Added, err = strconv.Atoi(string(parts[0]))
			if err != nil {
				return review, err
			}
			file.Removed, err = strconv.Atoi(string(parts[1]))
			if err != nil {
				return review, err
			}
		}
		review.Files = append(review.Files, file)
	}
	for _, fn := range inspect {
		if err := fn(gitDir); err != nil {
			return review, err
		}
	}
	return review, nil
}

// The alternate index includes staged ignored files, ordinary untracked files,
// tracked edits and deletions. Original index, HEAD and branches remain intact.
// Only the private temporary ref and temporary files are removed afterwards.
const reviewExportScript = `import base64,json,os,shutil,subprocess,sys,tempfile,uuid
base=sys.argv[1]
env={'PATH':'/usr/local/bin:/usr/bin:/bin','HOME':'/nonexistent','GIT_CONFIG_NOSYSTEM':'1','GIT_CONFIG_GLOBAL':'/dev/null','GIT_TERMINAL_PROMPT':'0','GIT_AUTHOR_NAME':'Panta','GIT_AUTHOR_EMAIL':'panta@localhost','GIT_COMMITTER_NAME':'Panta','GIT_COMMITTER_EMAIL':'panta@localhost','GIT_AUTHOR_DATE':'2001-01-01T00:00:00Z','GIT_COMMITTER_DATE':'2001-01-01T00:00:00Z'}
def git(*args): return subprocess.check_output(['git','-c','core.hooksPath=/dev/null',*args],env=env,stderr=subprocess.DEVNULL)
assert not git('ls-files','-u'), 'Resolve conflicts before capturing review'
for state in ['MERGE_HEAD','CHERRY_PICK_HEAD','REVERT_HEAD','rebase-merge','rebase-apply']:
 assert not os.path.exists(git('rev-parse','--git-path',state).decode().strip()), 'Finish the in-progress Git operation before review'
original_index=git('rev-parse','--git-path','index').decode().strip()
ref='refs/panta/capture-'+uuid.uuid4().hex
with tempfile.TemporaryDirectory(prefix='panta-review-') as tmp:
 env['GIT_INDEX_FILE']=os.path.join(tmp,'index')
 if os.path.exists(original_index): shutil.copyfile(original_index,env['GIT_INDEX_FILE'])
 else: git('read-tree','HEAD')
 git('add','-A','--','.')
 tree=git('write-tree').decode().strip()
 head=git('commit-tree',tree,'-p',base,'-m','Panta reviewed snapshot').decode().strip()
 # Create a temporary bare repository to give the bundle one fixed ref name
 # without replacing any ref in the agent's repository.
 bare=os.path.join(tmp,'bundle.git')
 git('init','--bare',bare)
 git('update-ref',ref,head)
 try:
  source=os.path.abspath(git('rev-parse','--git-dir').decode().strip())
  git('--git-dir='+bare,'fetch','--no-tags',source,ref+':refs/panta/review')
  bundle=os.path.join(tmp,'snapshot.bundle')
  git('--git-dir='+bare,'bundle','create',bundle,'refs/panta/review','^'+base)
  assert os.path.getsize(bundle)<=64*1024*1024, 'Review bundle exceeds 64 MiB'
  with open(bundle,'rb') as f: print(json.dumps({'bundle':base64.b64encode(f.read()).decode()}))
 finally: git('update-ref','-d',ref,head)
`
