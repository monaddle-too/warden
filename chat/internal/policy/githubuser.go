package policy

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubCredentials is the credential source behind approved GitHub
// requests: the App broker on OVH or a user token file in local mode. The
// gateway, the sharing store and pull request creation only see this
// interface; the injection point and redaction are the same for both.
type GitHubCredentials interface {
	// Identity reports the account the credential acts as and the GitHub App
	// ID, which is 0 for a user token. A user token whose file is missing or
	// unreadable fails closed here as everywhere else.
	Identity() (owner string, appID int64, err error)
	// Authorization returns the header value for one approved operation on
	// one repository, optionally pinned to the repository ID recorded at
	// selection time.
	Authorization(repository, operation string, repositoryID *int64) (string, error)
	// Repositories lists what the credential can select, one page at a time.
	Repositories(page int64) (map[string]any, error)
	Snapshot() map[string]any
}

// Identity implements GitHubCredentials for the App broker.
func (c *GitHubAppCredentials) Identity() (string, int64, error) { return c.Owner, c.AppID, nil }

// GitHubRefreshMessage is the fail-closed message for a missing, unreadable
// or rejected user token, in the wording the Codex and Claude sources use.
const GitHubRefreshMessage = "Refresh the GitHub sign-in before using repositories"

var userTokenShape = regexp.MustCompile(`^(?:gho_|ghp_|github_pat_)[A-Za-z0-9_]{20,}$`)

// GitHubUserTransport performs one authenticated GitHub REST call and
// returns the raw status and body so array responses can be paged.
type GitHubUserTransport func(method, path, token string) (status int, data []byte, err error)

// githubUserRequest is the real transport: fixed host, bounded body, no
// redirects, 15 seconds.
func githubUserRequest(method, path, token string) (int, []byte, error) {
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28", "User-Agent": "Warden-GitHub-Login"}
	status, data, err := directHTTPS("api.github.com", method, path, nil, headers, 15*time.Second, 1048577)
	if err != nil {
		return 0, nil, err
	}
	if len(data) > 1048576 {
		return 0, nil, errors.New("GitHub response exceeds the 1 MiB inspection limit")
	}
	return status, data, nil
}

// GitHubUserCredentials reads the user OAuth token `warden login github`
// wrote, exactly as CodexCredentials reads its login: a private 0600 file,
// never copied into a guest, never refreshed, read again on every use so a
// new sign-in takes effect without a restart. Actions appear as the person.
type GitHubUserCredentials struct {
	Path      string
	Redactor  *Redactor
	Clock     Clock
	Transport GitHubUserTransport
	mu        sync.Mutex
}

// NewGitHubUserCredentials returns the user-token source for one file.
func NewGitHubUserCredentials(path string, redactor *Redactor, clock Clock) *GitHubUserCredentials {
	if redactor == nil {
		redactor = NewRedactor()
	}
	return &GitHubUserCredentials{Path: path, Redactor: redactor, Clock: clock, Transport: githubUserRequest}
}

// githubUserFile is the on-disk shape: {"token":"gho_…","login":"…",
// "scopes":["repo","read:org"],"obtained":<unix seconds>}.
type githubUserFile struct {
	Token    string
	Login    string
	Scopes   []string
	Obtained float64
}

func (c *GitHubUserCredentials) read() (*githubUserFile, error) {
	raw, err := openPrivate(c.Path, 65536, "GitHub credential file")
	if err != nil {
		return nil, errors.New(GitHubRefreshMessage)
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil || data == nil {
		return nil, errors.New(GitHubRefreshMessage)
	}
	f := &githubUserFile{}
	f.Token, _ = data["token"].(string)
	f.Login, _ = data["login"].(string)
	if !userTokenShape.MatchString(f.Token) || !ownerShape.MatchString(f.Login) {
		return nil, errors.New(GitHubRefreshMessage)
	}
	if list, present := data["scopes"]; present {
		items, ok := list.([]any)
		if !ok {
			return nil, errors.New(GitHubRefreshMessage)
		}
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, errors.New(GitHubRefreshMessage)
			}
			f.Scopes = append(f.Scopes, s)
		}
	}
	if v, present := data["obtained"]; present {
		n, ok := asNumber(v)
		if !ok {
			return nil, errors.New(GitHubRefreshMessage)
		}
		f.Obtained = n
	}
	c.Redactor.Register(f.Token)
	return f, nil
}

// Available reports whether the sign-in file is readable and well formed.
func (c *GitHubUserCredentials) Available() bool {
	_, err := c.read()
	return err == nil
}

// Identity returns the signed-in login; the App ID is always 0.
func (c *GitHubUserCredentials) Identity() (string, int64, error) {
	f, err := c.read()
	if err != nil {
		return "", 0, err
	}
	return f.Login, 0, nil
}

