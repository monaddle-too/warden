package policy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Clock returns wall-clock seconds; tests substitute fixed values.
type Clock func() float64

func wallClock() float64 { return float64(time.Now().UnixNano()) / 1e9 }

var monotonicStart = time.Now()

func monotonicClock() float64 { return time.Since(monotonicStart).Seconds() }

// ProviderSource supplies the upstream route and credential headers for a
// model provider. Implementations read host-owned logins without copying or
// refreshing them; expired logins fail closed.
type ProviderSource interface {
	Available() bool
	Route(apiPath string) (host, path string, err error)
	Headers() (map[string]string, error)
}

// openPrivate opens a regular, owner-only file without following a final
// symlink and returns at most limit+1 bytes.
func openPrivate(path string, limit int64, description string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, _ := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || stat == nil || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 || info.Size() > limit {
		return nil, errors.New(description + " must be private and owned")
	}
	return io.ReadAll(io.LimitReader(file, limit+1))
}

var jwtShape = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
var accountShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)

// CodexCredentials reads an existing host-owned Codex login from a
// CredentialStore: Store and Name when set, else the private file at Path.
type CodexCredentials struct {
	Path  string
	Store CredentialStore
	Name  string
	Clock Clock
}

// loadCredential reads a login through its store: the given store and
// name, or the file store at path with the loader's limit and description.
func loadCredential(store CredentialStore, name, path string, limit int64, description string) ([]byte, error) {
	if store == nil {
		store, name = FileCredentials{Limit: limit, Description: description}, path
	}
	return store.Load(context.Background(), name)
}

func (c *CodexCredentials) clock() float64 {
	if c.Clock != nil {
		return c.Clock()
	}
	return wallClock()
}

func (c *CodexCredentials) read() (token, account string, err error) {
	raw, err := loadCredential(c.Store, c.Name, c.Path, 1024*1024, "Codex credential file")
	if err != nil {
		return "", "", err
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil || data == nil {
		return "", "", errors.New("Invalid host credential document")
	}
	tokens, ok := data["tokens"].(map[string]any)
	if data["auth_mode"] != "chatgpt" || !ok {
		return "", "", errors.New("A host ChatGPT login is required")
	}
	token, _ = tokens["access_token"].(string)
	account, _ = tokens["account_id"].(string)
	if token == "" || len(token) > 65536 || !jwtShape.MatchString(token) || !accountShape.MatchString(account) {
		return "", "", errors.New("Invalid host ChatGPT credential shape")
	}
	payload := strings.Split(token, ".")[1]
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(payload, "="))
	if err != nil {
		return "", "", errors.New("Invalid host credential claims")
	}
	var claims map[string]any
	if err = json.Unmarshal(decoded, &claims); err != nil || claims == nil {
		return "", "", errors.New("Invalid host credential claims")
	}
	// JWT signature verification belongs to the fixed TLS upstream. This
	// local expiry check only refuses stale credentials before disclosure.
	expiry, ok := asNumber(claims["exp"])
	if !ok || expiry <= c.clock()+30 {
		return "", "", errors.New("Refresh the host Codex sign-in before running a chat")
	}
	return token, account, nil
}

func (c *CodexCredentials) Available() bool {
	_, _, err := c.read()
	return err == nil
}

func (c *CodexCredentials) Route(apiPath string) (string, string, error) {
	if apiPath != "/v1/responses" {
		return "", "", errors.New("This host login supports only Codex Responses")
	}
	return "chatgpt.com", "/backend-api/codex/responses", nil
}

func (c *CodexCredentials) Headers() (map[string]string, error) {
	token, account, err := c.read()
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + token, "ChatGPT-Account-ID": account}, nil
}

// boundedClass reports whether s has min..max bytes all matching class.
func boundedClass(s string, min, max int, class func(byte) bool) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !class(s[i]) {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func claudeTokenShape(s string) bool {
	return boundedClass(s, 16, 4096, func(c byte) bool { return isAlnum(c) || c == '_' || c == '.' || c == '-' })
}

// ClaudeCredentials reads a host-owned Claude Code login (Store and Name,
// else the private file at Path); only the access token reaches the
// gateway.
type ClaudeCredentials struct {
	Path  string
	Store CredentialStore
	Name  string
	Clock Clock
}

func (c *ClaudeCredentials) clock() float64 {
	if c.Clock != nil {
		return c.Clock()
	}
	return wallClock()
}

func (c *ClaudeCredentials) read() (string, error) {
	raw, err := loadCredential(c.Store, c.Name, c.Path, 1024*1024, "Claude credential file")
	if err != nil {
		return "", err
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil || data == nil {
		return "", errors.New("invalid Claude credential document")
	}
	oauth, ok := data["claudeAiOauth"].(map[string]any)
	if _, present := data["claudeAiOauth"]; present && !ok {
		return "", errors.New("invalid Claude credential document")
	}
	token, _ := oauth["accessToken"].(string)
	expiry, numeric := asNumber(oauth["expiresAt"])
	lower := strings.ToLower(token)
	if !claudeTokenShape(token) || strings.Contains(lower, "proxy") || strings.Contains(lower, "placeholder") || !numeric || expiry/1000 <= c.clock()+30 {
		return "", errors.New("Refresh the Claude sign-in before running a chat")
	}
	return token, nil
}

func (c *ClaudeCredentials) Available() bool {
	_, err := c.read()
	return err == nil
}

func (c *ClaudeCredentials) Route(apiPath string) (string, string, error) {
	if apiPath != "/v1/messages" && apiPath != "/v1/messages/count_tokens" {
		return "", "", errors.New("unsupported Claude route")
	}
	return "api.anthropic.com", apiPath, nil
}

func (c *ClaudeCredentials) Headers() (map[string]string, error) {
	token, err := c.read()
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + token}, nil
}
