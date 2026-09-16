package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Path, URL, User, Password, AdminToken, ReadToken, IngestToken string
	MaxBytes                                                      int64
	BrowserRole                                                   func(*http.Request) string
}
type API struct {
	Store     *Store
	CH        *ClickHouse
	config    Config
	mux       *http.ServeMux
	slots     chan struct{}
	mu        sync.RWMutex
	ready     bool
	lastError string
}

func New(c Config) (*API, error) {
	for _, token := range []string{c.AdminToken, c.ReadToken, c.IngestToken} {
		if len(token) < 32 {
			return nil, fmt.Errorf("all OCSF tokens must be at least 32 characters")
		}
	}
	if c.AdminToken == c.ReadToken || c.AdminToken == c.IngestToken || c.ReadToken == c.IngestToken {
		return nil, fmt.Errorf("OCSF tokens must be distinct")
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid ClickHouse URL")
	}
	if c.MaxBytes < 0 {
		return nil, fmt.Errorf("queue capacity must be positive")
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 512 << 20
	}
	store, err := OpenStore(c.Path, c.MaxBytes)
	if err != nil {
		return nil, err
	}
	a := &API{Store: store, CH: &ClickHouse{URL: c.URL, User: c.User, Password: c.Password, Client: &http.Client{Timeout: 20 * time.Second}}, config: c, mux: http.NewServeMux(), slots: make(chan struct{}, 4)}
	a.mux.HandleFunc("POST /api/v1/events", a.ingest)
	a.mux.HandleFunc("GET /api/v1/events", a.search)
	a.mux.HandleFunc("GET /api/v1/status", a.status)
	a.mux.HandleFunc("GET /api/v1/batches", a.list)
	a.mux.HandleFunc("GET /api/v1/batches/{id}", a.batch)
	a.mux.HandleFunc("POST /api/v1/batches/{id}/replay", a.replay)
	a.mux.HandleFunc("GET /api/v1/backup", a.backup)
	return a, nil
}
func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	send(w, status, map[string]string{"error": message})
}
func tokenEqual(a, b string) bool {
	x := sha256.Sum256([]byte(a))
	y := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func (a *API) role(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" && a.config.BrowserRole != nil {
		return a.config.BrowserRole(r)
	}
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if tokenEqual(token, a.config.AdminToken) {
		return "admin"
	}
	if tokenEqual(token, a.config.ReadToken) {
		return "read"
	}
	if tokenEqual(token, a.config.IngestToken) {
		return "ingest"
	}
	return ""
}
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Google browser sessions enforce Origin and CSRF checks in BrowserRole.
	// Producers and operations keep explicit bearer credentials.
	role := a.role(r)
	if role == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, "sign in with Google or supply a valid API token")
		return
	}
	ingest := r.Method == "POST" && r.URL.Path == "/api/v1/events"
	if role == "ingest" && !ingest {
		fail(w, 403, "this token can only ingest events")
		return
	}
	if role == "read" && r.Method != "GET" {
		fail(w, 403, "read-only token")
		return
	}
	if (strings.Contains(r.URL.Path, "/batches") || r.URL.Path == "/api/v1/backup") && role != "admin" {
		fail(w, 403, "administrator token required")
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		w.Header().Set("Retry-After", "2")
		fail(w, 429, "server is busy; retry later")
		return
	}
	a.mux.ServeHTTP(w, r)
}

var streamPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func (a *API) ingest(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 {
		fail(w, 400, "Idempotency-Key header required (1–128 bytes)")
		return
	}
	stream := r.Header.Get("X-OCSF-Source")
	if stream == "" {
		stream = "default"
	}
	if !streamPattern.MatchString(stream) {
		fail(w, 400, "invalid X-OCSF-Source (use letters, numbers, dot, dash, underscore)")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && contentType != "application/x-ndjson" && contentType != "application/ndjson") {
		fail(w, 415, "use application/json or application/x-ndjson")
		return
	}
	if r.Header.Get("Content-Encoding") != "" {
		fail(w, 415, "compressed request bodies are not supported")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "request exceeds 10 MiB")
		} else {
			fail(w, 400, "could not read request")
		}
		return
	}
	random := make([]byte, 8)
	if _, err = rand.Read(random); err != nil {
		fail(w, 500, "could not allocate batch ID")
		return
	}
	id := fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(random))
	b, err := parse(body, contentType, id, stream, time.Now())
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	identity := digest([]byte(a.role(r) + "\x00" + stream + "\x00" + key))
	hash := digest(append([]byte(contentType+"\x00"), body...))
	receipt, duplicate, err := a.Store.Put(b, identity, hash)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			fail(w, 409, err.Error())
		} else if errors.Is(err, ErrFull) {
			w.Header().Set("Retry-After", "30")
			fail(w, 429, err.Error())
		} else {
			slog.Error("queue commit failed", "error", err)
			fail(w, 503, "durable queue unavailable; retry the same idempotency key")
		}
		return
	}
	problems := make([]Rejection, 0, min(100, len(receipt.Rejections)))
	for i, p := range receipt.Rejections {
		if i >= 100 {
			break
		}
		p.Raw = nil
		problems = append(problems, p)
	}
	w.Header().Set("Location", "/api/v1/batches/"+receipt.ID)
	send(w, 202, map[string]any{"id": receipt.ID, "state": receipt.State, "accepted": receipt.Accepted, "rejected": receipt.Rejected, "duplicate": duplicate, "problems": problems})
}
func (a *API) search(w http.ResponseWriter, r *http.Request) {
	if _, _, _, _, err := searchSQL(r.URL.Query()); err != nil {
		fail(w, 400, err.Error())
		return
	}
	result, err := a.CH.Search(r.Context(), r.URL.Query())
	if err != nil {
		fail(w, 503, "event search unavailable; queued ingestion remains durable")
		return
	}
	send(w, 200, result)
}
func (a *API) status(w http.ResponseWriter, r *http.Request) {
	stats, err := a.Store.Stats()
	if err != nil {
		fail(w, 503, "queue unavailable")
		return
	}
	a.mu.RLock()
	ready, last := a.ready, a.lastError
	a.mu.RUnlock()
	send(w, 200, map[string]any{"queue": stats, "storage_ready": ready, "last_error": last, "role": a.role(r), "retention_days": 30, "receipt_retention_days": 7, "max_records": MaxRecords, "max_body_bytes": MaxBody})
}
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Store.List(100)
	if err != nil {
		fail(w, 503, "queue unavailable")
		return
	}
	send(w, 200, map[string]any{"batches": rows})
}
func (a *API) batch(w http.ResponseWriter, r *http.Request) {
	b, err := a.Store.Get(r.PathValue("id"))
	if err != nil {
		fail(w, 404, "batch not found")
		return
	}
	send(w, 200, b)
}
func (a *API) replay(w http.ResponseWriter, r *http.Request) {
	err := a.Store.Update(r.PathValue("id"), func(b *Batch) error {
		if b.State != "dead_letter" {
			return fmt.Errorf("only dead-letter batches can be replayed")
		}
		b.State = "queued"
		b.NextAttempt = 0
		b.Error = ""
		return nil
	})
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	send(w, 202, map[string]string{"state": "queued"})
}
func (a *API) backup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", "attachment; filename=queue.db")
	if err := a.Store.db.View(func(tx *bolt.Tx) error { _, err := tx.WriteTo(w); return err }); err != nil {
		slog.Error("queue snapshot failed", "error", err)
	}
}
func (a *API) Ready(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	ready := a.ready
	a.mu.RUnlock()
	if !ready {
		fail(w, 503, "ClickHouse is not ready; ingestion can still queue")
		return
	}
	send(w, 200, map[string]string{"status": "ok"})
}
func (a *API) Run(ctx context.Context) {
	initialized := false
	lastCleanup := time.Time{}
	for ctx.Err() == nil {
		var err error
		if !initialized {
			err = a.CH.Init(ctx)
			initialized = err == nil
		} else {
			err = a.CH.Ping(ctx)
			if err != nil {
				initialized = false
			}
		}
		a.mu.Lock()
		a.ready = err == nil
		if err != nil {
			a.lastError = err.Error()
		} else {
			a.lastError = ""
		}
		a.mu.Unlock()
		if err == nil {
			batch, e := a.Store.Next()
			if e != nil {
				slog.Error("queue read failed", "error", e)
			} else if batch != nil {
				e = a.CH.Insert(ctx, batch.Events)
				if ctx.Err() != nil {
					return
				}
				updateErr := a.Store.Update(batch.ID, func(b *Batch) error {
					b.Attempts++
					if e == nil {
						b.State = "indexed"
						b.Events = nil
						b.Error = ""
						b.NextAttempt = 0
					} else {
						b.Error = e.Error()
						b.NextAttempt = time.Now().Add(time.Duration(min(60, 1<<min(b.Attempts, 6))) * time.Second).UnixMilli()
						var d *dbError
						if errors.As(e, &d) && d.Status >= 400 && d.Status < 500 && d.Status != 408 && d.Status != 429 && d.Status != 401 && d.Status != 403 {
							b.State = "dead_letter"
						}
					}
					return nil
				})
				if updateErr != nil {
					slog.Error("queue status commit failed", "error", updateErr)
				}
				// Always use stable event IDs when a write succeeds but receipt commit fails.
			}
		}
		if time.Since(lastCleanup) > time.Hour {
			if e := a.Store.Cleanup(time.Now()); e != nil {
				slog.Error("queue cleanup failed", "error", e)
			}
			lastCleanup = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
