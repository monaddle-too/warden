package bugreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"warden/chat/internal/config"
)

// Capturer writes drafts for one component. A draft is written only while
// reporting is enabled (config reporting.enabled, re-read from warden.json
// at every capture so `warden bugs on` takes effect in running services);
// disabled means no file, no page, nothing (plan decision 5).
type Capturer struct {
	// State is Warden's state directory; drafts go under it.
	State string
	// ConfigPath is warden.json, re-read for the reporting section at each
	// capture; "" keeps Reporting as given.
	ConfigPath string
	// Reporting is the section at construction, and the fallback when the
	// file cannot be re-read.
	Reporting config.Reporting
	// Component names the process (chat, runner, …).
	Component string
	// Runtime is the runtime kind the installation runs (sbx, kubernetes).
	Runtime string
	// Redactor rewrites every string before the draft is written.
	Redactor Redactor
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logf is where captures are logged; nil is the standard logger.
	Logf func(format string, args ...any)
}

// New builds the capturer of a component from its configuration; extra are
// secret values the process holds in memory (the chat's own capability).
func New(cfg config.Config, configPath, component string, extra ...string) *Capturer {
	return &Capturer{State: cfg.Paths.State, ConfigPath: configPath, Reporting: cfg.Reporting, Component: component, Runtime: cfg.RuntimeKind(), Redactor: NewRedactor(cfg, extra...)}
}

// Settings is the reporting section as it stands now.
func (c *Capturer) Settings() config.Reporting {
	if c == nil {
		return config.Reporting{}
	}
	if c.ConfigPath != "" {
		if cfg, err := config.Load(c.ConfigPath, ""); err == nil {
			return cfg.Reporting
		}
	}
	return c.Reporting
}

// Enabled reports whether drafts are written at all.
func (c *Capturer) Enabled() bool { return c != nil && c.Settings().Enabled }

// OffNotice is what a surface says when reporting is off.
const OffNotice = "Bug reporting is off — `warden bugs on` to enable it"

// DraftedNotice is what a surface says once a draft is written and a
// launcher will present it.
const DraftedNotice = "Bug report drafted — review it in the window that opened (or `warden bugs pending`)"

func (c *Capturer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Capturer) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Draft is a new report of this component with the build and host filled
// in; the caller adds the error, logs and context, then Capture writes it.
func (c *Capturer) Draft(kind, trigger, summary string) Report {
	return Report{Schema: Schema, ID: NewID(), Kind: kind, CreatedAt: c.now().UTC().Format(time.RFC3339), Component: c.Component, Trigger: trigger, Summary: Summarize(summary), Warden: WardenInfo(c.State, c.Runtime), System: SystemInfo()}
}

// AddLog appends the tail of the file at path to the report.
func (c *Capturer) AddLog(r *Report, path string) { r.Logs = append(r.Logs, Tail(path)) }

// AddServiceLog appends the tail of <state>/<name>.log when the file
// exists (the launcher writes one per service; the launcher's own,
// warden.log, exists only when it runs detached).
func (c *Capturer) AddServiceLog(r *Report, name string) {
	path := filepath.Join(c.State, name+".log")
	if _, err := os.Stat(path); err != nil {
		return
	}
	c.AddLog(r, path)
}

// PendingDir is where a state's undecided drafts live.
func PendingDir(state string) string { return filepath.Join(state, "bug-reports", "pending") }

// Capture redacts the report and writes it as a pending draft. It returns
// the draft's path and true when written; false with no error when
// reporting is disabled.
func (c *Capturer) Capture(r Report) (string, bool, error) {
	if c == nil || !c.Enabled() {
		return "", false, nil
	}
	if r.Schema == 0 {
		r.Schema = Schema
	}
	if !ValidID(r.ID) {
		r.ID = NewID()
	}
	if r.CreatedAt == "" {
		r.CreatedAt = c.now().UTC().Format(time.RFC3339)
	}
	if r.Component == "" {
		r.Component = c.Component
	}
	c.Redactor.Report(&r)
	dir := PendingDir(c.State)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	path := filepath.Join(dir, r.ID+".json")
	if err := Write(path, r); err != nil {
		return "", false, err
	}
	c.logf("bug report %s drafted (%s/%s): %s", r.ID, r.Component, r.Trigger, r.Summary)
	return path, true, nil
}

