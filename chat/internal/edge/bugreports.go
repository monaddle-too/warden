package edge

// The bug-report receiver (docs/bug-reporting-plan.md, "Contract"): POST
// /api/bug-reports, unauthenticated, answered by the edge itself and never
// proxied to the chat, on the one Warden other installs report to. A
// report is one JSON document (schema 1) of at most 256 KiB; it is
// validated, stored as <dir>/<YYYY-MM-DD>/<id>.json with the time it
// arrived and a hashed source, and kept for retentionDays and at most
// maxFiles files. The owner reads them through /api/admin/bug-reports.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BugReportsConfig is the receiver's settings; nil (or Enabled false) in
// Config leaves the route answering 404.
type BugReportsConfig struct {
	Enabled bool `json:"enabled"`
	// Dir holds the reports: <edge state>/bug-reports.
	Dir string `json:"dir"`
	// RetentionDays bounds how long a report is kept; MaxPerHour bounds
	// what one source IP may send in an hour and MaxPerDay what everyone
	// may send in a day. Zero takes the plan's defaults (90, 30, 500).
	RetentionDays int `json:"retentionDays,omitempty"`
	MaxPerHour    int `json:"maxPerHour,omitempty"`
	MaxPerDay     int `json:"maxPerDay,omitempty"`
}

// Bounds of the contract.
const (
	bugReportSchema   = "1"
	bugReportMaxBody  = 256 << 10
	bugReportMaxFiles = 10000
	bugSummaryMax     = 200
	bugListLimit      = 100
	bugListLimitMax   = 500
)

var (
	bugKinds      = map[string]bool{"error": true, "user": true}
	bugComponents = map[string]bool{"install": true, "launcher": true, "chat": true, "runner": true, "policy": true, "edge": true, "tui": true, "cli": true}
	bugTriggers   = map[string]bool{"install-step": true, "service-exit": true, "panic": true, "test": true, "user": true}
)

// bugEntry is one stored report as the index knows it: enough to order,
// page and locate it without reading the file.
type bugEntry struct {
	ID         string
	ReceivedAt time.Time
	Day        string // the directory, YYYY-MM-DD
}

// bugReports is the receiver: the index of stored reports, the in-memory
// limits and the daily salt the source hash uses.
type bugReports struct {
	cfg      BugReportsConfig
	maxFiles int
	now      func() time.Time
	logf     func(string, ...any)
	mu       sync.Mutex
	entries  []bugEntry          // newest first
	byID     map[string]bugEntry // id → entry
	perIP    map[string][]time.Time
	saltDay  string
	salt     []byte
}

func newBugReports(cfg BugReportsConfig, logf func(string, ...any)) (*bugReports, error) {
	if !filepath.IsAbs(cfg.Dir) {
		return nil, errors.New("bug reports need an absolute directory")
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 90
	}
	if cfg.MaxPerHour <= 0 {
		cfg.MaxPerHour = 30
	}
	if cfg.MaxPerDay <= 0 {
		cfg.MaxPerDay = 500
	}
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return nil, err
	}
	b := &bugReports{cfg: cfg, maxFiles: bugReportMaxFiles, now: time.Now, logf: logf, byID: map[string]bugEntry{}, perIP: map[string][]time.Time{}}
	if err := b.load(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.sweepLocked()
	b.mu.Unlock()
	return b, nil
}

