package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Fetch only the repository and refs selected by the authenticated backend.
// Credentials never cross into the VM. Importing refs preserves checkout/index.
func (w *Worker) fetch(ctx context.Context, r Request, s session) (Response, error) {
	if !repository.MatchString(r.Repository) || strings.Contains(r.Repository, "..") || len(r.Args) < 1 || len(r.Args) > 2 {
		return Response{}, errors.New("invalid repository fetch")
	}
	for _, ref := range r.Args {
		if command(ctx, "git", "check-ref-format", "refs/heads/"+ref).Run() != nil {
			return Response{}, errors.New("invalid branch")
		}
	}
	root := filepath.Join(w.Root, "transfers")
	if err := os.MkdirAll(root, 0700); err != nil {
		return Response{}, err
	}
	dir, err := os.MkdirTemp(root, "fetch-")
	if err != nil {
		return Response{}, err
	}
	defer os.RemoveAll(dir)
	gitDir := filepath.Join(dir, "objects.git")
	git := func(args ...string) error {
		cmd := command(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "--git-dir=" + gitDir}, args...)...)
		cmd.Env = gitEnvironment(r.Token)
		if cmd.Run() != nil {
			return errors.New("could not fetch repository refs; check GitHub access")
		}
		return nil
	}
	if err = git("init", "--bare", gitDir); err != nil {
		return Response{}, err
	}
	args := []string{"fetch", "--no-tags", "https://github.com/" + r.Repository + ".git", "refs/heads/" + r.Args[0] + ":refs/workspace/main"}
	refs := []string{"refs/workspace/main"}
	if len(r.Args) == 2 {
		args = append(args, "refs/heads/"+r.Args[1]+":refs/workspace/pr")
		refs = append(refs, "refs/workspace/pr")
	}
	if err = git(args...); err != nil {
		return Response{}, err
	}
	bundle := filepath.Join(dir, "refs.bundle")
	if err = git(append([]string{"bundle", "create", bundle}, refs...)...); err != nil {
		return Response{}, err
	}
	info, err := os.Stat(bundle)
	if err != nil {
		return Response{}, err
	}
	if info.Size() > 256<<20 {
		return Response{}, errors.New("repository fetch exceeds 256 MiB transfer limit")
	}
	target := "/tmp/workspace-" + filepath.Base(dir) + ".bundle"
	if command(ctx, w.Executable, "cp", bundle, "ws-"+strings.ToLower(r.SessionID)+":"+target).Run() != nil {
		return Response{}, errors.New("could not transfer fetched refs")
	}
	// SBX preserves the host UID and private mode; the guest agent has a
	// different UID and must own the bundle to read and remove it in /tmp.
	if _, err = w.output(ctx, r.SessionID, s.Directory, "sudo", "chown", "agent:agent", target); err != nil {
		return Response{}, err
	}
	output, err := w.output(ctx, r.SessionID, s.Directory, "bash", "-c", fetchImportScript, "workspace-fetch", target, r.Args[0])
	return Response{Output: output}, err
}

const fetchImportScript = `set -euo pipefail
bundle=$1
trap 'rm -f "$bundle"' EXIT
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
git -c core.hooksPath=/dev/null bundle verify "$bundle" >/dev/null
refs=(+refs/workspace/main:refs/remotes/workspace/main "+refs/workspace/main:refs/remotes/origin/$2")
if git bundle list-heads "$bundle" refs/workspace/pr | grep -q .; then refs+=(+refs/workspace/pr:refs/remotes/workspace/pr); fi
git -c core.hooksPath=/dev/null fetch --no-tags "$bundle" "${refs[@]}"
printf 'Fetched main at '
git rev-parse refs/remotes/workspace/main
`
