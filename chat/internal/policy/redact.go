package policy

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var githubSuffixes = []string{"github.com", "githubusercontent.com", "githubassets.com", "github.io", "githubapp.com", "github.dev", "githubpreview.dev", "ghcr.io", "githubstatus.com", "github.blog", "githubcopilot.com", "copilot.github.com"}

var sensitiveKey = regexp.MustCompile(`(?i)authorization|cookie|password|passwd|secret|token|api.?key|credential|private.?key|signature|^sig$|^key$`)

var tokenPattern = regexp.MustCompile(`(?i)(?:gh[pousr]_[A-Za-z0-9_]{15,}|github_pat_[A-Za-z0-9_]{15,}|sk-[A-Za-z0-9_-]{15,}|(?:Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+|-----BEGIN [^-]*PRIVATE KEY-----[\s\S]*?-----END [^-]*PRIVATE KEY-----)`)

var safeHeaders = map[string]bool{"accept": true, "content-type": true, "content-length": true, "user-agent": true, "x-github-api-version": true, "host": true, "date": true, "server": true, "x-github-request-id": true, "etag": true, "last-modified": true, "cache-control": true, "content-encoding": true, "connection": true}

// GitHubHost reports whether a hostname belongs to GitHub's protected suffixes.
func GitHubHost(host string) bool {
	host = strings.TrimRight(strings.ToLower(host), ".")
	for _, suffix := range githubSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// Redactor removes registered secrets and token-shaped strings from text
// that may reach audit records, responses or logs.
type Redactor struct {
	mu      sync.RWMutex
	secrets map[string]bool
}

func NewRedactor() *Redactor { return &Redactor{secrets: map[string]bool{}} }

func (r *Redactor) Register(value string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets[value] = true
	r.secrets[base64.StdEncoding.EncodeToString([]byte(value))] = true
}

// Secrets returns the registered values, longest first.
func (r *Redactor) Secrets() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.secrets))
	for s := range r.secrets {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

func (r *Redactor) Text(value string) string {
	for _, secret := range r.Secrets() {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return tokenPattern.ReplaceAllString(value, "[REDACTED]")
}

// Clean redacts sensitive keys and secret text throughout a generic value.
func (r *Redactor) Clean(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if sensitiveKey.MatchString(k) && k != "token_configured" && k != "credential" {
				out[r.Text(k)] = "[REDACTED]"
			} else {
				out[r.Text(k)] = r.Clean(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = r.Clean(item)
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = r.Text(item)
		}
		return out
	case [][]string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = r.Clean(item)
		}
		return out
	case string:
		return r.Text(v)
	}
	return value
}

// Headers keeps only known-safe header values, redacting everything else.
func (r *Redactor) Headers(pairs [][]string) [][]string {
	out := make([][]string, 0, len(pairs))
	for _, pair := range pairs {
		if len(pair) != 2 {
			continue
		}
		if safeHeaders[strings.ToLower(pair[0])] {
			out = append(out, []string{pair[0], r.Text(pair[1])})
		} else {
			out = append(out, []string{pair[0], "[REDACTED]"})
		}
	}
	return out
}

// Body describes a body for audit without retaining it.
func (r *Redactor) Body(size int) map[string]any {
	return map[string]any{"bytes": size, "capture": "omitted_policy"}
}