// load rebuilds the index from the directory: one day directory per
// date, one <id>.json per report, the file's modification time (set to
// receivedAt when written) as its order.
func (b *bugReports) load() error {
	days, err := os.ReadDir(b.cfg.Dir)
	if err != nil {
		return err
	}
	for _, day := range days {
		if !day.IsDir() || !dayShape(day.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(b.cfg.Dir, day.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			id := strings.TrimSuffix(f.Name(), ".json")
			if f.IsDir() || id == f.Name() || !idPattern.MatchString(id) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			e := bugEntry{ID: id, ReceivedAt: info.ModTime().UTC(), Day: day.Name()}
			b.entries = append(b.entries, e)
			b.byID[id] = e
		}
	}
	b.sortLocked()
	return nil
}

func dayShape(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil && len(s) == 10
}

func (b *bugReports) sortLocked() {
	sort.Slice(b.entries, func(i, j int) bool {
		if !b.entries[i].ReceivedAt.Equal(b.entries[j].ReceivedAt) {
			return b.entries[i].ReceivedAt.After(b.entries[j].ReceivedAt)
		}
		return b.entries[i].ID > b.entries[j].ID
	})
}

func (b *bugReports) path(e bugEntry) string {
	return filepath.Join(b.cfg.Dir, e.Day, e.ID+".json")
}

// source is sha256(ip + daily salt): stable within a UTC day so the
// owner can tell one install's reports apart, useless afterwards, and
// never the address itself (the salt is random and lives in memory).
func (b *bugReports) sourceLocked(ip string, now time.Time) string {
	day := now.UTC().Format("2006-01-02")
	if b.saltDay != day {
		b.salt = make([]byte, 32)
		if _, err := rand.Read(b.salt); err != nil {
			panic(err)
		}
		b.saltDay = day
	}
	sum := sha256.Sum256(append([]byte(ip), b.salt...))
	return hex.EncodeToString(sum[:])
}

// clientIP is the source address the limits and the hash key on. Behind
// the ingress (Kubernetes) or the TLS terminator (OVH) the peer is a
// private or loopback address and X-Forwarded-For's last entry is what
// that proxy saw (ingress-nginx replaces the header with it rather than
// appending, so a client cannot choose it); elsewhere the peer itself.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil && (peer.IsLoopback() || peer.IsPrivate() || peer.IsLinkLocalUnicast()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	if peer != nil {
		return peer.String()
	}
	return host
}

// validateBugReport checks a decoded report against the schema: the
// version, the enumerations, the shape of every known section and the
// bounds of the strings the console shows. Unknown fields pass through
// untouched. The error is the 400 body.
func validateBugReport(doc map[string]any) error {
	bad := errors.New
	if n, ok := doc["schema"].(json.Number); !ok || n.String() != bugReportSchema {
		return bad("unknown schema: expected 1")
	}
	str := func(key string, max int, required bool) (string, error) {
		v, present := doc[key]
		if !present || v == nil {
			if required {
				return "", bad(key + " is required")
			}
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", bad(key + " must be a string")
		}
		if len(s) > max {
			return "", bad(fmt.Sprintf("%s is longer than %d bytes", key, max))
		}
		return s, nil
	}
	kind, err := str("kind", 32, true)
	if err != nil {
		return err
	}
	if !bugKinds[kind] {
		return bad("kind must be error or user")
	}
	component, err := str("component", 32, true)
	if err != nil {
		return err
	}
	if !bugComponents[component] {
		return bad("unknown component " + strconv.Quote(component))
	}
	trigger, err := str("trigger", 32, true)
	if err != nil {
		return err
	}
	if !bugTriggers[trigger] {
		return bad("unknown trigger " + strconv.Quote(trigger))
	}
	if _, err = str("summary", bugSummaryMax, true); err != nil {
		return err
	}
	if _, err = str("description", 64<<10, false); err != nil {
		return err
	}
	if created, err := str("createdAt", 64, false); err != nil {
		return err
	} else if created != "" {
		if _, err = time.Parse(time.RFC3339, created); err != nil {
			return bad("createdAt must be RFC 3339")
		}
	}
	for _, key := range []string{"error", "warden", "system", "context"} {
		if v, present := doc[key]; present && v != nil {
			if _, ok := v.(map[string]any); !ok {
				return bad(key + " must be an object")
			}
		}
	}
	if e, ok := doc["error"].(map[string]any); ok {
		for key, max := range map[string]int{"message": 16 << 10, "stack": 128 << 10, "operation": 200} {
			if v, present := e[key]; present && v != nil {
				s, ok := v.(string)
				if !ok {
					return bad("error." + key + " must be a string")
				}
				if len(s) > max {
					return bad(fmt.Sprintf("error.%s is longer than %d bytes", key, max))
				}
			}
		}
	}
	if v, present := doc["logs"]; present && v != nil {
		logs, ok := v.([]any)
		if !ok {
			return bad("logs must be an array")
		}
		if len(logs) > 16 {
			return bad("logs has more than 16 files")
		}
		for i, item := range logs {
			file, ok := item.(map[string]any)
			if !ok {
				return bad(fmt.Sprintf("logs[%d] must be an object", i))
			}
			if name, ok := file["name"].(string); !ok || name == "" || len(name) > 200 {
				return bad(fmt.Sprintf("logs[%d].name must be a string of at most 200 bytes", i))
			}
			lines, ok := file["lines"].([]any)
			if !ok {
				return bad(fmt.Sprintf("logs[%d].lines must be an array", i))
			}
			if len(lines) > 2000 {
				return bad(fmt.Sprintf("logs[%d] has more than 2000 lines", i))
			}
			for _, l := range lines {
				if _, ok := l.(string); !ok {
					return bad(fmt.Sprintf("logs[%d].lines must hold strings", i))
				}
			}
		}
	}
	return nil
}

