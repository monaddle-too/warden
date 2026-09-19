package chats

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	cv "warden/chat/internal/conversation"

	_ "modernc.org/sqlite"
)

// The chat state's database: app/chats.sqlite, one table per kind of
// row (docs/chat-sqlite-store-plan.md). Each row has the columns an
// operator would query and one JSON column the service loads; both are
// written from the same struct in the same statement. A chat's lists
// (entries, turns, approvals, reviews, permission events) are rows keyed
// by the chat and their position in the list, so a change to one item
// writes one row and a truncation deletes the rows past the new length.

const dbFile = "chats.sqlite"

const schemaVersion = 1

var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS chats (
		id TEXT PRIMARY KEY, position INTEGER NOT NULL,
		provider TEXT NOT NULL, model TEXT NOT NULL, title TEXT NOT NULL, sandbox_id TEXT NOT NULL,
		status TEXT NOT NULL, archived INTEGER NOT NULL, created_at REAL NOT NULL,
		record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS entries (
		chat_id TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL,
		role TEXT NOT NULL, turn_id TEXT, parent_id TEXT, created_at REAL NOT NULL, delivery TEXT NOT NULL,
		entry TEXT NOT NULL, PRIMARY KEY (chat_id, seq))`,
	`CREATE INDEX IF NOT EXISTS entries_by_id ON entries (chat_id, id)`,
	`CREATE TABLE IF NOT EXISTS turns (
		chat_id TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL,
		started_at REAL NOT NULL, ended_at REAL NOT NULL, turn TEXT NOT NULL, PRIMARY KEY (chat_id, seq))`,
	`CREATE TABLE IF NOT EXISTS approvals (
		chat_id TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL,
		run_id TEXT NOT NULL, method TEXT NOT NULL, state TEXT NOT NULL, approval TEXT NOT NULL, PRIMARY KEY (chat_id, seq))`,
	`CREATE TABLE IF NOT EXISTS reviews (
		chat_id TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL,
		kind TEXT NOT NULL, status TEXT NOT NULL, review TEXT NOT NULL, PRIMARY KEY (chat_id, seq))`,
	`CREATE TABLE IF NOT EXISTS permission_events (
		chat_id TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL,
		at REAL NOT NULL, tool TEXT NOT NULL, decision TEXT NOT NULL, how TEXT NOT NULL, event TEXT NOT NULL, PRIMARY KEY (chat_id, seq))`,
	`CREATE TABLE IF NOT EXISTS ports (
		id TEXT PRIMARY KEY, seq INTEGER NOT NULL, chat_id TEXT NOT NULL, sandbox_id TEXT NOT NULL,
		port INTEGER NOT NULL, state TEXT NOT NULL, binding TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS deleted_sandboxes (sandbox_id TEXT PRIMARY KEY, seq INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS instructions (
		principal_id TEXT PRIMARY KEY, text TEXT NOT NULL, updated_at REAL NOT NULL, name TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS environments (sandbox_id TEXT PRIMARY KEY, record TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS catalog (provider TEXT PRIMARY KEY, at REAL NOT NULL, models TEXT NOT NULL)`,
}

// openChatDB opens (creating it) the database: WAL, every commit fsynced
// (a message is on disk when its caller is answered), one connection so
// the store's own lock orders the writes.
func openChatDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	// Private, as the transcript file was; SQLite gives the WAL and shared
	// memory files the database file's mode.
	if err = os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	for _, stmt := range schema {
		if _, err = db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	var version string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema'`).Scan(&version)
	switch {
	case err == sql.ErrNoRows:
		if _, err = db.Exec(`INSERT INTO meta (key, value) VALUES ('schema', ?)`, fmt.Sprint(schemaVersion)); err != nil {
			db.Close()
			return nil, err
		}
	case err != nil:
		db.Close()
		return nil, err
	case version != fmt.Sprint(schemaVersion):
		db.Close()
		return nil, fmt.Errorf("unsupported chat data version %s", version)
	}
	return db, nil
}

// shadow is what the database holds, as the encodings its rows were
// written from: a mutation is persisted by comparing the state's
// encodings with these and writing the rows that differ.
type shadow struct {
	chats        map[string]*chatShadow
	ports        map[string]portShadow
	deleted      map[string]int
	instructions map[string]string
	environments map[string]string
	catalog      map[string]string
}

type chatShadow struct {
	position int
	record   string
	lists    [listKinds][]string
}

type portShadow struct {
	seq     int
	binding string
}

func newShadow() shadow {
	return shadow{chats: map[string]*chatShadow{}, ports: map[string]portShadow{}, deleted: map[string]int{}, instructions: map[string]string{}, environments: map[string]string{}, catalog: map[string]string{}}
}

// The kinds of per-chat list, each its own table.
const (
	listEntries = iota
	listTurns
	listApprovals
	listReviews
	listPermissions
	listKinds
)

// listTable describes one per-chat list: its table, the columns beside
// chat_id, seq and the JSON, and how a chat's items are read.
type listTable struct {
	table   string
	column  string   // the JSON column
	columns []string // the queried columns, in the order values gives them
	length  func(*Chat) int
	item    func(*Chat, int) any   // the item to encode
	values  func(*Chat, int) []any // the queried columns' values
	load    func(*Chat, json.RawMessage) error
}

var listTables = [listKinds]listTable{
	listEntries: {
		table: "entries", column: "entry", columns: []string{"id", "role", "turn_id", "parent_id", "created_at", "delivery"},
		length: func(c *Chat) int { return len(c.Conversation.Entries) },
		item:   func(c *Chat, i int) any { return c.Conversation.Entries[i] },
		values: func(c *Chat, i int) []any {
			e := c.Conversation.Entries[i]
			return []any{e.ID, e.Role, nullable(e.TurnID), emptyNull(e.ParentID), e.CreatedAt, e.Delivery}
		},
		load: func(c *Chat, b json.RawMessage) error {
			var e cv.Entry
			if err := json.Unmarshal(b, &e); err != nil {
				return err
			}
			c.Conversation.Entries = append(c.Conversation.Entries, e)
			return nil
		},
	},
	listTurns: {
		table: "turns", column: "turn", columns: []string{"id", "started_at", "ended_at"},
		length: func(c *Chat) int { return len(c.Conversation.Turns) },
		item:   func(c *Chat, i int) any { return c.Conversation.Turns[i] },
		values: func(c *Chat, i int) []any {
			t := c.Conversation.Turns[i]
			return []any{t.ID, t.StartedAt, t.EndedAt}
		},
		load: func(c *Chat, b json.RawMessage) error {
			var t cv.Turn
			if err := json.Unmarshal(b, &t); err != nil {
				return err
			}
			c.Conversation.Turns = append(c.Conversation.Turns, t)
			return nil
		},
	},
	listApprovals: {
		table: "approvals", column: "approval", columns: []string{"id", "run_id", "method", "state"},
		length: func(c *Chat) int { return len(c.Approvals) },
		item:   func(c *Chat, i int) any { return c.Approvals[i] },
		values: func(c *Chat, i int) []any {
			a := c.Approvals[i]
			return []any{a.ID, a.RunID, a.Method, a.State}
		},
		load: func(c *Chat, b json.RawMessage) error {
			var a Approval
			if err := json.Unmarshal(b, &a); err != nil {
				return err
			}
			c.Approvals = append(c.Approvals, a)
			return nil
		},
	},
	listReviews: {
		table: "reviews", column: "review", columns: []string{"id", "kind", "status"},
		length: func(c *Chat) int { return len(c.Reviews) },
		item:   func(c *Chat, i int) any { return c.Reviews[i] },
		values: func(c *Chat, i int) []any {
			r := c.Reviews[i]
			return []any{r.ID, r.Kind, r.Status}
		},
		load: func(c *Chat, b json.RawMessage) error {
			var r Review
			if err := json.Unmarshal(b, &r); err != nil {
				return err
			}
			c.Reviews = append(c.Reviews, r)
			return nil
		},
	},
	listPermissions: {
		table: "permission_events", column: "event", columns: []string{"id", "at", "tool", "decision", "how"},
		length: func(c *Chat) int { return len(c.Permissions) },
		item:   func(c *Chat, i int) any { return c.Permissions[i] },
		values: func(c *Chat, i int) []any {
			p := c.Permissions[i]
			return []any{p.ID, p.At, p.Tool, p.Decision, p.How}
		},
		load: func(c *Chat, b json.RawMessage) error {
			var p PermissionEvent
			if err := json.Unmarshal(b, &p); err != nil {
				return err
			}
			c.Permissions = append(c.Permissions, p)
			return nil
		},
	},
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
func emptyNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// encodeRecord is the chat row's JSON: the chat without the lists that
// have tables of their own and without what is filled in for clients.
func encodeRecord(c *Chat) string {
	r := *c
	r.Conversation.Entries, r.Conversation.Turns = nil, nil
	r.Approvals, r.Reviews, r.Permissions = nil, nil, nil
	r.Typing, r.Startup, r.Spend, r.UndoRewind = nil, nil, nil, ""
	b, _ := json.Marshal(r)
	return string(b)
}

func encodeAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// createdAt is the chat row's created_at: its first entry's time, else
// the time it is first written.
func (s *Store) createdAt(c *Chat) float64 {
	if len(c.Conversation.Entries) > 0 {
		return c.Conversation.Entries[0].CreatedAt
	}
	return float64(time.Now().Unix())
}

// load reads the whole state from the database into memory and sets the
// shadow to what was read.
func (s *Store) load() error {
	sh := newShadow()
	st := State{Version: 1, Chats: []*Chat{}, Ports: []PortBinding{}}
	byID := map[string]*Chat{}
	rows, err := s.db.Query(`SELECT id, position, record FROM chats ORDER BY position`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, record string
		var position int
		if err = rows.Scan(&id, &position, &record); err != nil {
			rows.Close()
			return err
		}
		c := &Chat{}
		if err = json.Unmarshal([]byte(record), c); err != nil {
			rows.Close()
			return fmt.Errorf("chat %s: %w", id, err)
		}
		c.ID = id
		c.Conversation.Entries = []cv.Entry{}
		c.Approvals = []Approval{}
		st.Chats = append(st.Chats, c)
		byID[id] = c
		sh.chats[id] = &chatShadow{position: position, record: record}
	}
	rows.Close()
	for kind, lt := range listTables {
		rows, err = s.db.Query(fmt.Sprintf(`SELECT chat_id, %s FROM %s ORDER BY chat_id, seq`, lt.column, lt.table))
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, item string
			if err = rows.Scan(&id, &item); err != nil {
				rows.Close()
				return err
			}
			c := byID[id]
			if c == nil {
				continue // a row of a chat that is gone; the next write of the chat's rows removes it
			}
			if err = lt.load(c, json.RawMessage(item)); err != nil {
				rows.Close()
				return fmt.Errorf("%s of chat %s: %w", lt.table, id, err)
			}
			sh.chats[id].lists[kind] = append(sh.chats[id].lists[kind], item)
		}
		rows.Close()
	}
	rows, err = s.db.Query(`SELECT id, seq, binding FROM ports ORDER BY seq`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, binding string
		var seq int
		if err = rows.Scan(&id, &seq, &binding); err != nil {
			rows.Close()
			return err
		}
		var p PortBinding
		if err = json.Unmarshal([]byte(binding), &p); err != nil {
			rows.Close()
			return fmt.Errorf("port %s: %w", id, err)
		}
		st.Ports = append(st.Ports, p)
		sh.ports[id] = portShadow{seq: seq, binding: binding}
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT sandbox_id, seq FROM deleted_sandboxes ORDER BY seq`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var seq int
		if err = rows.Scan(&id, &seq); err != nil {
			rows.Close()
			return err
		}
		st.DeletedSandboxes = append(st.DeletedSandboxes, id)
		sh.deleted[id] = seq
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT principal_id, text, updated_at, name FROM instructions`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		in := &Instructions{}
		if err = rows.Scan(&id, &in.Text, &in.UpdatedAt, &in.Name); err != nil {
			rows.Close()
			return err
		}
		if st.Instructions == nil {
			st.Instructions = map[string]*Instructions{}
		}
		st.Instructions[id] = in
		sh.instructions[id] = encodeAny(in)
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT sandbox_id, record FROM environments`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, record string
		if err = rows.Scan(&id, &record); err != nil {
			rows.Close()
			return err
		}
		env := &EnvironmentRecord{}
		if err = json.Unmarshal([]byte(record), env); err != nil {
			rows.Close()
			return fmt.Errorf("environment %s: %w", id, err)
		}
		if st.Environments == nil {
			st.Environments = map[string]*EnvironmentRecord{}
		}
		st.Environments[id] = env
		sh.environments[id] = record
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT provider, models FROM catalog`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var provider, models string
		if err = rows.Scan(&provider, &models); err != nil {
			rows.Close()
			return err
		}
		cat := &Catalog{}
		if err = json.Unmarshal([]byte(models), cat); err != nil {
			rows.Close()
			return fmt.Errorf("catalog %s: %w", provider, err)
		}
		if st.Catalog == nil {
			st.Catalog = map[string]*Catalog{}
		}
		st.Catalog[provider] = cat
		sh.catalog[provider] = models
	}
	rows.Close()
	s.state, s.shadow = st, sh
	return nil
}

// change is one row write and, once the transaction commits, the shadow
// update that records it.
type change struct {
	exec  func(*sql.Tx) error
	apply func()
}

// persist writes the rows whose encoding differs from the shadow, for the
// chats in scopes (or everything when scopes holds scopeAll), in one
// transaction, and reports whether anything was written.
func (s *Store) persist(scopes map[string]bool) (bool, error) {
	if s.shadow.chats == nil {
		s.shadow = newShadow()
	}
	var changes []change
	if scopes[scopeAll] {
		changes = s.planAll()
	} else {
		for id := range scopes {
			changes = append(changes, s.planChat(id)...)
		}
	}
	if len(changes) == 0 {
		return false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	for _, ch := range changes {
		if err = ch.exec(tx); err != nil {
			tx.Rollback()
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	for _, ch := range changes {
		ch.apply()
	}
	s.writes++
	return true, nil
}

// planChat is the writes one chat needs: its row when the record or its
// position changed, or the chat is gone; each list's changed items and
// the rows past its length.
func (s *Store) planChat(id string) []change {
	position := -1
	var c *Chat
	for i, x := range s.state.Chats {
		if x.ID == id {
			position, c = i, x
			break
		}
	}
	sh := s.shadow.chats[id]
	if c == nil {
		if sh == nil {
			return nil
		}
		return []change{{
			exec: func(tx *sql.Tx) error {
				for _, lt := range listTables {
					if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE chat_id = ?`, lt.table), id); err != nil {
						return err
					}
				}
				_, err := tx.Exec(`DELETE FROM chats WHERE id = ?`, id)
				return err
			},
			apply: func() { delete(s.shadow.chats, id) },
		}}
	}
	var changes []change
	record := encodeRecord(c)
	if sh == nil || sh.record != record || sh.position != position {
		created := s.createdAt(c)
		changes = append(changes, change{
			exec: func(tx *sql.Tx) error {
				_, err := tx.Exec(`INSERT INTO chats (id, position, provider, model, title, sandbox_id, status, archived, created_at, record) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					ON CONFLICT (id) DO UPDATE SET position = excluded.position, provider = excluded.provider, model = excluded.model, title = excluded.title, sandbox_id = excluded.sandbox_id, status = excluded.status, archived = excluded.archived, record = excluded.record`,
					id, position, c.Provider, c.Model, c.Title, c.SandboxID, c.Status, c.Archived, created, record)
				return err
			},
			apply: func() {
				if s.shadow.chats[id] == nil {
					s.shadow.chats[id] = &chatShadow{}
				}
				s.shadow.chats[id].position, s.shadow.chats[id].record = position, record
			},
		})
	}
	for kind := range listTables {
		lt := listTables[kind]
		var old []string
		if sh != nil {
			old = sh.lists[kind]
		}
		n := lt.length(c)
		var items []string
		before := len(changes)
		for i := 0; i < n; i++ {
			item := encodeAny(lt.item(c, i))
			items = append(items, item)
			if i < len(old) && old[i] == item {
				continue
			}
			seq, values := i, lt.values(c, i)
			changes = append(changes, change{
				exec: func(tx *sql.Tx) error {
					args := append([]any{id, seq}, values...)
					args = append(args, item)
					marks := "?, ?, " + placeholders(len(values)+1)
					_, err := tx.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s (chat_id, seq, %s, %s) VALUES (%s)`, lt.table, joinColumns(lt.columns), lt.column, marks), args...)
					return err
				},
				apply: func() {},
			})
		}
		if len(old) > n {
			changes = append(changes, change{
				exec: func(tx *sql.Tx) error {
					_, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE chat_id = ? AND seq >= ?`, lt.table), id, n)
					return err
				},
				apply: func() {},
			})
		}
		if len(changes) > before {
			kind := kind
			changes = append(changes, change{exec: func(*sql.Tx) error { return nil }, apply: func() {
				if s.shadow.chats[id] == nil {
					s.shadow.chats[id] = &chatShadow{}
				}
				s.shadow.chats[id].lists[kind] = items
			}})
		}
	}
	return changes
}

// planAll is the writes the whole state needs: every chat's, the chats
// that are gone, and the ports, deleted sandboxes, instructions,
// environments and catalog that differ.
func (s *Store) planAll() []change {
	var changes []change
	seen := map[string]bool{}
	for _, c := range s.state.Chats {
		seen[c.ID] = true
		changes = append(changes, s.planChat(c.ID)...)
	}
	for id := range s.shadow.chats {
		if !seen[id] {
			changes = append(changes, s.planChat(id)...)
		}
	}
	// Ports, by id; seq keeps the list's order.
	ports := map[string]bool{}
	for i, p := range s.state.Ports {
		ports[p.ID] = true
		binding := encodeAny(p)
		if old, ok := s.shadow.ports[p.ID]; ok && old.binding == binding && old.seq == i {
			continue
		}
		p, i := p, i
		changes = append(changes, change{
			exec: func(tx *sql.Tx) error {
				_, err := tx.Exec(`INSERT OR REPLACE INTO ports (id, seq, chat_id, sandbox_id, port, state, binding) VALUES (?, ?, ?, ?, ?, ?, ?)`, p.ID, i, p.ChatID, p.SandboxID, p.Port, p.State, binding)
				return err
			},
			apply: func() { s.shadow.ports[p.ID] = portShadow{seq: i, binding: binding} },
		})
	}
	for id := range s.shadow.ports {
		if !ports[id] {
			id := id
			changes = append(changes, change{
				exec:  func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM ports WHERE id = ?`, id); return err },
				apply: func() { delete(s.shadow.ports, id) },
			})
		}
	}
	deleted := map[string]bool{}
	for i, id := range s.state.DeletedSandboxes {
		deleted[id] = true
		if seq, ok := s.shadow.deleted[id]; ok && seq == i {
			continue
		}
		id, i := id, i
		changes = append(changes, change{
			exec: func(tx *sql.Tx) error {
				_, err := tx.Exec(`INSERT OR REPLACE INTO deleted_sandboxes (sandbox_id, seq) VALUES (?, ?)`, id, i)
				return err
			},
			apply: func() { s.shadow.deleted[id] = i },
		})
	}
	for id := range s.shadow.deleted {
		if !deleted[id] {
			id := id
			changes = append(changes, change{
				exec: func(tx *sql.Tx) error {
					_, err := tx.Exec(`DELETE FROM deleted_sandboxes WHERE sandbox_id = ?`, id)
					return err
				},
				apply: func() { delete(s.shadow.deleted, id) },
			})
		}
	}
	changes = append(changes, planMap(s.shadow.instructions, keys(s.state.Instructions), func(id string) string { return encodeAny(s.state.Instructions[id]) },
		func(tx *sql.Tx, id, _ string) error {
			in := s.state.Instructions[id]
			_, err := tx.Exec(`INSERT OR REPLACE INTO instructions (principal_id, text, updated_at, name) VALUES (?, ?, ?, ?)`, id, in.Text, in.UpdatedAt, in.Name)
			return err
		},
		func(tx *sql.Tx, id string) error {
			_, err := tx.Exec(`DELETE FROM instructions WHERE principal_id = ?`, id)
			return err
		})...)
	changes = append(changes, planMap(s.shadow.environments, keys(s.state.Environments), func(id string) string { return encodeAny(s.state.Environments[id]) },
		func(tx *sql.Tx, id, record string) error {
			_, err := tx.Exec(`INSERT OR REPLACE INTO environments (sandbox_id, record) VALUES (?, ?)`, id, record)
			return err
		},
		func(tx *sql.Tx, id string) error {
			_, err := tx.Exec(`DELETE FROM environments WHERE sandbox_id = ?`, id)
			return err
		})...)
	changes = append(changes, planMap(s.shadow.catalog, keys(s.state.Catalog), func(id string) string { return encodeAny(s.state.Catalog[id]) },
		func(tx *sql.Tx, id, models string) error {
			_, err := tx.Exec(`INSERT OR REPLACE INTO catalog (provider, at, models) VALUES (?, ?, ?)`, id, s.state.Catalog[id].At, models)
			return err
		},
		func(tx *sql.Tx, id string) error {
			_, err := tx.Exec(`DELETE FROM catalog WHERE provider = ?`, id)
			return err
		})...)
	return changes
}

// planMap is the writes a map-shaped table needs: the keys whose encoding
// differs from the shadow are written, the keys the state lost deleted.
func planMap(shadow map[string]string, ids []string, encode func(string) string, write func(*sql.Tx, string, string) error, remove func(*sql.Tx, string) error) []change {
	var changes []change
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
		enc := encode(id)
		if shadow[id] == enc {
			continue
		}
		id := id
		changes = append(changes, change{
			exec:  func(tx *sql.Tx) error { return write(tx, id, enc) },
			apply: func() { shadow[id] = enc },
		})
	}
	for id := range shadow {
		if !seen[id] {
			id := id
			changes = append(changes, change{
				exec:  func(tx *sql.Tx) error { return remove(tx, id) },
				apply: func() { delete(shadow, id) },
			})
		}
	}
	return changes
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func placeholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ", "
		}
		s += "?"
	}
	return s
}

func joinColumns(cols []string) string {
	s := ""
	for i, c := range cols {
		if i > 0 {
			s += ", "
		}
		s += c
	}
	return s
}
