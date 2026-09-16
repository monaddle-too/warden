package policy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UUID4 returns a random RFC 4122 version 4 identifier.
func UUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Audit appends hash-chained, redacted JSON events. Emission is synchronous
// and durable: no permit is returned before the record is on disk.
type Audit struct {
	Path     string
	redactor *Redactor
	mu       sync.Mutex
	file     *os.File
	instance string
	seq      int64
	previous string
	// Hook lets tests simulate storage failure.
	failWith error
}

func NewAudit(path string, redactor *Redactor) (*Audit, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &Audit{Path: path, redactor: redactor, file: file, instance: UUID4(), previous: strings.Repeat("0", 64)}, nil
}

func (a *Audit) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.file = nil
	return err
}

// Emit records an event and returns it. Fields are cleared of bodies and
// secrets before hashing, so persisted records never contain either.
func (a *Audit) Emit(eventType string, fields map[string]any) (map[string]any, error) {
	return a.EmitSeverity(eventType, "info", fields)
}

func (a *Audit) EmitSeverity(eventType, severity string, fields map[string]any) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failWith != nil {
		return nil, a.failWith
	}
	if a.file == nil {
		return nil, errors.New("audit closed")
	}
	event := map[string]any{
		"schema_version": "1.0.0",
		"event_id":       UUID4(),
		"time":           time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"event_type":     eventType,
		"severity":       severity,
		"producer":       map[string]any{"name": "warden", "version": "0.1.0", "instance_id": a.instance},
		"sequence":       a.seq + 1,
		"previous_hash":  a.previous,
	}
	cleaned, _ := a.redactor.Clean(WithoutBodies(cloneJSON(fields))).(map[string]any)
	for k, v := range cleaned {
		event[k] = v
	}
	encoded, err := DumpsErr(event)
	if err != nil {
		return nil, err
	}
	event["event_hash"] = sha256Hex([]byte(encoded))
	line, err := DumpsErr(event)
	if err != nil {
		return nil, err
	}
	if _, err = a.file.WriteString(line + "\n"); err != nil {
		return nil, err
	}
	if err = a.file.Sync(); err != nil {
		return nil, err
	}
	a.seq++
	a.previous = event["event_hash"].(string)
	return event, nil
}

// WithoutBodies enforces metadata-only retention on audit fields.
func WithoutBodies(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = WithoutBodies(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			switch k {
			case "body":
				body := map[string]any{"capture": "omitted_policy"}
				if m, ok := item.(map[string]any); ok {
					if n, ok := asInt(m["bytes"]); ok && n >= 0 {
						body["bytes"] = n
					}
				}
				out[k] = body
			case "body_base64":
				out[k] = "[OMITTED]"
			default:
				out[k] = WithoutBodies(item)
			}
		}
		return out
	}
	return value
}
