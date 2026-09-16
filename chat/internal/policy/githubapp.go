package policy

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var repositoryShape = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

func installationTokenShape(s string) bool {
	return strings.HasPrefix(s, "ghs_") && boundedClass(s[4:], 16, 8192, func(c byte) bool { return isAlnum(c) || strings.IndexByte("._~-", c) >= 0 })
}

var ownerShape = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)

var contentsRead = stringSet("git/read", "repos/get-content", "repos/list-branches", "repos/get-branch",
	"repos/list-commits", "repos/get-commit", "repos/compare-commits",
	"git/get-blob", "git/get-tree", "git/get-commit", "git/get-ref", "git/list-matching-refs")
var contentsWrite = stringSet("git/push", "git/create-blob", "git/create-tree", "git/create-commit",
	"git/create-ref", "git/update-ref", "git/delete-ref", "repos/create-or-update-file-contents",
	"repos/delete-file", "repos/merge")
var pullRead = stringSet("pulls/get", "pulls/list", "pulls/list-files", "pulls/list-commits", "pulls/list-reviews", "pulls/list-review-comments")
var pullWrite = stringSet("pulls/create", "pulls/update", "pulls/merge", "pulls/create-review", "pulls/create-review-comment")

func stringSet(values ...string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		out[v] = true
	}
	return out
}

// GitHubPermissions maps a supported operation to the installation token
// permissions it needs. Unknown operations cannot inherit app permissions.
func GitHubPermissions(operation string) (map[string]string, error) {
	switch {
	case operation == "repos/get":
		return map[string]string{"metadata": "read"}, nil
	case contentsRead[operation]:
		return map[string]string{"contents": "read", "metadata": "read"}, nil
	case contentsWrite[operation]:
		return map[string]string{"contents": "write", "metadata": "read"}, nil
	case pullRead[operation]:
		return map[string]string{"pull_requests": "read", "metadata": "read"}, nil
	case pullWrite[operation]:
		return map[string]string{"pull_requests": "write", "metadata": "read"}, nil
	}
	return nil, errors.New("operation is not supported by the GitHub App broker")
}

// GitHubTransport performs one authenticated GitHub REST call.
type GitHubTransport func(method, path, token string, body map[string]any) (map[string]any, error)

