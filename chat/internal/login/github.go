// Package login obtains provider sign-ins for local mode and writes each one
// as a private file in the provider directory. `warden login github` calls
// GitHub; a pasted `gh auth token` value goes through GitHubPaste. The
// policy service reads the resulting file (GitHubUserCredentials) and never
// refreshes it by itself; the owner console runs the same DeviceFlow through
// the policy service ("github_login_start") and stores the result where the
// service reads.
package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"warden/chat/internal/release"
)

// GitHubScopes are the classic OAuth scopes Warden asks for: repository
// contents and pull requests, plus organisation membership so organisation
// repositories appear in the selection list.
const GitHubScopes = "repo read:org"

// Overridable in tests: the OAuth (device code) and REST base URLs, the
// client ID, the HTTP client and the poll sleeper.
var (
	githubOAuthBase = "https://github.com"
	githubAPIBase   = "https://api.github.com"
	githubClientID  = release.GitHubOAuthClientID
	httpClient      = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	sleep           = func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
)

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

var githubTokenShape = regexp.MustCompile(`^(?:gho_|ghp_|github_pat_)[A-Za-z0-9_]{20,}$`)
var githubLoginShape = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)

// GitHubFile is the on-disk record `warden login github` writes and the
// policy service reads: {"token":"gho_…","login":"…","scopes":[…],"obtained":<unix>}.
type GitHubFile struct {
	Token    string   `json:"token"`
	Login    string   `json:"login"`
	Scopes   []string `json:"scopes"`
	Obtained int64    `json:"obtained"`
}

// DeviceCode is one device-flow attempt as GitHub describes it: the code the
// person types on the verification page, how long it lasts and how often to
// poll.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DeviceFlow runs GitHub's OAuth device flow: RequestCode, then Wait for
// the person to authorise. The CLI and the policy service (the owner
// console's "Sign in with GitHub") share it; tests point it at a fake
// GitHub. NewDeviceFlow fills in this release's client ID and the real
// endpoints; a nil Client or Sleep means the package's own.
type DeviceFlow struct {
	OAuthBase string
	APIBase   string
	ClientID  string
	Client    *http.Client
	Sleep     func(ctx context.Context, d time.Duration) error
}

// NewDeviceFlow returns the flow the CLI uses, built from the package
// defaults at call time.
func NewDeviceFlow() *DeviceFlow {
	return &DeviceFlow{OAuthBase: githubOAuthBase, APIBase: githubAPIBase, ClientID: githubClientID, Client: httpClient, Sleep: sleep}
}

func (d *DeviceFlow) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return httpClient
}

func (d *DeviceFlow) sleep(ctx context.Context, wait time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, wait)
	}
	return sleep(ctx, wait)
}

// Configured reports whether a client ID exists to start the flow with.
func (d *DeviceFlow) Configured() bool { return d.ClientID != "" }

// ErrNoClientID is returned when the release ships no OAuth client ID.
var ErrNoClientID = errors.New("the GitHub OAuth client ID is not configured in this release; paste a token with `warden login github --token` instead")

// RequestCode starts one attempt: GitHub returns the user code to show and
// the device code to poll with.
func (d *DeviceFlow) RequestCode(ctx context.Context) (*DeviceCode, error) {
	if !d.Configured() {
		return nil, ErrNoClientID
	}
	return d.requestDeviceCode(ctx)
}

// Wait polls until the person authorises the code (or it expires, is
// refused or ctx ends), confirms the token with GET /user and returns the
// record to store.
func (d *DeviceFlow) Wait(ctx context.Context, code *DeviceCode) (GitHubFile, error) {
	token, scopes, err := d.pollDeviceToken(ctx, code)
	if err != nil {
		return GitHubFile{}, err
	}
	login, headerScopes, err := d.verifyToken(ctx, token)
	if err != nil {
		return GitHubFile{}, err
	}
	if len(scopes) == 0 {
		scopes = headerScopes
	}
	if scopes == nil {
		scopes = []string{}
	}
	return GitHubFile{Token: token, Login: login, Scopes: scopes, Obtained: time.Now().Unix()}, nil
}

