package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// RepositorySource runs only in the trusted worker. Public repository reads do
// not use GitHub credentials, host Git configuration, or agent-supplied URLs.
type RepositorySource interface {
	Bundle(context.Context, string, string, string) (string, error)
}

type publicGitHubSource struct{}

func publicRepositoryURL(selected string) (string, error) {
	if selected == "" {
		return "", nil
	}
	repo, ok := strings.CutPrefix(selected, "github://")
	if !ok || !repository.MatchString(repo) || strings.Contains(repo, "..") || strings.HasSuffix(repo, ".git") || len(repo) > 256 {
		return "", errors.New("choose a public GitHub repository using github://owner/repository")
	}
	return "https://github.com/" + repo + ".git", nil
}

func publicGitEnvironment() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/usr/bin/false"}
}

func (publicGitHubSource) Bundle(ctx context.Context, selected, pinned, destination string) (string, error) {
	url, err := publicRepositoryURL(selected)
	if err != nil || url == "" {
		return "", errors.New("a public GitHub repository is required")
	}
	if pinned != "" && !commit.MatchString(pinned) {
		return "", errors.New("invalid repository base commit")
	}
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".fetch-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	gitDir := filepath.Join(dir, "objects.git")
	git := func(args ...string) (string, error) {
		args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "init.templateDir=", "-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "protocol.ext.allow=never", "--git-dir=" + gitDir}, args...)
		cmd := command(ctx, "git", args...)
		cmd.Env = publicGitEnvironment()
		out, e := cmd.Output()
		if e != nil {
			return "", errors.New("could not read the public GitHub repository; check its name and worker connectivity")
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err = git("init", "--bare", gitDir); err != nil {
		return "", err
	}
	ref := pinned
	if ref == "" {
		ref = "HEAD"
	}
	if _, err = git("fetch", "--no-tags", url, ref+":refs/heads/panta-base"); err != nil {
		return "", err
	}
	base, err := git("rev-parse", "refs/heads/panta-base")
	if err != nil || !commit.MatchString(base) {
		return "", errors.New("public repository returned an invalid base commit")
	}
	if pinned != "" && base != pinned {
		return "", errors.New("public repository base changed during retry")
	}
	if _, err = git("bundle", "create", destination, "refs/heads/panta-base"); err != nil {
		return "", err
	}
	info, err := os.Stat(destination)
	if err != nil {
		return "", err
	}
	if info.Size() > 256<<20 {
		return "", errors.New("public repository bundle exceeds the 256 MiB transfer limit")
	}
	return base, nil
}

func (w *Worker) bindRepositoryLocked(s *managedSandbox, selected string) error {
	if s.Repository == selected {
		return nil
	}
	if s.Repository != "" {
		return errors.New("this environment is bound to a different repository; choose a new environment")
	}
	if s.Active != nil {
		return ErrBusy
	}
	if s.Base != "" {
		return errors.New("choose a new environment before attaching a repository to a legacy checkout")
	}
	s.Repository = selected
	return nil
}

func (w *Worker) prepareRepositoryLocked(ctx context.Context, s *managedSandbox) error {
	if s.Repository == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	url, err := publicRepositoryURL(s.Repository)
	if err != nil {
		return err
	}
	if s.RepositoryReady {
		// Check availability without fetching, switching branches, or resetting
		// files. Agent edits and commits survive both new runs and worker restarts.
		_, err = w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "python3", "-c", repositoryResumeScript, s.Directory, s.Base)
		return err
	}
	root := filepath.Join(w.Root, "repository-bundles", s.ID)
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	bundle := filepath.Join(root, "checkout.bundle")
	if s.Base == "" {
		source := w.RepositorySource
		if source == nil {
			source = publicGitHubSource{}
		}
		base, e := source.Bundle(ctx, s.Repository, "", bundle+".tmp")
		if e != nil {
			return e
		}
		if !commit.MatchString(base) {
			return errors.New("repository source returned an invalid commit")
		}
		if err = os.Rename(bundle+".tmp", bundle); err != nil {
			return err
		}
		s.Base = base
	}
	if s.RepositoryCheckout == "" {
		s.RepositoryCheckout = path.Join(s.Directory, "checkout-"+randomID())
	}
	// Pin the fetched base before the guest can receive it. Retrying after a
	// lost response uses the same bundle and checkout identity.
	if err = w.saveManagedLocked(); err != nil {
		return err
	}
	info, err := os.Stat(bundle)
	if err != nil {
		return fmt.Errorf("pending repository bundle is unavailable: %w", err)
	}
	if info.Size() > 256<<20 {
		return errors.New("public repository bundle exceeds the 256 MiB transfer limit")
	}
	target := "/tmp/panta-repository-" + randomID() + ".bundle"
	if err = w.Runtime.Copy(ctx, s.RuntimeName, bundle, target); err != nil {
		return err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = w.Runtime.Exec(cleanup, s.RuntimeName, "/tmp", "sudo", "rm", "-f", "--", target)
	}()
	if _, err = w.Runtime.Exec(ctx, s.RuntimeName, "/tmp", "sudo", "chown", "agent:agent", target); err != nil {
		return err
	}
	result, err := w.Runtime.Exec(ctx, s.RuntimeName, s.Directory, "python3", "-c", repositoryImportScript, target, s.RepositoryCheckout, url, s.Base)
	if err != nil {
		return fmt.Errorf("could not import repository without replacing existing files: %w", err)
	}
	if strings.TrimSpace(result) != s.Base {
		return errors.New("sandbox did not confirm the pinned repository base")
	}
	s.Directory = s.RepositoryCheckout
	s.RepositoryReady = true
	if err = w.saveManagedLocked(); err != nil {
		return err
	}
	// Only the imported transfer is removed; the guest checkout is retained.
	_ = os.Remove(bundle)
	return nil
}

const repositoryResumeScript = `import os,subprocess,sys
directory,base=sys.argv[1:]
assert os.path.isdir(os.path.join(directory,'.git')), 'Prepared repository is missing; restore it or choose a new environment'
env={'PATH':'/usr/local/bin:/usr/bin:/bin','HOME':'/nonexistent','GIT_CONFIG_GLOBAL':'/dev/null','GIT_CONFIG_NOSYSTEM':'1','GIT_TERMINAL_PROMPT':'0'}
subprocess.run(['git','-c','core.hooksPath=/dev/null','-C',directory,'cat-file','-e',base+'^{commit}'],env=env,check=True)
`

const repositoryImportScript = `import ctypes,errno,json,os,shutil,subprocess,sys,tempfile
bundle,destination,url,base=sys.argv[1:]
env={'PATH':'/usr/local/bin:/usr/bin:/bin','HOME':'/nonexistent','GIT_CONFIG_GLOBAL':'/dev/null','GIT_CONFIG_NOSYSTEM':'1','GIT_TERMINAL_PROMPT':'0'}
def git(directory,*args):
 return subprocess.check_output(['git','-c','core.hooksPath=/dev/null','-c','init.templateDir=','-c','credential.helper=','-C',directory,*args],env=env,stderr=subprocess.DEVNULL,text=True).strip()
def matches():
 if os.path.islink(destination): return False
 try:
  with open(os.path.join(destination,'.git','panta-import.json')) as f: record=json.load(f)
  return record=={'url':url,'base':base} and git(destination,'cat-file','-t',base)=='commit'
 except (OSError,ValueError,subprocess.SubprocessError): return False
if os.path.lexists(destination):
 assert matches(), 'Checkout destination already exists; its files were preserved'
 print(base); sys.exit(0)
stage=tempfile.mkdtemp(prefix='.panta-import-',dir=os.path.dirname(destination))
try:
 git(stage,'clone','--no-checkout','--no-hardlinks',bundle,stage)
 assert git(stage,'rev-parse','refs/remotes/origin/panta-base')==base, 'Transferred base does not match authorized checkout'
 git(stage,'config','remote.origin.url',url)
 git(stage,'config','core.hooksPath','/dev/null')
 git(stage,'config','user.name','Panta')
 git(stage,'config','user.email','panta@localhost')
 git(stage,'checkout','-b','panta-work',base)
 with open(os.path.join(stage,'.git','panta-import.json'),'x') as f: json.dump({'url':url,'base':base},f)
 # Linux renameat2 preserves a directory created concurrently by another process.
 libc=ctypes.CDLL(None,use_errno=True)
 rename=libc.renameat2
 rename.argtypes=[ctypes.c_int,ctypes.c_char_p,ctypes.c_int,ctypes.c_char_p,ctypes.c_uint]
 rename.restype=ctypes.c_int
 if rename(-100,os.fsencode(stage),-100,os.fsencode(destination),1)!=0:
  err=ctypes.get_errno()
  if err!=errno.EEXIST or not matches(): raise OSError(err,os.strerror(err))
 print(base)
finally:
 if os.path.exists(stage): shutil.rmtree(stage)
`