// GitHubRequest is the real transport: fixed host, bounded JSON, 15 seconds.
func GitHubRequest(method, path, token string, body map[string]any) (map[string]any, error) {
	var payload []byte
	if body != nil {
		payload = mustJSON(body)
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28", "User-Agent": "Warden-GitHub-App", "Content-Type": "application/json"}
	status, data, err := directHTTPS("api.github.com", method, path, payload, headers, 15*time.Second, 1048577)
	if err != nil {
		return nil, err
	}
	if len(data) > 1048576 {
		return nil, errors.New("GitHub response exceeds the 1 MiB inspection limit")
	}
	if status != 200 && status != 201 {
		return nil, errors.New("GitHub App request failed; check installation access")
	}
	var result map[string]any
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// StoreBroker mints installation tokens from the existing app record in
// Panta's encrypted store. It never reads user token records.
// DefaultGitHubAppSlug is the App the OVH deployment installed; the broker
// subcommand assumes it unless --app-slug (providers.github.appSlug) says otherwise.
const DefaultGitHubAppSlug = "monaddle-workspace"

type StoreBroker struct {
	Directory string
	AppID     int64
	AppSlug   string
	Owner     string
	Transport GitHubTransport
	Clock     Clock
	// app is replaceable in tests.
	app func() (map[string]any, error)
}

func NewStoreBroker(directory string, appID int64, owner string, transport GitHubTransport) *StoreBroker {
	b := &StoreBroker{Directory: directory, AppID: appID, AppSlug: DefaultGitHubAppSlug, Owner: owner, Transport: transport}
	b.app = b.readApp
	return b
}

func (b *StoreBroker) now() float64 {
	if b.Clock != nil {
		return b.Clock()
	}
	return wallClock()
}

func (b *StoreBroker) readApp() (map[string]any, error) {
	key, err := openPrivate(filepath.Join(b.Directory, "encryption.key"), 32, "credential configuration")
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("credential configuration must be a private regular file")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(b.Directory, "github.sqlite")+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var sealed []byte
	if err = db.QueryRow("SELECT value FROM secrets WHERE key=?", "app").Scan(&sealed); err != nil {
		return nil, errors.New("existing GitHub App record is missing")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < 12 {
		return nil, errors.New("invalid GitHub App record")
	}
	plain, err := gcm.Open(nil, sealed[:12], sealed[12:], []byte("app"))
	if err != nil {
		return nil, errors.New("invalid GitHub App record")
	}
	var app map[string]any
	if err = json.Unmarshal(plain, &app); err != nil {
		return nil, err
	}
	id, _ := asInt(app["id"])
	if id != b.AppID || app["slug"] != b.AppSlug {
		return nil, errors.New("unexpected GitHub App identity")
	}
	return app, nil
}

func parseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("invalid app signing key")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("invalid app signing key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid app signing key")
	}
	return key, nil
}

// JWT signs a short-lived app token with the stored private key.
func (b *StoreBroker) JWT(app map[string]any) (string, error) {
	pemText, _ := app["pem"].(string)
	key, err := parseRSAPrivateKey(pemText)
	if err != nil {
		return "", err
	}
	if key.N.BitLen() < 2048 {
		return "", errors.New("invalid app signing key")
	}
	now := int64(b.now())
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`, now-60, now+540, b.AppID)
	data := header + "." + base64.RawURLEncoding.EncodeToString([]byte(claims))
	digest := sha256.Sum256([]byte(data))
	signature, err := rsa.SignPKCS1v15(cryptoRandReader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return data + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func lowerString(v any) string {
	s, _ := v.(string)
	return strings.ToLower(s)
}

func (b *StoreBroker) installationValid(installation map[string]any) (int64, bool) {
	appID, _ := asInt(installation["app_id"])
	account, _ := installation["account"].(map[string]any)
	id, ok := asInt(installation["id"])
	if appID != b.AppID || installation["suspended_at"] != nil || lowerString(account["login"]) != strings.ToLower(b.Owner) || !ok || id <= 0 {
		return 0, false
	}
	return id, true
}

// Repositories lists repositories the owner's installation exposes, using
// metadata-only discovery credentials.
func (b *StoreBroker) Repositories(page int64) (map[string]any, error) {
	if page < 1 || page > 10000 {
		return nil, errors.New("invalid repository page")
	}
	app, err := b.app()
	if err != nil {
		return nil, err
	}
	jwt, err := b.JWT(app)
	if err != nil {
		return nil, err
	}
	installation, err := b.Transport("GET", "/users/"+b.Owner+"/installation", jwt, nil)
	if err != nil {
		return nil, err
	}
	account, _ := installation["account"].(map[string]any)
	id, ok := b.installationValid(installation)
	if !ok || account["type"] != "User" {
		return nil, errors.New("unexpected GitHub account installation")
	}
	credential, err := b.Transport("POST", "/app/installations/"+strconv.FormatInt(id, 10)+"/access_tokens", jwt, map[string]any{"permissions": map[string]any{"metadata": "read"}})
	if err != nil {
		return nil, err
	}
	if !jsonEqual(credential["permissions"], map[string]any{"metadata": "read"}) {
		return nil, errors.New("unexpected discovery permissions")
	}
	token, _ := credential["token"].(string)
	data, err := b.Transport("GET", "/installation/repositories?per_page=100&page="+strconv.FormatInt(page, 10), token, nil)
	if err != nil {
		return nil, err
	}
	listing, _ := data["repositories"].([]any)
	repos := []any{}
	for _, item := range listing {
		repo, _ := item.(map[string]any)
		name, _ := repo["full_name"].(string)
		owner, _ := repo["owner"].(map[string]any)
		if repositoryShape.MatchString(name) && strings.ToLower(strings.SplitN(name, "/", 2)[0]) == strings.ToLower(b.Owner) && lowerString(owner["login"]) == strings.ToLower(b.Owner) {
			private, _ := repo["private"].(bool)
			repos = append(repos, map[string]any{"id": repo["id"], "full_name": name, "private": private})
		}
	}
	var next any
	if len(listing) == 100 {
		next = page + 1
	}
	return map[string]any{"owner": b.Owner, "app_id": b.AppID, "repositories": repos, "next_page": next, "installation_id": id}, nil
}

// Issue mints a repository-scoped installation token for one operation.
func (b *StoreBroker) Issue(value map[string]any) (map[string]any, error) {
	if value == nil || !sameKeys(value, "repository", "operation") {
		return nil, errors.New("invalid broker request")
	}
	repo, _ := value["repository"].(string)
	if !repositoryShape.MatchString(repo) || strings.ToLower(strings.SplitN(repo, "/", 2)[0]) != strings.ToLower(b.Owner) {
		return nil, errors.New("repository outside configured installation owner")
	}
	operation, _ := value["operation"].(string)
	perms, err := GitHubPermissions(operation)
	if err != nil {
		return nil, err
	}
	app, err := b.app()
	if err != nil {
		return nil, err
	}
	jwt, err := b.JWT(app)
	if err != nil {
		return nil, err
	}
	installation, err := b.Transport("GET", "/repos/"+repo+"/installation", jwt, nil)
	if err != nil {
		return nil, err
	}
	id, ok := b.installationValid(installation)
	if !ok {
		return nil, errors.New("unexpected or suspended installation")
	}
	permsAny := map[string]any{}
	for k, v := range perms {
		permsAny[k] = v
	}
	result, err := b.Transport("POST", "/app/installations/"+strconv.FormatInt(id, 10)+"/access_tokens", jwt,
		map[string]any{"repositories": []any{strings.SplitN(repo, "/", 2)[1]}, "permissions": permsAny})
	if err != nil {
		return nil, err
	}
	if !jsonEqual(result["permissions"], permsAny) {
		return nil, errors.New("unexpected installation token permissions")
	}
	repositories, ok := result["repositories"].([]any)
	if !ok || len(repositories) != 1 {
		return nil, errors.New("unexpected installation token repositories")
	}
	first, _ := repositories[0].(map[string]any)
	if lowerString(first["full_name"]) != strings.ToLower(repo) {
		return nil, errors.New("unexpected installation token repositories")
	}
	return map[string]any{"token": result["token"], "expires_at": result["expires_at"], "repository": repo,
		"permissions": permsAny, "app_id": b.AppID, "installation_id": id, "repository_id": first["id"]}, nil
}

// RunGitHubBroker implements the `github-broker` subcommand: one JSON request
// on stdin, one JSON result on stdout, generic errors only.
func RunGitHubBroker(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("github-broker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	store := fs.String("store", "", "")
	appID := fs.Int64("app-id", 0, "")
	owner := fs.String("owner", "", "")
	slug := fs.String("app-slug", DefaultGitHubAppSlug, "")
	if err := fs.Parse(args); err != nil || *store == "" || *appID == 0 || *owner == "" || *slug == "" {
		fmt.Fprint(stdout, `{"error":"GitHub App credential unavailable"}`)
		return 1
	}
	data, err := io.ReadAll(io.LimitReader(stdin, 4097))
	if err != nil || len(data) > 4096 {
		fmt.Fprint(stdout, `{"error":"GitHub App credential unavailable"}`)
		return 1
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		fmt.Fprint(stdout, `{"error":"GitHub App credential unavailable"}`)
		return 1
	}
	broker := NewStoreBroker(*store, *appID, *owner, GitHubRequest)
	broker.AppSlug = *slug
	var result map[string]any
	if sameKeys(value, "action", "page") && value["action"] == "repositories" {
		page, _ := asInt(value["page"])
		result, err = broker.Repositories(page)
	} else {
		result, err = broker.Issue(value)
	}
	if err != nil {
		fmt.Fprint(stdout, `{"error":"GitHub App credential unavailable"}`)
		return 1
	}
	fmt.Fprint(stdout, string(mustJSON(result)))
	return 0
}

// GitHubAppCredentials runs the trusted broker command for each approved
// dispatch. One source can serve multiple engines; grants stay separate.
type GitHubAppCredentials struct {
	Command  []string
	Owner    string
	AppID    int64
	Redactor *Redactor
	Clock    Clock
	mu       sync.Mutex
	// run is replaceable in tests.
	run func(input []byte) ([]byte, error)
}

func NewGitHubAppCredentials(command []string, owner string, appID int64, redactor *Redactor, clock Clock) *GitHubAppCredentials {
	c := &GitHubAppCredentials{Command: command, Owner: owner, AppID: appID, Redactor: redactor, Clock: clock}
	c.run = c.execute
	return c
}

func (c *GitHubAppCredentials) now() float64 {
	if c.Clock != nil {
		return c.Clock()
	}
	return wallClock()
}

func (c *GitHubAppCredentials) execute(input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, limit: 65537}
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

type limitedWriter struct {
	w     *bytes.Buffer
	limit int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	remaining := l.limit - l.w.Len()
	if remaining > 0 {
		if len(p) > remaining {
			l.w.Write(p[:remaining])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

func parseExpiry(value string) (float64, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.Replace(value, "Z", "+00:00", 1))
	if err != nil {
		t, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return 0, err
		}
	}
	return float64(t.UnixNano()) / 1e9, nil
}

// Authorization mints a token only after a grant matched; nothing is cached
// so each approval rechecks installation access and repository selection.
func (c *GitHubAppCredentials) Authorization(repository, operation string, repositoryID *int64) (string, error) {
	if !repositoryShape.MatchString(repository) || strings.ToLower(strings.SplitN(repository, "/", 2)[0]) != strings.ToLower(c.Owner) {
		return "", errors.New("repository outside configured GitHub App owner")
	}
	perms, err := GitHubPermissions(operation)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := c.run(mustJSON(map[string]any{"repository": repository, "operation": operation}))
	if err != nil {
		return "", err
	}
	if len(out) > 65536 {
		return "", errors.New("oversized broker response")
	}
	var data map[string]any
	if err = json.Unmarshal(out, &data); err != nil {
		return "", err
	}
	token, _ := data["token"].(string)
	appID, _ := asInt(data["app_id"])
	permsAny := map[string]any{}
	for k, v := range perms {
		permsAny[k] = v
	}
	if !installationTokenShape(token) || lowerString(data["repository"]) != strings.ToLower(repository) || !jsonEqual(data["permissions"], permsAny) || appID != c.AppID {
		return "", errors.New("invalid broker response")
	}
	if repositoryID != nil {
		id, ok := asInt(data["repository_id"])
		if !ok || id != *repositoryID {
			return "", errors.New("repository identity changed; select it again")
		}
	}
	expiresText, _ := data["expires_at"].(string)
	expires, err := parseExpiry(expiresText)
	if err != nil {
		return "", errors.New("invalid installation expiry")
	}
	now := c.now()
	if !(now+30 < expires && expires <= now+3700) {
		return "", errors.New("invalid installation expiry")
	}
	c.Redactor.Register(token)
	return "Bearer " + token, nil
}

// Repositories lists the installation's owned repositories through the broker.
func (c *GitHubAppCredentials) Repositories(page int64) (map[string]any, error) {
	if page < 1 || page > 10000 {
		return nil, errors.New("invalid repository page")
	}
	out, err := c.run(mustJSON(map[string]any{"action": "repositories", "page": page}))
	if err != nil {
		return nil, err
	}
	if len(out) > 65536 {
		return nil, errors.New("oversized repository listing")
	}
	var data map[string]any
	if err = json.Unmarshal(out, &data); err != nil {
		return nil, err
	}
	appID, _ := asInt(data["app_id"])
	if lowerString(data["owner"]) != strings.ToLower(c.Owner) || appID != c.AppID {
		return nil, errors.New("unexpected repository account")
	}
	repos, ok := data["repositories"].([]any)
	if !ok {
		return nil, errors.New("invalid repository listing")
	}
	for _, item := range repos {
		repo, _ := item.(map[string]any)
		name, _ := repo["full_name"].(string)
		id, idOK := asInt(repo["id"])
		if !repositoryShape.MatchString(name) || strings.ToLower(strings.SplitN(name, "/", 2)[0]) != strings.ToLower(c.Owner) || !idOK || id <= 0 {
			return nil, errors.New("invalid repository listing")
		}
	}
	return data, nil
}

func (c *GitHubAppCredentials) Snapshot() map[string]any {
	return map[string]any{"configured": true, "app_id": c.AppID, "owner": c.Owner, "identity": "installation",
		"credential_source": "private_host_broker", "manual_token_required": false}
}

// LoadGitHubAppConfig reads the private broker configuration file.
func LoadGitHubAppConfig(path string, redactor *Redactor, clock Clock) (*GitHubAppCredentials, error) {
	raw, err := openPrivate(path, 16384, "credential configuration")
	if err != nil {
		if strings.Contains(err.Error(), "must be private") {
			return nil, errors.New("credential configuration must be a private regular file")
		}
		return nil, err
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil || data == nil {
		return nil, errors.New("invalid broker configuration")
	}
	command, _ := data["command"].([]any)
	owner, _ := data["owner"].(string)
	appID, appOK := asInt(data["app_id"])
	if !sameKeys(data, "command", "owner", "app_id") || len(command) == 0 || !ownerShape.MatchString(owner) || !appOK || appID <= 0 {
		return nil, errors.New("invalid broker configuration")
	}
	argv := make([]string, 0, len(command))
	for _, item := range command {
		s, ok := item.(string)
		if !ok || s == "" || strings.ContainsRune(s, 0) {
			return nil, errors.New("invalid broker configuration")
		}
		argv = append(argv, s)
	}
	return NewGitHubAppCredentials(argv, owner, appID, redactor, clock), nil
}