// receive answers POST /api/bug-reports.
func (b *bugReports) receive(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.ContentLength > bugReportMaxBody {
		http.Error(w, "bug report larger than 256 KiB", http.StatusRequestEntityTooLarge)
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, bugReportMaxBody))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "bug report larger than 256 KiB", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "malformed JSON: "+err.Error(), 400)
		return
	}
	if dec.More() {
		http.Error(w, "one JSON document expected", 400)
		return
	}
	if err := validateBugReport(doc); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	id, _ := doc["id"].(string)
	if !idPattern.MatchString(id) {
		id = random()[:32]
		doc["id"] = id
	}
	now := b.now().UTC()
	ip := clientIP(r)
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.byID[id]; ok {
		// A resend: answered as the first time, nothing stored twice.
		writeJSON(w, 202, map[string]any{"id": id, "received": e.ReceivedAt.Format(time.RFC3339)})
		return
	}
	if retry := b.limitedLocked(ip, now); retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
		http.Error(w, "too many bug reports; try again later", http.StatusTooManyRequests)
		return
	}
	doc["receivedAt"] = now.Format(time.RFC3339)
	doc["source"] = b.sourceLocked(ip, now)
	e := bugEntry{ID: id, ReceivedAt: now, Day: now.Format("2006-01-02")}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		http.Error(w, "bug report cannot be stored", 500)
		return
	}
	if err := b.writeLocked(e, raw); err != nil {
		b.logf("bug reports: %v", err)
		http.Error(w, "bug report cannot be stored", 500)
		return
	}
	b.perIP[ip] = append(b.perIP[ip], now)
	b.entries = append(b.entries, e)
	b.byID[id] = e
	b.sortLocked()
	b.sweepLocked()
	b.logf("bug report %s received (%s/%s, %s)", id, doc["kind"], doc["component"], doc["summary"])
	writeJSON(w, 202, map[string]any{"id": id, "received": e.ReceivedAt.Format(time.RFC3339)})
}

// limitedLocked applies the two limits and says how long the caller
// should wait, zero when the report may be taken.
func (b *bugReports) limitedLocked(ip string, now time.Time) time.Duration {
	hourAgo := now.Add(-time.Hour)
	for key, times := range b.perIP {
		kept := times[:0]
		for _, t := range times {
			if t.After(hourAgo) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(b.perIP, key)
		} else {
			b.perIP[key] = kept
		}
	}
	if times := b.perIP[ip]; len(times) >= b.cfg.MaxPerHour {
		return times[0].Add(time.Hour).Sub(now) + time.Second
	}
	// The daily cap counts stored files: the index survives a restart.
	dayAgo := now.Add(-24 * time.Hour)
	count := 0
	var oldest time.Time
	for _, e := range b.entries {
		if !e.ReceivedAt.After(dayAgo) {
			break
		}
		count++
		oldest = e.ReceivedAt
	}
	if count >= b.cfg.MaxPerDay {
		return oldest.Add(24*time.Hour).Sub(now) + time.Second
	}
	return 0
}

func (b *bugReports) writeLocked(e bugEntry, raw []byte) error {
	dir := filepath.Join(b.cfg.Dir, e.Day)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := b.path(e)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	// The order after a restart is the file's time.
	_ = os.Chtimes(path, e.ReceivedAt, e.ReceivedAt)
	return nil
}