// GitHub runs GitHub's OAuth device flow with Warden's client ID, prints
// the verification URL and user code to w, polls at the server's interval
// (adding five seconds on slow_down), records the signed-in login from
// GET /user and writes authFile atomically with mode 0600.
// OpenBrowser and CopyToClipboard are set by the CLI; nil (the default, and
// what tests use) means print the instructions only.
var (
	OpenBrowser     func(url string) error
	CopyToClipboard func(text string) error
)

func GitHub(ctx context.Context, w io.Writer, authFile string) error {
	flow := NewDeviceFlow()
	code, err := flow.RequestCode(ctx)
	if err != nil {
		return err
	}
	// GitHub's device flow has no URL that carries the code, so the browser
	// lands on the verification page and the person types the code. Put it
	// on the clipboard when a clipboard command exists, then open the page.
	copied := CopyToClipboard != nil && CopyToClipboard(code.UserCode) == nil
	opened := OpenBrowser != nil && OpenBrowser(code.VerificationURI) == nil
	switch {
	case opened && copied:
		fmt.Fprintf(w, "Opened %s in your browser; the code %s is on your clipboard, paste it there.\n", code.VerificationURI, code.UserCode)
	case opened:
		fmt.Fprintf(w, "Opened %s in your browser; enter the code %s there.\n", code.VerificationURI, code.UserCode)
	default:
		fmt.Fprintf(w, "Open %s in a browser and enter the code %s\n", code.VerificationURI, code.UserCode)
	}
	fmt.Fprintf(w, "Waiting for GitHub (the code expires in %d minutes)...\n", code.ExpiresIn/60)
	record, err := flow.Wait(ctx, code)
	if err != nil {
		return err
	}
	if err = writeGitHubFile(authFile, record); err != nil {
		return err
	}
	fmt.Fprintf(w, "Signed in to GitHub as %s; sign-in stored at %s\n", record.Login, authFile)
	return nil
}

