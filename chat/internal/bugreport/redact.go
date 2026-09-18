package bugreport

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"

	"warden/chat/internal/config"
)

// Redactor rewrites every string of a report before it is written as a
// draft (docs/bug-reporting-plan.md § Redaction), so what the review page
// shows is byte for byte what is sent:
//
//   - bearer and capability tokens, and whatever follows Authorization:,
//     token= or capability= (session= too: the launch URL carries the
//     owner capability that way) → <token>;
//   - e-mail addresses → <email>;
//   - the home directory prefix → ~;
//   - the known secret shapes sk-…, ghp_…/gho_…, github_pat_…, ya29.… →
//     <secret>;
//   - every value in Secrets (warden.json providers.*.secret and the owner
//     capability) → <secret>.
//
// Runs of 32+ hex are kept: report, chat and run ids are what the reader
// needs, and the capability is caught by value.
type Redactor struct {
	// Secrets are exact values replaced wherever they appear. Values
	// shorter than minSecret are ignored: they would match ordinary text.
	Secrets []string
	// Home is the directory prefix replaced by "~"; "" reads $HOME.
	Home string
}

const minSecret = 8

// The replacements, in the order they run.
var (
	// "Authorization: Bearer abc", "authorization=abc", with or without
	// the scheme, quotes or a trailing separator.
	reAuthorization = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)(?:(?:bearer|basic|token)\s+)?[^\s"'&,;}\]]+`)
	// A bare "Bearer <token>" outside an Authorization header.
	reBearer = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	// token=…, capability=…, session=… (and the JSON "token": "…" shape).
	reTokenParam = regexp.MustCompile(`(?i)\b((?:access_|refresh_|id_)?token|capability|session)(["']?\s*[=:]\s*["']?)[^\s"'&,;}\]]+`)
	// Known secret shapes.
	reSecretShape = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]{8,}|ya29\.[A-Za-z0-9._-]{8,})`)
	reEmail       = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
)

// NewRedactor collects the secrets warden.json names (providers.*.secret)
// and the owner capability from the state's endpoint file when there is
// one, plus any extra values the caller holds in memory (the chat service's
// own capability).
func NewRedactor(cfg config.Config, extra ...string) Redactor {
	r := Redactor{}
	p := cfg.Providers
	if p.Codex != nil {
		r.Secrets = append(r.Secrets, p.Codex.Secret)
	}
	if p.Claude != nil {
		r.Secrets = append(r.Secrets, p.Claude.Secret)
	}
	if p.GitHub != nil {
		r.Secrets = append(r.Secrets, p.GitHub.Secret)
	}
	if cfg.Paths.State != "" {
		if raw, err := os.ReadFile(cfg.OwnerTokenFile()); err == nil {
			var e struct{ Token string }
			if json.Unmarshal(raw, &e) == nil {
				r.Secrets = append(r.Secrets, e.Token)
			}
		}
	}
	r.Secrets = append(r.Secrets, extra...)
	return r
}

// secrets are the usable values, longest first so a value that contains
// another is replaced whole.
func (r Redactor) secrets() []string {
	var out []string
	for _, s := range r.Secrets {
		if len(s) >= minSecret {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func (r Redactor) home() string {
	if r.Home != "" {
		return r.Home
	}
	home, err := os.UserHomeDir()
	if err != nil || len(home) < 2 {
		return ""
	}
	return home
}

// Redact returns s with everything above replaced.
func (r Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	for _, secret := range r.secrets() {
		s = strings.ReplaceAll(s, secret, "<secret>")
	}
	s = reAuthorization.ReplaceAllString(s, "${1}<token>")
	s = reBearer.ReplaceAllString(s, "Bearer <token>")
	s = reTokenParam.ReplaceAllString(s, "${1}${2}<token>")
	s = reSecretShape.ReplaceAllString(s, "<secret>")
	s = reEmail.ReplaceAllString(s, "<email>")
	if home := r.home(); home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

// Report redacts every string of rep in place and applies the size bounds.
func (r Redactor) Report(rep *Report) {
	rep.Summary = Clip(r.Redact(rep.Summary), MaxSummary)
	rep.Description = Clip(r.Redact(rep.Description), MaxDescription)
	if rep.Error != nil {
		rep.Error.Message = r.Redact(rep.Error.Message)
		rep.Error.Stack = Clip(r.Redact(rep.Error.Stack), MaxStack)
		rep.Error.Operation = r.Redact(rep.Error.Operation)
	}
	rep.Warden.Version = r.Redact(rep.Warden.Version)
	rep.Warden.Runtime = r.Redact(rep.Warden.Runtime)
	rep.Warden.Installed.Codex = r.Redact(rep.Warden.Installed.Codex)
	rep.Warden.Installed.Claude = r.Redact(rep.Warden.Installed.Claude)
	rep.Warden.Installed.GuestArch = r.Redact(rep.Warden.Installed.GuestArch)
	rep.System.OS = r.Redact(rep.System.OS)
	rep.System.Arch = r.Redact(rep.System.Arch)
	rep.System.OSVersion = r.Redact(rep.System.OSVersion)
	for i := range rep.Logs {
		rep.Logs[i].Name = r.Redact(rep.Logs[i].Name)
		lines := rep.Logs[i].Lines
		if len(lines) > MaxLogLines {
			lines = lines[len(lines)-MaxLogLines:]
		}
		for j := range lines {
			lines[j] = r.Redact(lines[j])
		}
		rep.Logs[i].Lines = lines
	}
	if rep.Context != nil {
		rep.Context.ChatID = r.Redact(rep.Context.ChatID)
		rep.Context.RunID = r.Redact(rep.Context.RunID)
		rep.Context.Provider = r.Redact(rep.Context.Provider)
		rep.Context.Model = r.Redact(rep.Context.Model)
	}
}