// CapturePanic writes a draft for a recovered panic: the value, the stack
// and the operation it happened in.
func (c *Capturer) CapturePanic(trigger, operation string, value any, stack []byte) (string, bool, error) {
	if c == nil || !c.Enabled() {
		return "", false, nil
	}
	message := fmt.Sprint(value)
	r := c.Draft(KindError, trigger, "panic in "+operation+": "+Summarize(message))
	r.Error = &Error{Message: message, Stack: string(stack), Operation: operation}
	c.AddServiceLog(&r, "warden-"+c.Component)
	if c.Component != ComponentLauncher {
		c.AddServiceLog(&r, "warden")
	}
	return c.Capture(r)
}

// Recover is deferred at the entry of a goroutine or a handler: a panic
// there is captured as a draft and raised again, so the process fails
// exactly as it did before (net/http logs it and closes the connection; a
// worker or run goroutine takes the process down). With a nil capturer
// nothing is touched.
func (c *Capturer) Recover(operation string) {
	if c == nil {
		return
	}
	v := recover()
	if v == nil {
		return
	}
	if v == http.ErrAbortHandler {
		panic(v)
	}
	c.capturePanicSafely(TriggerPanic, operation, v)
	panic(v)
}

// Trap is Recover for a deliberate panic (/test bugreporting): captured as
// a draft with the given trigger and logged, never raised again.
func (c *Capturer) Trap(trigger, operation string) {
	if c == nil {
		return
	}
	v := recover()
	if v == nil {
		return
	}
	c.capturePanicSafely(trigger, operation, v)
	c.logf("recovered panic in %s: %v", operation, v)
}

func (c *Capturer) capturePanicSafely(trigger, operation string, v any) {
	defer func() {
		if again := recover(); again != nil {
			c.logf("bug report of a panic in %s failed: %v", operation, again)
		}
	}()
	if _, _, err := c.CapturePanic(trigger, operation, v, debug.Stack()); err != nil {
		c.logf("bug report of a panic in %s not written: %v", operation, err)
	}
}

// Handler wraps an HTTP handler so a panic in it is captured (then raised
// again for net/http to log as it does today).
func (c *Capturer) Handler(next http.Handler) http.Handler {
	if c == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer c.Recover(r.Method + " " + r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// The process-wide capturer, for goroutine entry points deep in packages
// that have no configuration of their own (the policy control loop, the
// gateways, the runner's connections). Unset, every guard is a no-op.
var defaultCapturer atomic.Pointer[Capturer]

// SetDefault installs c as the process-wide capturer; nil removes it.
func SetDefault(c *Capturer) { defaultCapturer.Store(c) }

// Default is the process-wide capturer, or nil.
func Default() *Capturer { return defaultCapturer.Load() }

// Recover is Default().Recover: deferred at a goroutine's entry.
func Recover(operation string) {
	c := Default()
	if c == nil {
		return
	}
	v := recover()
	if v == nil {
		return
	}
	if v == http.ErrAbortHandler {
		panic(v)
	}
	c.capturePanicSafely(TriggerPanic, operation, v)
	panic(v)
}

// Handler is Default().Handler at request time, so a capturer installed
// after the server was built still guards it.
func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer Recover(r.Method + " " + r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// Draft is a pending report on disk.
type Draft struct {
	Path   string
	Report Report
}

// Pending lists the state's undecided drafts, oldest first. A file that is
// not a report is skipped.
func Pending(state string) ([]Draft, error) {
	entries, err := os.ReadDir(PendingDir(state))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Draft
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(PendingDir(state), e.Name())
		r, err := Read(path)
		if err != nil {
			continue
		}
		out = append(out, Draft{Path: path, Report: r})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Report.CreatedAt != out[j].Report.CreatedAt {
			return out[i].Report.CreatedAt < out[j].Report.CreatedAt
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Read loads one draft.
func Read(path string) (Report, error) {
	var r Report
	raw, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	if r.Schema != Schema || !ValidID(r.ID) {
		return r, fmt.Errorf("%s is not a bug report draft", path)
	}
	return r, nil
}

// Write stores a report at path, owner-only, through a temporary file.
func Write(path string, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err = os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Discard deletes a draft; a draft already gone is not an error.
func Discard(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Recent reports whether a pending draft of component with the trigger was
// written within window of now: the launcher skips a service-exit draft
// when the service itself just drafted the panic that took it down.
func Recent(state, component, trigger string, now time.Time, window time.Duration) bool {
	drafts, _ := Pending(state)
	for _, d := range drafts {
		if d.Report.Component != component || d.Report.Trigger != trigger {
			continue
		}
		at, err := time.Parse(time.RFC3339, d.Report.CreatedAt)
		if err == nil && now.Sub(at) >= 0 && now.Sub(at) <= window {
			return true
		}
	}
	return false
}
