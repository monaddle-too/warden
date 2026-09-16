package edge

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"warden/chat/internal/browserauth"
)

// loginRecord summarizes every verified sign-in by one Google identity. It holds
// no session material; sessions stay in memory and revocable.
type loginRecord struct {
	Email        string    `json:"email"`
	Subject      string    `json:"sub"`
	Role         string    `json:"role"`
	HostedDomain string    `json:"hosted_domain,omitempty"`
	FirstLogin   time.Time `json:"first_login"`
	LastLogin    time.Time `json:"last_login"`
	Logins       int       `json:"logins"`
}
type ledger struct {
	mu    sync.Mutex
	path  string
	users map[string]loginRecord
}

func newLedger(path string) (*ledger, error) {
	l := &ledger{path: path, users: map[string]loginRecord{}}
	if path == "" {
		return l, nil
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("logins file must be an absolute path")
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	var stored struct {
		Users []loginRecord `json:"users"`
	}
	if err = json.Unmarshal(b, &stored); err != nil {
		return nil, errors.New("invalid logins file")
	}
	for _, u := range stored.Users {
		if u.Email != "" {
			l.users[u.Email] = u
		}
	}
	return l, nil
}
func (l *ledger) persistent() bool { return l.path != "" }

// record never fails a sign-in: a persistence error is logged and the record
// remains in memory until the next successful write.
func (l *ledger) record(login browserauth.Login) {
	email := strings.ToLower(login.Email)
	if email == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u, ok := l.users[email]
	if !ok {
		u = loginRecord{Email: email, FirstLogin: login.At}
	}
	u.Subject, u.Role, u.HostedDomain, u.LastLogin = login.Subject, login.Role, login.HostedDomain, login.At
	u.Logins++
	l.users[email] = u
	if err := l.writeLocked(); err != nil {
		log.Printf("login ledger: %v", err)
	}
}
func (l *ledger) writeLocked() error {
	if l.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(map[string]any{"users": l.snapshotLocked()}, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}
func (l *ledger) snapshotLocked() []loginRecord {
	out := make([]loginRecord, 0, len(l.users))
	for _, u := range l.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastLogin.Equal(out[j].LastLogin) {
			return out[i].LastLogin.After(out[j].LastLogin)
		}
		return out[i].Email < out[j].Email
	})
	return out
}
func (l *ledger) snapshot() []loginRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshotLocked()
}
