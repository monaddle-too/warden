package main

import (
	"context"
	"io"

	"warden/chat/internal/login"
)

// gitHubLogin is the surface of chat/internal/login that `warden login
// github` uses; tests substitute a fake through the githubLogin variable.
type gitHubLogin interface {
	Device(ctx context.Context, w io.Writer, authFile string) error
	Paste(token, authFile string) error
}

var githubLogin gitHubLogin = realGitHubLogin{}

// realGitHubLogin runs GitHub's device flow, or verifies a pasted token,
// and writes the private auth file the policy service reads.
type realGitHubLogin struct{}

func (realGitHubLogin) Device(ctx context.Context, w io.Writer, authFile string) error {
	return login.GitHub(ctx, w, authFile)
}
func (realGitHubLogin) Paste(token, authFile string) error { return login.GitHubPaste(token, authFile) }