// repository fetches one repository record with the token; a 401 means the
// token was revoked and the sign-in must be refreshed.
func (c *GitHubUserCredentials) repository(token, repository string) (map[string]any, error) {
	status, data, err := c.Transport("GET", "/repos/"+repository, token)
	if err != nil {
		return nil, errors.New("GitHub is unavailable; retry the request")
	}
	switch status {
	case 200:
	case 401:
		return nil, errors.New(GitHubRefreshMessage)
	default:
		return nil, errors.New("repository is not available to the signed-in GitHub account")
	}
	var repo map[string]any
	if err = json.Unmarshal(data, &repo); err != nil || repo == nil {
		return nil, errors.New("invalid GitHub repository response")
	}
	return repo, nil
}

// Authorization is called only after a grant matched. It rereads the file,
// confirms the token still reaches the repository (and that the repository
// is still the one selected, when an ID is pinned) and returns the bearer
// header. Nothing is cached, so a revoked token fails on the next approval.
func (c *GitHubUserCredentials) Authorization(repository, operation string, repositoryID *int64) (string, error) {
	if !repositoryShape.MatchString(repository) {
		return "", errors.New("invalid repository")
	}
	if _, err := GitHubPermissions(operation); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := c.read()
	if err != nil {
		return "", err
	}
	repo, err := c.repository(f.Token, repository)
	if err != nil {
		return "", err
	}
	if lowerString(repo["full_name"]) != strings.ToLower(repository) {
		return "", errors.New("repository identity changed; select it again")
	}
	if repositoryID != nil {
		id, ok := asInt(repo["id"])
		if !ok || id != *repositoryID {
			return "", errors.New("repository identity changed; select it again")
		}
	}
	return "Bearer " + f.Token, nil
}

// Repositories lists the user's own, collaborator and organisation
// repositories, 100 per page, in the shape the App broker returns so
// selection code is shared. There is no owner filter: the token, the
// selection and the per-request approvals are the boundary.
func (c *GitHubUserCredentials) Repositories(page int64) (map[string]any, error) {
	if page < 1 || page > 10000 {
		return nil, errors.New("invalid repository page")
	}
	f, err := c.read()
	if err != nil {
		return nil, err
	}
	status, data, err := c.Transport("GET", "/user/repos?per_page=100&affiliation=owner,collaborator,organization_member&sort=full_name&page="+strconv.FormatInt(page, 10), f.Token)
	if err != nil {
		return nil, errors.New("GitHub is unavailable; retry the request")
	}
	if status == 401 {
		return nil, errors.New(GitHubRefreshMessage)
	}
	if status != 200 {
		return nil, errors.New("GitHub repository listing failed; retry the request")
	}
	var listing []any
	if err = json.Unmarshal(data, &listing); err != nil {
		return nil, errors.New("invalid repository listing")
	}
	repos := []any{}
	for _, item := range listing {
		repo, _ := item.(map[string]any)
		name, _ := repo["full_name"].(string)
		id, idOK := asInt(repo["id"])
		if !repositoryShape.MatchString(name) || !idOK || id <= 0 {
			return nil, errors.New("invalid repository listing")
		}
		private, _ := repo["private"].(bool)
		repos = append(repos, map[string]any{"id": id, "full_name": name, "private": private})
	}
	var next any
	if len(listing) == 100 {
		next = page + 1
	}
	return map[string]any{"owner": f.Login, "app_id": int64(0), "repositories": repos, "next_page": next}, nil
}

// Details reports the sign-in as the UI shows it: login, scopes and when
// the token was stored. Never the token. Empty when no sign-in is readable.
func (c *GitHubUserCredentials) Details() map[string]any {
	f, err := c.read()
	if err != nil {
		return map[string]any{"mode": "user"}
	}
	scopes := []any{}
	for _, s := range f.Scopes {
		scopes = append(scopes, s)
	}
	return map[string]any{"mode": "user", "login": f.Login, "scopes": scopes, "obtained": f.Obtained}
}

// Disconnect deletes the sign-in file. GitHub OAuth tokens can only be
// revoked with the App's client secret, which a release does not ship, so
// the token stays valid at GitHub until the person revokes it under
// Settings, Applications; the UI says so. Every later use fails closed.
func (c *GitHubUserCredentials) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Remove(c.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Snapshot describes the source without the token.
func (c *GitHubUserCredentials) Snapshot() map[string]any {
	login := ""
	if f, err := c.read(); err == nil {
		login = f.Login
	}
	return map[string]any{"configured": true, "app_id": int64(0), "owner": login, "identity": "user",
		"credential_source": "private_host_file", "manual_token_required": false}
}
