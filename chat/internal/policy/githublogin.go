package policy

import (
	"context"
	"errors"
	"sync"
	"time"

	"warden/chat/internal/login"
)

// githubSaver is the credential source the console can sign in to: the
// user-token source (a file or a Secret). The App broker is the operator's
// and has no sign-in of its own.
type githubSaver interface {
	Save(record login.GitHubFile) error
	Details() map[string]any
}

// GitHubSignIn is the owner console's GitHub sign-in ("Sign in with
// GitHub" / "Refresh sign-in"): GitHub's device flow, the same one `warden
// login github` runs, run here because the policy service owns the
// credential. Start requests a code for the person to type at GitHub and
// polls in the background; Status reports pending, done or failed; the
// result is saved through the credential source, so it takes effect on the
// next use. One attempt at a time: a second Start while one is pending
// returns the same code (a reloaded page), a new one replaces a finished
// attempt.
type GitHubSignIn struct {
	mu sync.Mutex
	// Flow is the device flow to run, this release's when nil; tests point
	// it at a fake GitHub.
	Flow *login.DeviceFlow
	// Wall time; the sharing store's clock when set (tests).
	Clock Clock

	state           string // "", "pending", "done", "failed"
	userCode        string
	verificationURI string
	expiresAt       float64
	login           string
	failure         string
	cancel          context.CancelFunc
	done            chan struct{}
}

func (g *GitHubSignIn) now() float64 {
	if g.Clock != nil {
		return g.Clock()
	}
	return wallClock()
}

func (g *GitHubSignIn) flow() *login.DeviceFlow {
	if g.Flow != nil {
		return g.Flow
	}
	return login.NewDeviceFlow()
}

// Start begins an attempt unless one is pending. complete is called once,
// off the caller's goroutine, with the record GitHub confirmed; its error
// (a failed save) fails the attempt.
func (g *GitHubSignIn) Start(complete func(record login.GitHubFile) error) (map[string]any, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == "pending" && g.expiresAt > g.now() {
		return g.statusLocked(), nil
	}
	flow := g.flow()
	if !flow.Configured() {
		return nil, login.ErrNoClientID
	}
	ctx, cancel := context.WithCancel(context.Background())
	code, err := flow.RequestCode(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	g.cancelLocked()
	g.state, g.userCode, g.verificationURI = "pending", code.UserCode, code.VerificationURI
	g.expiresAt = g.now() + float64(code.ExpiresIn)
	g.login, g.failure = "", ""
	g.cancel = cancel
	done := make(chan struct{})
	g.done = done
	go g.run(ctx, cancel, done, flow, code, complete)
	return g.statusLocked(), nil
}

func (g *GitHubSignIn) run(ctx context.Context, cancel context.CancelFunc, done chan struct{}, flow *login.DeviceFlow, code *login.DeviceCode, complete func(login.GitHubFile) error) {
	defer close(done)
	defer cancel()
	record, err := flow.Wait(ctx, code)
	if err == nil {
		err = complete(record)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done != done {
		// Replaced or cancelled meanwhile; that attempt owns the state.
		return
	}
	g.cancel = nil
	switch {
	case ctx.Err() != nil && err != nil && errors.Is(err, ctx.Err()):
		g.state, g.failure = "failed", "the GitHub sign-in was cancelled"
	case err != nil:
		g.state, g.failure = "failed", err.Error()
	default:
		g.state, g.login = "done", record.Login
	}
	g.userCode, g.verificationURI, g.expiresAt = "", "", 0
}

// Cancel stops a pending attempt; finished ones are forgotten.
func (g *GitHubSignIn) Cancel() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cancelLocked()
	g.state, g.userCode, g.verificationURI, g.expiresAt, g.login, g.failure = "", "", "", 0, "", ""
}

func (g *GitHubSignIn) cancelLocked() {
	if g.cancel != nil {
		g.cancel()
		g.cancel = nil
	}
	g.done = nil
}

// Status reports the attempt as the console shows it. A pending code that
// GitHub has expired reads as failed even before the poller notices.
func (g *GitHubSignIn) Status() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.statusLocked()
}

func (g *GitHubSignIn) statusLocked() map[string]any {
	state := g.state
	if state == "" {
		state = "none"
	}
	out := map[string]any{"status": state}
	switch state {
	case "pending":
		if g.expiresAt <= g.now() {
			out["status"] = "failed"
			out["error"] = "the GitHub sign-in code expired; start the sign-in again"
			return out
		}
		out["user_code"], out["verification_uri"], out["expires_at"] = g.userCode, g.verificationURI, g.expiresAt
	case "done":
		out["login"] = g.login
	case "failed":
		out["error"] = g.failure
	}
	return out
}

// Wait blocks until the current attempt finishes or d passes (tests).
func (g *GitHubSignIn) Wait(d time.Duration) bool {
	g.mu.Lock()
	done := g.done
	g.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// githubSignIn handles the console's sign-in operations. Only the
// user-token source can be signed in to; the App broker is the operator's.
func (s *Sharing) githubSignIn(op string, data map[string]any) (map[string]any, error) {
	saver, ok := s.GitHub.(githubSaver)
	if !ok {
		return nil, errors.New("this GitHub connection is managed by the operator and cannot be signed in to here")
	}
	switch op {
	case "github_login_status":
		return s.SignIn.Status(), nil
	case "github_login_cancel":
		s.SignIn.Cancel()
		return map[string]any{"ok": true}, nil
	case "github_login_start":
		previous, _ := saver.Details()["login"].(string)
		actor := actorOf(data)
		return s.SignIn.Start(func(record login.GitHubFile) error {
			if err := saver.Save(record); err != nil {
				return err
			}
			return s.githubSignedIn(previous, record.Login, actor)
		})
	}
	return nil, errors.New("unknown sharing operation")
}

// githubSignedIn records a completed sign-in. A refreshed sign-in as the
// same account keeps its repository selections (that is what a refresh is
// for); a different account must not inherit them, exactly as after a
// disconnect.
func (s *Sharing) githubSignedIn(previous, current, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DB == nil {
		return errors.New("sharing store closed")
	}
	detail := map[string]any{"login": current}
	if previous != "" && previous != current {
		if _, err := s.DB.Exec("DELETE FROM repositories"); err != nil {
			return err
		}
		detail["previous"] = previous
	}
	_, err := s.DB.Exec("INSERT INTO repository_events (at,sandbox,actor,kind,detail) VALUES (?,?,?,?,?)", s.Clock(), "", actor, "github_signed_in", string(mustJSON(detail)))
	return err
}