// GitHubPaste stores a token the person obtained elsewhere (for example
// `gh auth token`) after checking its shape and that GitHub accepts it.
func GitHubPaste(token, authFile string) error {
	token = strings.TrimSpace(token)
	if !githubTokenShape.MatchString(token) {
		return errors.New("expected a GitHub OAuth token (gho_…), classic personal access token (ghp_…) or fine-grained token (github_pat_…)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	login, scopes, err := NewDeviceFlow().verifyToken(ctx, token)
	if err != nil {
		return err
	}
	return writeGitHubFile(authFile, GitHubFile{Token: token, Login: login, Scopes: scopes, Obtained: time.Now().Unix()})
}

func (d *DeviceFlow) postForm(ctx context.Context, endpoint string, form url.Values) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Warden-GitHub-Login")
	res, err := d.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("GitHub is unreachable; check the network and retry")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(data) > 65536 {
		return nil, errors.New("GitHub returned an unreadable response")
	}
	var result map[string]any
	if err = json.Unmarshal(data, &result); err != nil || result == nil {
		return nil, fmt.Errorf("GitHub returned an unexpected response (HTTP %d)", res.StatusCode)
	}
	return result, nil
}

func (d *DeviceFlow) requestDeviceCode(ctx context.Context) (*DeviceCode, error) {
	form := url.Values{"client_id": {d.ClientID}, "scope": {GitHubScopes}}
	result, err := d.postForm(ctx, d.OAuthBase+"/login/device/code", form)
	if err != nil {
		return nil, err
	}
	if e, _ := result["error"].(string); e != "" {
		return nil, fmt.Errorf("GitHub refused the device code request (%s); check that the Warden OAuth App has device flow enabled", e)
	}
	raw, _ := json.Marshal(result)
	var code DeviceCode
	if err = json.Unmarshal(raw, &code); err != nil {
		return nil, errors.New("GitHub returned an unexpected device code response")
	}
	if code.DeviceCode == "" || code.UserCode == "" || !strings.HasPrefix(code.VerificationURI, "https://") || code.ExpiresIn <= 0 {
		return nil, errors.New("GitHub returned an incomplete device code response")
	}
	if code.Interval < 1 {
		code.Interval = 5
	}
	return &code, nil
}

// pollDeviceToken follows the device flow: wait interval seconds between
// requests, add five on slow_down, keep waiting on authorization_pending
// and stop on any other error or on expiry.
func (d *DeviceFlow) pollDeviceToken(ctx context.Context, code *DeviceCode) (string, []string, error) {
	interval := time.Duration(code.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	form := url.Values{"client_id": {d.ClientID}, "device_code": {code.DeviceCode}, "grant_type": {deviceGrant}}
	for {
		if err := d.sleep(ctx, interval); err != nil {
			return "", nil, err
		}
		if time.Now().After(deadline) {
			return "", nil, errors.New("the GitHub sign-in code expired; run the login again")
		}
		result, err := d.postForm(ctx, d.OAuthBase+"/login/oauth/access_token", form)
		if err != nil {
			return "", nil, err
		}
		switch e, _ := result["error"].(string); e {
		case "":
			token, _ := result["access_token"].(string)
			tokenType, _ := result["token_type"].(string)
			if !githubTokenShape.MatchString(token) || !strings.EqualFold(tokenType, "bearer") {
				return "", nil, errors.New("GitHub returned an unexpected token")
			}
			scope, _ := result["scope"].(string)
			return token, splitScopes(scope), nil
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			if n, ok := result["interval"].(float64); ok && n > 0 {
				interval = time.Duration(n) * time.Second
			}
			continue
		case "expired_token":
			return "", nil, errors.New("the GitHub sign-in code expired; run the login again")
		case "access_denied":
			return "", nil, errors.New("the GitHub sign-in was cancelled")
		default:
			return "", nil, fmt.Errorf("GitHub sign-in failed (%s)", e)
		}
	}
}

// verifyToken calls GET /user with the token and returns the login and the
// classic scopes GitHub reports in X-OAuth-Scopes.
func (d *DeviceFlow) verifyToken(ctx context.Context, token string) (string, []string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", d.APIBase+"/user", nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Warden-GitHub-Login")
	res, err := d.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, errors.New("GitHub is unreachable; check the network and retry")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(data) > 65536 {
		return "", nil, errors.New("GitHub returned an unreadable response")
	}
	if res.StatusCode == 401 {
		return "", nil, errors.New("GitHub rejected the token; it may be revoked or expired")
	}
	if res.StatusCode != 200 {
		return "", nil, fmt.Errorf("GitHub could not identify the account (HTTP %d)", res.StatusCode)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err = json.Unmarshal(data, &user); err != nil || !githubLoginShape.MatchString(user.Login) {
		return "", nil, errors.New("GitHub returned an unexpected account response")
	}
	return user.Login, splitScopes(res.Header.Get("X-OAuth-Scopes")), nil
}

func splitScopes(value string) []string {
	var out []string
	for _, s := range strings.Split(value, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// writeGitHubFile creates the provider directory (0700) if needed and
// replaces authFile atomically with a 0600 file that never has a wider mode.
func writeGitHubFile(authFile string, record GitHubFile) error {
	if authFile == "" {
		return errors.New("an auth file path is required")
	}
	if record.Scopes == nil {
		record.Scopes = []string{}
	}
	dir := filepath.Dir(authFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".github-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err = tmp.Chmod(0o600); err != nil {
		return cleanup(err)
	}
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		return cleanup(err)
	}
	if err = tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err = os.Rename(tmpName, authFile); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
