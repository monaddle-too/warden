// Package bugreport is the client side of Warden's bug reporting
// (docs/bug-reporting-plan.md): the report schema the cloud receiver
// accepts, what a draft gathers about the build and the host, the
// redaction every string passes before a draft touches the disk, the
// pending drafts under <state>/bug-reports/pending, the standalone review
// page a person decides on ("Send report" / "Don't send") and the send
// itself. Nothing here ever includes chat content: a report carries ids.
package bugreport

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"warden/chat/internal/hostinfo"
	"warden/chat/internal/release"
)

// Schema is the report schema this package writes; the receiver refuses
// any other.
const Schema = 1

// Report kinds.
const (
	KindError = "error" // something broke; drafted automatically
	KindUser  = "user"  // a person wrote it
)

// Triggers: what produced the report.
const (
	TriggerInstallStep = "install-step"
	TriggerServiceExit = "service-exit"
	TriggerPanic       = "panic"
	TriggerTest        = "test"
	TriggerUser        = "user"
)

// Components: the process the report is about.
const (
	ComponentInstall  = "install"
	ComponentLauncher = "launcher"
	ComponentChat     = "chat"
	ComponentRunner   = "runner"
	ComponentPolicy   = "policy"
	ComponentEdge     = "edge"
	ComponentTUI      = "tui"
	ComponentCLI      = "cli"
)

// Bounds. MaxBody is the receiver's limit on a report; the others keep a
// draft well under it with two log tails and a stack.
const (
	MaxBody        = 256 << 10
	MaxSummary     = 200
	MaxDescription = 16 << 10
	MaxStack       = 32 << 10
	MaxLogLines    = 300
	MaxLogBytes    = 64 << 10
)

// Report is one bug report, schema 1 (docs/bug-reporting-plan.md § Report
// schema). Every string in it has been through Redactor.Report by the time
// it is written as a draft, so the review page shows exactly what is sent.
type Report struct {
	Schema      int      `json:"schema"`
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	CreatedAt   string   `json:"createdAt"`
	Component   string   `json:"component"`
	Trigger     string   `json:"trigger"`
	Summary     string   `json:"summary"`
	Description string   `json:"description,omitempty"`
	Error       *Error   `json:"error,omitempty"`
	Warden      Warden   `json:"warden"`
	System      System   `json:"system"`
	Logs        []Log    `json:"logs,omitempty"`
	Context     *Context `json:"context,omitempty"`
}

// Error is what broke: the message, the stack when there is one, and the
// operation (an install step, a route, a runner op) it broke in.
type Error struct {
	Message   string `json:"message"`
	Stack     string `json:"stack,omitempty"`
	Operation string `json:"operation,omitempty"`
}

// Warden is the build and the installation the report comes from.
type Warden struct {
	Version   string    `json:"version"`
	Protocol  int       `json:"protocol"`
	Runtime   string    `json:"runtime"`
	Installed Installed `json:"installed"`
}

// Installed are the pinned runtimes install.json records.
type Installed struct {
	Codex     string `json:"codex,omitempty"`
	Claude    string `json:"claude,omitempty"`
	GuestArch string `json:"guestArch,omitempty"`
}

// System is the host.
type System struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	OSVersion string `json:"osVersion,omitempty"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int    `json:"memoryMB"`
}

// Log is the tail of one log file.
type Log struct {
	Name  string   `json:"name"`
	Lines []string `json:"lines"`
}

// Context names the chat a user report was written from: ids only, never
// a title or a message.
type Context struct {
	ChatID   string `json:"chatID,omitempty"`
	RunID    string `json:"runID,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// NewID is a fresh report id: 32 hex, the shape the receiver keeps.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ValidID reports whether id is a report id (32 lower-case hex).
func ValidID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Summarize clips text to one line of at most MaxSummary characters.
func Summarize(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	return Clip(line, MaxSummary)
}

// Clip keeps at most n runes of s, with an ellipsis when it cut.
func Clip(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n == 1 {
		return string(r[:1])
	}
	return string(r[:n-1]) + "…"
}

// SystemInfo reads this host: the Go platform, the kernel release, cores
// and memory.
func SystemInfo() System {
	memoryMB, cores := hostinfo.Capacity()
	return System{OS: runtime.GOOS, Arch: runtime.GOARCH, OSVersion: osVersion(), CPUs: cores, MemoryMB: memoryMB}
}

// WardenInfo reads the build (the linked revision and protocol), the
// runtime kind and the pinned runtimes from <state>/install.json when the
// state has one (the sbx shapes; a Kubernetes pod has none).
func WardenInfo(state, runtimeKind string) Warden {
	w := Warden{Version: release.Revision, Protocol: release.Protocol, Runtime: runtimeKind}
	if raw, err := os.ReadFile(filepath.Join(state, "install.json")); err == nil {
		var rec struct {
			Codex     string `json:"codex"`
			Claude    string `json:"claude"`
			GuestArch string `json:"guestArch"`
		}
		if json.Unmarshal(raw, &rec) == nil {
			w.Installed = Installed{Codex: rec.Codex, Claude: rec.Claude, GuestArch: rec.GuestArch}
		}
	}
	return w
}

// Tail is the end of the file at path as a Log named after it: at most
// MaxLogBytes from the end, whole lines, at most MaxLogLines of them. A
// file that cannot be read is an empty log with one line saying so, so
// the report still names what it tried to include.
func Tail(path string) Log {
	l := Log{Name: filepath.Base(path)}
	lines, err := tailLines(path, MaxLogBytes, MaxLogLines)
	if err != nil {
		l.Lines = []string{"(" + err.Error() + ")"}
		return l
	}
	l.Lines = lines
	return l
}

// TailText is the tail of text in memory (an installer's own output) under
// the same bounds as a file's.
func TailText(text string) []string {
	if len(text) > MaxLogBytes {
		text = text[len(text)-MaxLogBytes:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return lastLines(text, MaxLogLines)
}

func tailLines(path string, maxBytes int64, maxLines int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New(path + " is a directory")
	}
	start := int64(0)
	partial := false
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
		partial = true
	}
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if partial {
		// Drop the line the byte cut landed in.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return lastLines(string(data), maxLines), nil
}

func lastLines(text string, n int) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return []string{}
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(strings.ToValidUTF8(l, "�"), "\r")
	}
	return lines
}