// sweepLocked applies retention: day directories older than
// retentionDays go, then the oldest reports until at most maxFiles remain.
func (b *bugReports) sweepLocked() {
	now := b.now().UTC()
	cutoff := now.AddDate(0, 0, -b.cfg.RetentionDays).Format("2006-01-02")
	kept := b.entries[:0]
	for _, e := range b.entries {
		if e.Day < cutoff {
			b.removeLocked(e)
			continue
		}
		kept = append(kept, e)
	}
	b.entries = kept
	for len(b.entries) > b.maxFiles {
		last := b.entries[len(b.entries)-1]
		b.removeLocked(last)
		b.entries = b.entries[:len(b.entries)-1]
	}
	// Day directories the retention emptied (or that never held a
	// report) are removed with their files.
	if days, err := os.ReadDir(b.cfg.Dir); err == nil {
		for _, day := range days {
			if day.IsDir() && dayShape(day.Name()) && day.Name() < cutoff {
				os.RemoveAll(filepath.Join(b.cfg.Dir, day.Name()))
			}
		}
	}
}

func (b *bugReports) removeLocked(e bugEntry) {
	delete(b.byID, e.ID)
	if err := os.Remove(b.path(e)); err != nil && !errors.Is(err, os.ErrNotExist) {
		b.logf("bug reports: remove %s: %v", e.ID, err)
	}
}

// bugSummary is one row of the owner's list.
type bugSummary struct {
	ID         string `json:"id"`
	ReceivedAt string `json:"receivedAt"`
	Kind       string `json:"kind"`
	Component  string `json:"component"`
	Version    string `json:"version"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Summary    string `json:"summary"`
}

// admin answers the owner's routes under /api/admin/bug-reports; the
// caller has checked the role. rest is what follows the prefix: "" for
// the list, "/<id>" for one report.
func (b *bugReports) admin(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" || rest == "/" {
		if r.Method != "GET" {
			http.Error(w, "not found", 404)
			return
		}
		b.list(w, r)
		return
	}
	id := strings.TrimPrefix(rest, "/")
	if !idPattern.MatchString(id) {
		http.Error(w, "not found", 404)
		return
	}
	b.mu.Lock()
	e, ok := b.byID[id]
	b.mu.Unlock()
	if !ok {
		http.Error(w, "no such bug report", 404)
		return
	}
	switch r.Method {
	case "GET":
		raw, err := os.ReadFile(b.path(e))
		if err != nil {
			http.Error(w, "no such bug report", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	case "DELETE":
		b.mu.Lock()
		if _, ok := b.byID[id]; ok {
			b.removeLocked(e)
			for i, x := range b.entries {
				if x.ID == id {
					b.entries = append(b.entries[:i], b.entries[i+1:]...)
					break
				}
			}
		}
		b.mu.Unlock()
		w.WriteHeader(204)
	default:
		http.Error(w, "not found", 404)
	}
}

// list answers GET /api/admin/bug-reports?limit=&before=<id>: the newest
// first, at most limit rows (100, up to 500), those older than before
// when given.
func (b *bugReports) list(w http.ResponseWriter, r *http.Request) {
	limit := bugListLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive number", 400)
			return
		}
		limit = min(n, bugListLimitMax)
	}
	b.mu.Lock()
	start := 0
	if before := r.URL.Query().Get("before"); before != "" {
		start = -1
		for i, e := range b.entries {
			if e.ID == before {
				start = i + 1
				break
			}
		}
		if start < 0 {
			b.mu.Unlock()
			http.Error(w, "unknown before cursor", 400)
			return
		}
	}
	page := append([]bugEntry(nil), b.entries[start:min(start+limit, len(b.entries))]...)
	b.mu.Unlock()
	rows := make([]bugSummary, 0, len(page))
	for _, e := range page {
		rows = append(rows, b.summary(e))
	}
	writeJSON(w, 200, rows)
}

// summary reads the fields the list shows from one stored report; a file
// that cannot be read still lists by what the index knows.
func (b *bugReports) summary(e bugEntry) bugSummary {
	row := bugSummary{ID: e.ID, ReceivedAt: e.ReceivedAt.Format(time.RFC3339)}
	raw, err := os.ReadFile(b.path(e))
	if err != nil {
		return row
	}
	var doc struct {
		Kind      string `json:"kind"`
		Component string `json:"component"`
		Summary   string `json:"summary"`
		Warden    struct {
			Version string `json:"version"`
		} `json:"warden"`
		System struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"system"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return row
	}
	row.Kind, row.Component, row.Summary = doc.Kind, doc.Component, doc.Summary
	row.Version, row.OS, row.Arch = doc.Warden.Version, doc.System.OS, doc.System.Arch
	return row
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
