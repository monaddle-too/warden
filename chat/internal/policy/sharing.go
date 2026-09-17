package policy

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ValueError marks validation failures whose message may be shown to the
// requesting side, as distinct from transport or storage failures.
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

func valueErr(msg string) error { return &ValueError{Msg: msg} }

func isValueError(err error) bool {
	var v *ValueError
	return errors.As(err, &v)
}

var sharingScopes = []string{"https://www.googleapis.com/auth/documents", "https://www.googleapis.com/auth/spreadsheets", "https://www.googleapis.com/auth/drive.metadata.readonly"}

// GoogleSharing is the owner's Google connection as seen by Sharing.
type GoogleSharing interface {
	Configured() bool
	Connected() bool
	CanWrite() bool
	Start() (string, error)
	Complete(state, code string) error
	Files(page string) (map[string]any, error)
	File(id string) (map[string]any, error)
	Create(title string) (map[string]any, error)
	Authorization() (string, error)
	// Disconnect forgets the stored Google credential (revoking it with
	// Google on a best-effort basis) so the account must be connected again.
	Disconnect() error
}

// GoogleConnection is the durable, write-capable Google connection used by
// public chat deployments. Tokens persist in google.sqlite.
type GoogleConnection struct {
	*OAuthConnection
	Root          string
	DB            *sql.DB
	grantedScopes map[string]bool
	// HTTP is replaceable in tests: method, host, path, body, headers.
	HTTP func(host, method, path string, body []byte, headers map[string]string, timeout time.Duration, limit int64) (int, []byte, error)
}

// GoogleClientOptions selects the Docs OAuth client. ConfigFile is the
// operator's private client file and always wins. Otherwise, when the
// release ships a built-in Desktop client (BuiltinClientID non-empty) and
// ChatListen names the chat listener, the connection uses that client with
// the loopback redirect http://127.0.0.1:<chat port>/oauth/google_docs/callback,
// which warden-chat serves itself. With neither, the connection is not
// configured and the UI hides it.
type GoogleClientOptions struct {
	ConfigFile          string
	ChatListen          string
	BuiltinClientID     string
	BuiltinClientSecret string
}

// BuiltinGoogleRedirect derives the loopback callback for the built-in
// client from the chat listen address; only the port is used because
// warden-chat checks the Host header against 127.0.0.1:<port>.
func BuiltinGoogleRedirect(chatListen string) (string, error) {
	_, port, err := net.SplitHostPort(chatListen)
	if err != nil {
		return "", errors.New("chat listen address must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("chat listen address must name a port")
	}
	return "http://127.0.0.1:" + port + googleDocsProvider.callbackPath, nil
}

// NewGoogleConnection opens the Docs connection with an operator client file
// only (the OVH shape); see NewGoogleConnectionWithClient for the built-in client.
func NewGoogleConnection(root, config string, clock Clock) (*GoogleConnection, error) {
	return NewGoogleConnectionWithClient(root, GoogleClientOptions{ConfigFile: config}, clock)
}

// NewGoogleConnectionWithClient opens the Docs connection with the client
// GoogleClientOptions selects.
func NewGoogleConnectionWithClient(root string, options GoogleClientOptions, clock Clock) (*GoogleConnection, error) {
	g := &GoogleConnection{OAuthConnection: NewGoogleDocsConnection(NewRedactor(), clock), Root: root, grantedScopes: map[string]bool{}, HTTP: directHTTPS}
	config := options.ConfigFile
	if config == "" && options.BuiltinClientID != "" && options.ChatListen != "" {
		redirect, err := BuiltinGoogleRedirect(options.ChatListen)
		if err != nil {
			return nil, err
		}
		if err = g.Configure(options.BuiltinClientID, options.BuiltinClientSecret, redirect); err != nil {
			return nil, errors.New("built-in Google client: " + err.Error())
		}
	}
	if config != "" {
		raw, err := openPrivate(config, 1024*1024, "Google configuration")
		if err != nil {
			return nil, errors.New("Google configuration must be private")
		}
		var c struct{ ClientID, ClientSecret, RedirectURI string }
		var generic map[string]any
		if err = json.Unmarshal(raw, &generic); err != nil {
			return nil, err
		}
		c.ClientID, _ = generic["client_id"].(string)
		c.ClientSecret, _ = generic["client_secret"].(string)
		c.RedirectURI, _ = generic["redirect_uri"].(string)
		if err = g.Configure(c.ClientID, c.ClientSecret, c.RedirectURI); err != nil {
			return nil, err
		}
	}
	db, err := openSQLite(filepath.Join(root, "google.sqlite"))
	if err != nil {
		return nil, err
	}
	os.Chmod(filepath.Join(root, "google.sqlite"), 0o600)
	if _, err = db.Exec("CREATE TABLE IF NOT EXISTS credentials (id INTEGER PRIMARY KEY, data TEXT)"); err != nil {
		db.Close()
		return nil, err
	}
	// The connection record names who connected (decision 8); the one
	// existing record is the owner's.
	if err = ensureTextColumns(db, "credentials", [][2]string{{"principal", OwnerPrincipal}}); err != nil {
		db.Close()
		return nil, err
	}
	g.DB = db
	var stored string
	if err = db.QueryRow("SELECT data FROM credentials WHERE id=1").Scan(&stored); err == nil && g.ClientID != "" {
		var data map[string]any
		if json.Unmarshal([]byte(stored), &data) == nil && data["client_id"] == g.ClientID {
			g.AccessToken, _ = data["access_token"].(string)
			g.RefreshToken, _ = data["refresh_token"].(string)
			g.Expires, _ = asNumber(data["expires"])
			scopes := []string{"https://www.googleapis.com/auth/documents.readonly", "https://www.googleapis.com/auth/drive.metadata.readonly"}
			if list, ok := data["scopes"].([]any); ok {
				scopes = nil
				for _, item := range list {
					if s, ok := item.(string); ok {
						scopes = append(scopes, s)
					}
				}
			}
			g.grantedScopes = map[string]bool{}
			for _, s := range scopes {
				g.grantedScopes[s] = true
			}
			g.Redactor.Register(g.AccessToken)
			g.Redactor.Register(g.RefreshToken)
		}
	}
	return g, nil
}

func (g *GoogleConnection) Close() {
	if g.DB != nil {
		g.DB.Close()
		g.DB = nil
	}
}

// Configure accepts the public HTTPS callback of an authenticated edge.
func (g *GoogleConnection) Configure(clientID, clientSecret, redirectURI string) error {
	return g.OAuthConnection.Configure(clientID, clientSecret, redirectURI, true)
}

func (g *GoogleConnection) Configured() bool { return g.ClientID != "" }

func (g *GoogleConnection) CanWrite() bool {
	return g.AccessToken != "" && g.grantedScopes["https://www.googleapis.com/auth/documents"]
}

// Start requests the write and file-listing scopes.
func (g *GoogleConnection) Start() (string, error) {
	return g.startWithScopes(sharingScopes)
}

// Complete finishes the callback with this connection's scope rules.
func (g *GoogleConnection) Complete(state, code string) error {
	c := g.OAuthConnection
	if c.pendingState == "" || state != c.pendingState || c.now() >= c.pendingExpires {
		return errors.New("invalid or expired Google Docs connection request")
	}
	verifier := c.pendingVerifier
	c.pendingState, c.pendingVerifier, c.pendingExpires = "", "", 0
	if !authCodeShape(code) {
		return errors.New("invalid Google Docs authorization code")
	}
	c.Redactor.Register(code)
	result, err := c.Transport(c.ClientID, c.ClientSecret, c.provider.tokenEndpoint, map[string]string{
		"code": code, "redirect_uri": c.RedirectURI, "grant_type": "authorization_code", "code_verifier": verifier})
	if err != nil {
		return err
	}
	return g.Install(result, true)
}

// Install requires the listing scope plus a document scope, then persists.
func (g *GoogleConnection) Install(data map[string]any, initial bool) error {
	scopeText, _ := data["scope"].(string)
	scopes := map[string]bool{}
	for _, s := range strings.Fields(scopeText) {
		scopes[s] = true
	}
	if !scopes["https://www.googleapis.com/auth/drive.metadata.readonly"] || !(scopes["https://www.googleapis.com/auth/documents"] || scopes["https://www.googleapis.com/auth/documents.readonly"]) {
		return errors.New("Google did not grant document and file-list access; reconnect")
	}
	// Reuse the credential validation without changing the older adapter.
	adjusted := map[string]any{}
	for k, v := range data {
		adjusted[k] = v
	}
	adjusted["scope"] = "https://www.googleapis.com/auth/documents.readonly"
	if err := g.OAuthConnection.Install(adjusted, initial); err != nil {
		return err
	}
	g.grantedScopes = scopes
	sorted := keysOf(map[string]any{})
	sorted = sorted[:0]
	for s := range scopes {
		sorted = append(sorted, s)
	}
	sortStrings(sorted)
	record := map[string]any{"client_id": g.ClientID, "access_token": g.AccessToken, "refresh_token": g.RefreshToken, "expires": g.Expires, "scopes": sorted}
	_, err := g.DB.Exec("INSERT OR REPLACE INTO credentials (id,data,principal) VALUES (1,?,?)", string(mustJSON(record)), OwnerPrincipal)
	return err
}

// Disconnect revokes the refresh token with Google when it can (a failure
// there is not fatal: the token is forgotten locally either way), clears the
// in-memory credential and deletes the stored record.
func (g *GoogleConnection) Disconnect() error {
	if token := g.RefreshToken; token != "" && g.HTTP != nil {
		form := url.Values{"token": {token}}
		_, _, _ = g.HTTP("oauth2.googleapis.com", "POST", "/revoke", []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 10*time.Second, 65537)
	}
	g.OAuthConnection.Disconnect()
	g.grantedScopes = map[string]bool{}
	if g.DB == nil {
		return nil
	}
	_, err := g.DB.Exec("DELETE FROM credentials WHERE id=1")
	return err
}

// Authorization refreshes with this connection's install rules.
func (g *GoogleConnection) Authorization() (string, error) {
	return g.OAuthConnection.authorizationWith(g.Install)
}

// Create makes a new Google document owned by the connected account.
func (g *GoogleConnection) Create(title string) (map[string]any, error) {
	if !g.CanWrite() {
		return nil, errors.New("Reconnect Google to allow document creation")
	}
	authorization, err := g.Authorization()
	if err != nil {
		return nil, err
	}
	status, raw, err := g.HTTP("docs.googleapis.com", "POST", "/v1/documents", mustJSON(map[string]any{"title": title}), map[string]string{"Authorization": authorization, "Content-Type": "application/json"}, 15*time.Second, 1048577)
	if err != nil {
		return nil, err
	}
	if status != 200 || len(raw) > 1048576 {
		return nil, errors.New("Google document creation failed; check Google before retrying")
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	id, _ := data["documentId"].(string)
	if !googleDocumentID.MatchString(id) {
		return nil, errors.New("invalid created document response")
	}
	name := title
	if t, ok := data["title"].(string); ok {
		name = t
	}
	return map[string]any{"id": id, "title": name, "url": "https://docs.google.com/document/d/" + id + "/edit", "api_url": "https://docs.googleapis.com/v1/documents/" + id}, nil
}

// Files lists recent Google documents visible to the connected account.
func (g *GoogleConnection) Files(page string) (map[string]any, error) {
	params := url.Values{}
	params.Set("q", "trashed = false and (mimeType = 'application/vnd.google-apps.document' or mimeType = 'application/vnd.google-apps.spreadsheet') and createdTime >= '2026-09-09T00:00:00Z'")
	params.Set("fields", "nextPageToken,files(id,name,mimeType)")
	params.Set("pageSize", "100")
	params.Set("orderBy", "modifiedTime desc")
	if page != "" {
		params.Set("pageToken", page)
	}
	return g.get("/drive/v3/files?" + params.Encode())
}

// File validates that an ID is an available Google document.
func (g *GoogleConnection) File(id string) (map[string]any, error) {
	if !googleDocumentID.MatchString(id) {
		return nil, errors.New("invalid document")
	}
	f, err := g.get("/drive/v3/files/" + id + "?fields=id,name,mimeType,trashed")
	if err != nil {
		return nil, err
	}
	name, _ := f["name"].(string)
	switch trashed, _ := f["trashed"].(bool); {
	case trashed:
		return nil, errors.New("not an available Google document")
	case f["mimeType"] == "application/vnd.google-apps.document":
		return map[string]any{"id": id, "title": name, "kind": "document", "url": "https://docs.google.com/document/d/" + id + "/edit", "api_url": "https://docs.googleapis.com/v1/documents/" + id}, nil
	case f["mimeType"] == "application/vnd.google-apps.spreadsheet":
		return map[string]any{"id": id, "title": name, "kind": "spreadsheet", "url": "https://docs.google.com/spreadsheets/d/" + id + "/edit", "api_url": "https://sheets.googleapis.com/v4/spreadsheets/" + id}, nil
	}
	return nil, errors.New("not an available Google document or spreadsheet")
}

func (g *GoogleConnection) get(path string) (map[string]any, error) {
	authorization, err := g.Authorization()
	if err != nil {
		return nil, err
	}
	status, raw, err := g.HTTP("www.googleapis.com", "GET", path, nil, map[string]string{"Authorization": authorization, "Accept": "application/json"}, 8*time.Second, 1048577)
	if err != nil {
		return nil, err
	}
	if status != 200 || len(raw) > 1048576 {
		return nil, errors.New("Google file listing unavailable; reconnect or retry")
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// Sharing is the durable owner-approved sharing service: Google documents,
// blocked documents, repository selection, images and pull requests.
type Sharing struct {
	mu     sync.Mutex
	Clock  Clock
	Google GoogleSharing
	// GitHubConfigured and GitHubAppSlug come from providers.github; the UI
	// hides the repository section when GitHub is absent and links to the
	// App's installation page by slug (empty in user-token mode).
	GitHubConfigured bool
	GitHubAppSlug    string
	GitHub           GitHubCredentials
	DB               *sql.DB
	Images           *Images
	PullRequests     *PullRequests
	// Egress, when set, is the registry's runtime egress switch exposed to
	// the console (the "egress" and "egress_set" operations).
	Egress EgressSwitch
	// Network, when set, applies owner-approved temporary host grants to a
	// sandbox's engine (the "network_allow" operation).
	Network NetworkGrants
}

// NetworkGrants is the registry as the sharing store needs it for
// request_network_access approvals.
type NetworkGrants interface {
	AllowHost(sandbox, host string, until float64) error
}

// EgressSwitch is the registry as the console sees it: the current mode
// and where it came from, and a setter that applies everywhere.
type EgressSwitch interface {
	EgressMode() (mode, source string)
	SetEgressMode(mode string) error
}

// Egress mode names as the console and warden.json use them, mapped to the
// policy document's own ("restricted" and "public").
var egressNames = map[string]string{"restricted": "restricted", "public": "open"}
var egressModes = map[string]string{"restricted": "restricted", "open": "public"}

var validDurations = map[int64]bool{900: true, 3600: true, 86400: true, 604800: true}

// NewSharing opens the sharing store. github may be nil (no GitHub provider),
// the App broker or the user-token source; a typed nil pointer is treated as
// absent so callers can pass either concrete type directly.
func NewSharing(root string, google GoogleSharing, clock Clock, github GitHubCredentials) (*Sharing, error) {
	if app, ok := github.(*GitHubAppCredentials); ok && app == nil {
		github = nil
	}
	if user, ok := github.(*GitHubUserCredentials); ok && user == nil {
		github = nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	db, err := openSQLite(filepath.Join(root, "sharing.sqlite"))
	if err != nil {
		return nil, err
	}
	os.Chmod(filepath.Join(root, "sharing.sqlite"), 0o600)
	s := &Sharing{Clock: clock, Google: google, GitHub: github, DB: db}
	if s.Clock == nil {
		s.Clock = wallClock
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS requests (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, reason TEXT, status TEXT, created REAL, expires REAL, documents TEXT, delivered INTEGER DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS repositories (chat TEXT, sandbox TEXT, owner TEXT, app INTEGER, name TEXT, id INTEGER, grant_id TEXT, PRIMARY KEY(chat,sandbox,name))`,
		`CREATE TABLE IF NOT EXISTS blocked_documents (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', blocked REAL)`,
		// Who shared which repositories with a sandbox, and when: the
		// repositories table holds only the current selection.
		`CREATE TABLE IF NOT EXISTS repository_events (id INTEGER PRIMARY KEY AUTOINCREMENT, at REAL, sandbox TEXT, actor TEXT, kind TEXT, detail TEXT)`,
	} {
		if _, err = db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Grants and repository selections record which principal asked and
	// approved (decision 8). Rows from before the column existed were the
	// owner's, so the default back-fills them.
	for table, columns := range map[string][][2]string{
		// resolved/resolved_by: when and by whom a request left "pending"
		// (approval, denial, revocation), for the access history.
		"requests":     {{"access", "read"}, {"title", ""}, {"principal", OwnerPrincipal}, {"resolved", ""}, {"resolved_by", ""}},
		"repositories": {{"principal", OwnerPrincipal}, {"access", "contents,pull_requests"}},
	} {
		if err = ensureTextColumns(db, table, columns); err != nil {
			db.Close()
			return nil, err
		}
	}
	// A crash during a non-idempotent Google create is uncertain, never replay it.
	if _, err = db.Exec("UPDATE requests SET status='failed' WHERE status='creating'"); err != nil {
		db.Close()
		return nil, err
	}
	if s.Images, err = newImages(s); err != nil {
		db.Close()
		return nil, err
	}
	if s.PullRequests, err = newPullRequests(s); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OwnerPrincipal is the single principal of a local deployment. Every
// operation records it until the edge and chat supply real principals.
const OwnerPrincipal = "owner"

// principalOf returns the principal an operation names, or the owner.
func principalOf(data map[string]any) string {
	if p, ok := data["principal"].(string); ok && validIdentifier(p) {
		return p
	}
	return OwnerPrincipal
}

// ensureTextColumns adds each missing TEXT NOT NULL column with its default,
// which SQLite applies to existing rows.
func ensureTextColumns(db *sql.DB, table string, columns [][2]string) error {
	existing := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err = rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	rows.Close()
	for _, column := range columns {
		if !existing[column[0]] {
			if _, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column[0] + " TEXT NOT NULL DEFAULT '" + column[1] + "'"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Sharing) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DB != nil {
		s.DB.Close()
		s.DB = nil
	}
}

func (s *Sharing) blockedLocked() (map[string]bool, error) {
	rows, err := s.DB.Query("SELECT id FROM blocked_documents")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

type sharingRow struct {
	id, chat, sandbox, reason, status, documents, access, title string
	created                                                     float64
	expires                                                     sql.NullFloat64
	delivered                                                   int64
	resolved, resolvedBy                                        string // resolved: unix seconds as text, "" while pending
}

const sharingColumns = "id,chat,sandbox,reason,status,created,expires,documents,delivered,access,title,resolved,resolved_by"

func scanSharing(scanner interface{ Scan(...any) error }) (*sharingRow, error) {
	var r sharingRow
	var reason, documents, access, title, resolved, resolvedBy sql.NullString
	var created sql.NullFloat64
	err := scanner.Scan(&r.id, &r.chat, &r.sandbox, &reason, &r.status, &created, &r.expires, &documents, &r.delivered, &access, &title, &resolved, &resolvedBy)
	if err != nil {
		return nil, err
	}
	r.reason, r.documents, r.access, r.title, r.created = reason.String, documents.String, access.String, title.String, created.Float64
	r.resolved, r.resolvedBy = resolved.String, resolvedBy.String
	return &r, nil
}

func (s *Sharing) rowLocked(id string) (*sharingRow, error) {
	return scanSharing(s.DB.QueryRow("SELECT "+sharingColumns+" FROM requests WHERE id=?", id))
}

func (s *Sharing) rowsLocked(query string, args ...any) ([]*sharingRow, error) {
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sharingRow
	for rows.Next() {
		r, err := scanSharing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Sharing) result(r *sharingRow) map[string]any {
	var documents any = []any{}
	_ = json.Unmarshal([]byte(r.documents), &documents)
	if documents == nil {
		documents = []any{}
	}
	var expires any
	if r.expires.Valid {
		expires = r.expires.Float64
	}
	out := map[string]any{"request_id": r.id, "chatID": r.chat, "sandboxID": r.sandbox, "reason": r.reason, "status": r.status, "expires_at": expires, "documents": documents, "access": r.access, "title": r.title,
		"created_at": r.created, "resolved_by": r.resolvedBy}
	if n, err := strconv.ParseFloat(r.resolved, 64); err == nil {
		out["resolved_at"] = n
	} else {
		out["resolved_at"] = nil
	}
	return out
}

// actorOf is the person a console operation names ("" for the agent or an
// unattributed caller); the chat fills data["actor"] from the edge's identity.
func actorOf(data map[string]any) string {
	a, _ := data["actor"].(string)
	if len(a) > 200 {
		a = a[:200]
	}
	return a
}

// resolvedNow marks rows as resolved at this moment by actor.
func (s *Sharing) resolvedNow(actor string) (string, string) {
	return strconv.FormatFloat(s.Clock(), 'f', 3, 64), actor
}

func resultsOf(s *Sharing, rows []*sharingRow) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.result(r))
	}
	return out
}

func stringField(data map[string]any, key string) string {
	v, _ := data[key].(string)
	return v
}

func validIdentifier(v string) bool { return len(v) > 0 && len(v) <= 128 }

// Dispatch handles one sharing operation from the chat service or owner UI.
func (s *Sharing) Dispatch(op string, data map[string]any) (map[string]any, error) {
	if data == nil {
		data = map[string]any{}
	}
	switch {
	case strings.HasPrefix(op, "image_"):
		return s.Images.Dispatch(op, data)
	case op == "pr_submit":
		result, err := s.PullRequests.Submit(data)
		if err != nil && isValueError(err) {
			return map[string]any{"status": "invalid", "error": err.Error()}, nil
		}
		return result, err
	case strings.HasPrefix(op, "pr_"):
		return s.PullRequests.Dispatch(op, data)
	case op == "github_write":
		return s.githubWrite(data)
	case strings.HasPrefix(op, "github_"):
		return s.githubDispatch(op, data)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dispatchLocked(op, data)
}

func (s *Sharing) dispatchLocked(op string, data map[string]any) (map[string]any, error) {
	switch op {
	case "status":
		// "configured" per provider says whether warden.json names it; the UI
		// hides an absent provider instead of showing it as disconnected.
		// connected: the App broker is configured, or the user sign-in file
		// is readable right now.
		github := map[string]any{"configured": s.GitHubConfigured || s.GitHub != nil, "connected": false, "owner": "", "appSlug": s.GitHubAppSlug}
		if s.GitHub != nil {
			owner, _, err := s.GitHub.Identity()
			github["connected"] = err == nil
			github["owner"] = owner
			// The user-token source can say who signed in, with which scopes
			// and when, and can be disconnected from the UI; the App broker
			// is the operator's and cannot.
			if d, ok := s.GitHub.(interface{ Details() map[string]any }); ok {
				for k, v := range d.Details() {
					github[k] = v
				}
			}
			_, github["disconnectable"] = s.GitHub.(interface{ Disconnect() error })
		}
		// google.configured: a usable Docs client exists (an operator file, or
		// the built-in client with a shipped ID). A provider section alone,
		// with no client, hides the Google UI rather than offering a connect
		// button that can only fail.
		google := map[string]any{"configured": s.Google != nil && s.Google.Configured(), "connected": s.Google != nil && s.Google.Connected()}
		return map[string]any{"configured": s.Google != nil && s.Google.Configured(), "connected": s.Google != nil && s.Google.Connected(), "can_write": s.Google != nil && s.Google.CanWrite(), "google": google, "github": github}, nil
	case "network_allow":
		// The owner approved an agent's request_network_access: one public
		// host, HTTP/HTTPS, for a bounded time, this sandbox only.
		if s.Network == nil {
			return nil, errors.New("network grants unavailable")
		}
		sandbox := stringField(data, "sandboxID")
		host := strings.TrimRight(strings.ToLower(stringField(data, "host")), ".")
		seconds, ok := asInt(data["duration"])
		if !validIdentifier(sandbox) || !ValidHost(host) || !ok || seconds < 60 || seconds > 86400 {
			return nil, errors.New("network grant needs a public hostname and a duration of 1 minute to 24 hours")
		}
		until := s.Clock() + float64(seconds)
		if err := s.Network.AllowHost(sandbox, host, until); err != nil {
			return nil, err
		}
		detail := map[string]any{"host": host, "until": until, "reason": stringField(data, "reason")}
		if _, err := s.DB.Exec("INSERT INTO repository_events (at,sandbox,actor,kind,detail) VALUES (?,?,?,?,?)", s.Clock(), sandbox, actorOf(data), "network_allowed", string(mustJSON(detail))); err != nil {
			return nil, err
		}
		return map[string]any{"host": host, "expires_at": until}, nil
	case "github_write":
		return s.githubWrite(data)
	case "egress":
		if s.Egress == nil {
			return nil, errors.New("egress switch unavailable")
		}
		mode, source := s.Egress.EgressMode()
		return map[string]any{"mode": egressNames[mode], "source": source}, nil
	case "egress_set":
		if s.Egress == nil {
			return nil, errors.New("egress switch unavailable")
		}
		mode, ok := egressModes[stringField(data, "mode")]
		if !ok {
			return nil, errors.New("mode must be restricted or open")
		}
		if err := s.Egress.SetEgressMode(mode); err != nil {
			return nil, err
		}
		mode, source := s.Egress.EgressMode()
		return map[string]any{"mode": egressNames[mode], "source": source}, nil
	case "disconnect":
		// Forget one provider's sign-in. Everything that credential backed
		// is revoked with it: Google document grants, GitHub repository
		// selections (a later sign-in, perhaps as someone else, must not
		// inherit them). The stored secret is gone after this returns.
		switch stringField(data, "provider") {
		case "google":
			if s.Google == nil || !s.Google.Connected() {
				return nil, errors.New("Google is not connected")
			}
			if err := s.Google.Disconnect(); err != nil {
				return nil, err
			}
			at, by := s.resolvedNow("Google disconnected")
			if _, err := s.DB.Exec("UPDATE requests SET status='revoked',resolved=?,resolved_by=? WHERE status='granted'", at, by); err != nil {
				return nil, err
			}
		case "github":
			d, ok := s.GitHub.(interface{ Disconnect() error })
			if !ok {
				return nil, errors.New("this GitHub connection is managed by the operator and cannot be disconnected here")
			}
			if err := d.Disconnect(); err != nil {
				return nil, err
			}
			if _, err := s.DB.Exec("DELETE FROM repositories"); err != nil {
				return nil, err
			}
			if _, err := s.DB.Exec("INSERT INTO repository_events (at,sandbox,actor,kind,detail) VALUES (?,?,?,?,?)", s.Clock(), "", actorOf(data), "github_disconnected", "{}"); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("provider must be google or github")
		}
		return map[string]any{"ok": true}, nil
	case "connect":
		if s.Google == nil {
			return nil, errors.New("Google is not configured")
		}
		u, err := s.Google.Start()
		if err != nil {
			return nil, err
		}
		return map[string]any{"authorization_url": u}, nil
	case "callback":
		if s.Google == nil {
			return nil, errors.New("Google is not configured")
		}
		if err := s.Google.Complete(stringField(data, "state"), stringField(data, "code")); err != nil {
			return nil, err
		}
		// A reconnected Google account must never inherit old grants.
		at, by := s.resolvedNow("Google reconnected")
		if _, err := s.DB.Exec("UPDATE requests SET status='revoked',resolved=?,resolved_by=? WHERE status='granted'", at, by); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	case "files":
		if s.Google == nil {
			return nil, errors.New("Google is not configured")
		}
		listing, err := s.Google.Files(stringField(data, "page"))
		if err != nil {
			return nil, err
		}
		blocked, err := s.blockedLocked()
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for k, v := range listing {
			out[k] = v
		}
		files := []any{}
		list, _ := listing["files"].([]any)
		for _, item := range list {
			f, _ := item.(map[string]any)
			entry := map[string]any{}
			for k, v := range f {
				entry[k] = v
			}
			id, _ := f["id"].(string)
			entry["blocked"] = blocked[id]
			files = append(files, entry)
		}
		out["files"] = files
		return out, nil
	case "blocked":
		rows, err := s.DB.Query("SELECT id,name,blocked FROM blocked_documents ORDER BY blocked DESC, id")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		documents := []any{}
		for rows.Next() {
			var id, name string
			var blocked float64
			if err = rows.Scan(&id, &name, &blocked); err != nil {
				return nil, err
			}
			documents = append(documents, map[string]any{"id": id, "name": name, "blocked_at": blocked})
		}
		return map[string]any{"documents": documents}, rows.Err()
	case "block", "unblock":
		id, idOK := data["id"].(string)
		name := ""
		if v, present := data["name"]; present {
			var ok bool
			if name, ok = v.(string); !ok {
				return nil, errors.New("invalid document")
			}
		}
		if !idOK || !googleDocumentID.MatchString(id) || len(name) > 200 {
			return nil, errors.New("invalid document")
		}
		revoked := []any{}
		if op == "unblock" {
			if _, err := s.DB.Exec("DELETE FROM blocked_documents WHERE id=?", id); err != nil {
				return nil, err
			}
		} else {
			if _, err := s.DB.Exec("INSERT OR REPLACE INTO blocked_documents VALUES (?,?,?)", id, name, s.Clock()); err != nil {
				return nil, err
			}
			// An existing grant must not outlive the tag.
			granted, err := s.rowsLocked("SELECT " + sharingColumns + " FROM requests WHERE status='granted'")
			if err != nil {
				return nil, err
			}
			for _, r := range granted {
				var documents []map[string]any
				_ = json.Unmarshal([]byte(r.documents), &documents)
				for _, d := range documents {
					if d["id"] == id {
						revoked = append(revoked, r.id)
						break
					}
				}
			}
			at, by := s.resolvedNow(actorOf(data))
			if by == "" {
				by = "document tagged unsharable"
			}
			for _, rid := range revoked {
				if _, err = s.DB.Exec("UPDATE requests SET status='revoked',resolved=?,resolved_by=? WHERE id=?", at, by, rid); err != nil {
					return nil, err
				}
			}
		}
		return map[string]any{"ok": true, "blocked": op == "block", "revoked": revoked}, nil
	case "select":
		request := map[string]any{}
		for k, v := range data {
			request[k] = v
		}
		request["callID"] = "owner-" + randomHex(16)
		request["reason"] = "Owner shared documents during conversation setup"
		requested, err := s.dispatchLocked("request", request)
		if err != nil {
			return nil, err
		}
		resolve := map[string]any{}
		for k, v := range data {
			resolve[k] = v
		}
		resolve["id"] = requested["request_id"]
		resolve["allow"] = true
		result, err := s.dispatchLocked("resolve", resolve)
		if err != nil {
			return nil, err
		}
		if _, err = s.DB.Exec("UPDATE requests SET delivered=1 WHERE id=?", requested["request_id"]); err != nil {
			return nil, err
		}
		return result, nil
	case "request":
		chat, sandbox := stringField(data, "chatID"), stringField(data, "sandboxID")
		reason := ""
		if v, present := data["reason"]; present {
			var ok bool
			if reason, ok = v.(string); !ok {
				return nil, errors.New("invalid permission request")
			}
		}
		if !validIdentifier(chat) || !validIdentifier(sandbox) || len(reason) < 1 || len(reason) > 2000 {
			return nil, errors.New("invalid permission request")
		}
		access := "read"
		if v, present := data["access"]; present {
			access, _ = v.(string)
		}
		title := ""
		if v, present := data["title"]; present {
			var ok bool
			if title, ok = v.(string); !ok {
				return nil, errors.New("invalid document access or title")
			}
		}
		if AccessRank(access) < 0 || len(title) > 200 || (access == "create" && strings.TrimSpace(title) == "") {
			return nil, errors.New("invalid document access or title")
		}
		// Retry the same tool call without creating duplicate prompts.
		call, ok := data["callID"].(string)
		if !ok || len(call) < 1 || len(call) > 512 {
			return nil, errors.New("invalid tool call")
		}
		key := sha256Hex([]byte(chat + "\x00" + sandbox + "\x00" + call))
		if _, err := s.DB.Exec("INSERT OR IGNORE INTO requests (id,chat,sandbox,reason,status,created,expires,documents,delivered,access,title,principal) VALUES (?,?,?,?,?,?,?,?,0,?,?,?)", key, chat, sandbox, reason, "pending", s.Clock(), nil, "[]", access, title, principalOf(data)); err != nil {
			return nil, err
		}
		r, err := s.rowLocked(key)
		if err != nil {
			return nil, err
		}
		return s.result(r), nil
	case "state":
		rows, err := s.rowsLocked("SELECT " + sharingColumns + " FROM requests ORDER BY created")
		if err != nil {
			return nil, err
		}
		return map[string]any{"requests": resultsOf(s, rows)}, nil
	case "get":
		// Grants belong to the environment (sandbox): every chat on it shares
		// one disk, so the requesting chat is attribution, not a boundary.
		r, err := scanSharing(s.DB.QueryRow("SELECT "+sharingColumns+" FROM requests WHERE id=? AND sandbox=?", stringField(data, "id"), stringField(data, "sandboxID")))
		if err != nil {
			return nil, errors.New("unknown request")
		}
		return s.result(r), nil
	case "resolve":
		r, err := s.rowLocked(stringField(data, "id"))
		if err != nil {
			return nil, errors.New("unknown request")
		}
		if r.status != "pending" {
			return s.result(r), nil
		}
		var docs []any
		var expiry any
		status := "denied"
		if allow, _ := data["allow"].(bool); allow {
			if AccessRank(r.access) > 0 && (s.Google == nil || !s.Google.CanWrite()) {
				return nil, errors.New("Reconnect Google to allow writing documents")
			}
			ttl, ttlOK := asInt(data["duration"])
			if _, isBool := data["duration"].(bool); isBool {
				ttlOK = false
			}
			if r.access == "create" {
				if !ttlOK || !validDurations[ttl] {
					return nil, errors.New("invalid duration")
				}
				if _, err = s.DB.Exec("UPDATE requests SET status='creating' WHERE id=?", r.id); err != nil {
					return nil, err
				}
				created, err := s.Google.Create(r.title)
				if err != nil {
					if _, err = s.DB.Exec("UPDATE requests SET status='failed' WHERE id=?", r.id); err != nil {
						return nil, err
					}
					r, err = s.rowLocked(r.id)
					if err != nil {
						return nil, err
					}
					return s.result(r), nil
				}
				docs = []any{created}
				expiry = s.Clock() + float64(ttl)
				status = "granted"
			} else {
				ids, idsOK := data["documents"].([]any)
				seen := map[string]bool{}
				for _, item := range ids {
					id, _ := item.(string)
					seen[id] = true
				}
				if !ttlOK || !validDurations[ttl] || !idsOK || len(ids) < 1 || len(ids) > 20 || len(seen) != len(ids) {
					return nil, errors.New("select 1–20 files and a valid duration")
				}
				blocked, err := s.blockedLocked()
				if err != nil {
					return nil, err
				}
				for id := range seen {
					if blocked[id] {
						return nil, errors.New("a selected document is tagged unsharable with AI")
					}
				}
				for _, item := range ids {
					id, _ := item.(string)
					f, err := s.Google.File(id)
					if err != nil {
						return nil, err
					}
					docs = append(docs, f)
				}
				expiry = s.Clock() + float64(ttl)
				status = "granted"
			}
		}
		if docs == nil {
			docs = []any{}
		}
		at, by := s.resolvedNow(actorOf(data))
		if _, err = s.DB.Exec("UPDATE requests SET status=?,expires=?,documents=?,resolved=?,resolved_by=? WHERE id=?", status, expiry, string(mustJSON(docs)), at, by, r.id); err != nil {
			return nil, err
		}
		r, err = s.rowLocked(r.id)
		if err != nil {
			return nil, err
		}
		return s.result(r), nil
	case "revoke":
		at, by := s.resolvedNow(actorOf(data))
		if _, err := s.DB.Exec("UPDATE requests SET status='revoked',resolved=?,resolved_by=? WHERE id=? AND status='granted'", at, by, stringField(data, "id")); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	case "history":
		// Everything that ever granted or removed access for a sandbox:
		// document requests in every state and repository selections, newest
		// first, capped. Read-only; the console shows it as "Access history".
		sandbox := stringField(data, "sandboxID")
		if !validIdentifier(sandbox) {
			return nil, errors.New("invalid environment")
		}
		rows, err := s.rowsLocked("SELECT "+sharingColumns+" FROM requests WHERE sandbox=? ORDER BY created DESC LIMIT 200", sandbox)
		if err != nil {
			return nil, err
		}
		now := s.Clock()
		events := []any{}
		for _, r := range rows {
			e := s.result(r)
			e["kind"] = "document_request"
			e["expired"] = r.status == "granted" && r.expires.Valid && r.expires.Float64 <= now
			events = append(events, e)
		}
		repoRows, err := s.DB.Query("SELECT at,actor,kind,detail FROM repository_events WHERE sandbox=? OR sandbox='' ORDER BY at DESC LIMIT 200", sandbox)
		if err != nil {
			return nil, err
		}
		defer repoRows.Close()
		for repoRows.Next() {
			var at float64
			var actor, kind, detail string
			if err = repoRows.Scan(&at, &actor, &kind, &detail); err != nil {
				return nil, err
			}
			var parsed any
			_ = json.Unmarshal([]byte(detail), &parsed)
			events = append(events, map[string]any{"kind": kind, "created_at": at, "resolved_by": actor, "repositories": parsed})
		}
		if err = repoRows.Err(); err != nil {
			return nil, err
		}
		sort.SliceStable(events, func(i, j int) bool {
			a, _ := events[i].(map[string]any)["created_at"].(float64)
			b, _ := events[j].(map[string]any)["created_at"].(float64)
			return a > b
		})
		return map[string]any{"events": events}, nil
	case "list":
		rows, err := s.rowsLocked("SELECT "+sharingColumns+" FROM requests WHERE sandbox=? AND status='granted' AND expires>?", stringField(data, "sandboxID"), s.Clock())
		if err != nil {
			return nil, err
		}
		return map[string]any{"grants": resultsOf(s, rows)}, nil
	case "undelivered":
		rows, err := s.rowsLocked("SELECT " + sharingColumns + " FROM requests WHERE status NOT IN ('pending','creating') AND delivered=0")
		if err != nil {
			return nil, err
		}
		requests := resultsOf(s, rows)
		prs, err := s.PullRequests.undeliveredLocked()
		if err != nil {
			return nil, err
		}
		return map[string]any{"requests": append(requests, prs...)}, nil
	case "ack":
		id := stringField(data, "id")
		if _, err := s.DB.Exec("UPDATE requests SET delivered=1 WHERE id=?", id); err != nil {
			return nil, err
		}
		if _, err := s.DB.Exec("UPDATE pull_requests SET delivered=1 WHERE id=?", id); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	return nil, errors.New("unknown sharing operation")
}

// Authorize finds the grant covering one Docs or Sheets API request (a
// grant covers its own access level and below) and returns it with the
// owner's credential.
func (s *Sharing) Authorize(chat, sandbox string, request map[string]any) (map[string]any, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	port, _ := asInt(request["port"])
	if request["scheme"] != "https" || port != 443 || (request["host"] != "docs.googleapis.com" && request["host"] != "sheets.googleapis.com") {
		return nil, "", errors.New("unsupported document authority")
	}
	path, _ := request["path"].(string)
	parsed, err := url.Parse(path)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Fragment != "" {
		return nil, "", errors.New("invalid document route")
	}
	bodyEncoded, _ := request["body_base64"].(string)
	body, err := base64.StdEncoding.Strict().DecodeString(bodyEncoded)
	if err != nil {
		return nil, "", errors.New("invalid document body")
	}
	method, _ := request["method"].(string)
	access, doc, err := DocumentWriteOperation(method, parsed.Path, parseQSL(parsed.RawQuery), body)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.rowsLocked("SELECT "+sharingColumns+" FROM requests WHERE sandbox=? AND status='granted' AND expires>?", sandbox, s.Clock())
	if err != nil {
		return nil, "", err
	}
	var grant map[string]any
	for _, r := range rows {
		if AccessRank(r.access) < AccessRank(access) {
			continue
		}
		var documents []map[string]any
		_ = json.Unmarshal([]byte(r.documents), &documents)
		for _, d := range documents {
			if d["id"] == doc {
				grant = s.result(r)
				break
			}
		}
		if grant != nil {
			break
		}
	}
	if grant == nil {
		return nil, "", errors.New("document is not shared with this environment")
	}
	blocked, err := s.blockedLocked()
	if err != nil {
		return nil, "", err
	}
	if blocked[doc] {
		return nil, "", errors.New("document is tagged unsharable with AI")
	}
	if s.Google == nil {
		return nil, "", errors.New("Google is not configured")
	}
	authorization, err := s.Google.Authorization()
	if err != nil {
		return nil, "", err
	}
	return grant, authorization, nil
}

// Active reports whether a grant still applies to the sandbox.
func (s *Sharing) Active(id, chat, sandbox string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var one int
	err := s.DB.QueryRow("SELECT 1 FROM requests WHERE id=? AND sandbox=? AND status='granted' AND expires>?", id, sandbox, s.Clock()).Scan(&one)
	return err == nil
}

func (s *Sharing) githubDispatch(op string, data map[string]any) (map[string]any, error) {
	if s.GitHub == nil {
		return nil, errors.New("GitHub is not connected")
	}
	owner, app, err := s.GitHub.Identity()
	if err != nil {
		return nil, err
	}
	owner = strings.ToLower(owner)
	if op == "github_repositories" {
		var page int64 = 1
		if v, present := data["page"]; present {
			n, ok := asInt(v)
			if !ok {
				return nil, errors.New("invalid repository page")
			}
			page = n
		}
		return s.GitHub.Repositories(page)
	}
	chat, sandbox := stringField(data, "chatID"), stringField(data, "sandboxID")
	if !validIdentifier(chat) || !validIdentifier(sandbox) {
		return nil, errors.New("invalid conversation")
	}
	if op == "github_select" {
		list, ok := data["repositories"].([]any)
		if !ok || len(list) > 100 {
			return nil, errors.New("select up to 100 repositories")
		}
		// Per-repository read categories: data["access"] maps a name to a
		// non-empty subset of RepositoryReadCategories; a repository absent
		// from the map gets every read category.
		access, err := repositoryAccess(list, data["access"])
		if err != nil {
			return nil, err
		}
		wanted := map[string]bool{}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return nil, errors.New("select up to 100 repositories")
			}
			wanted[strings.ToLower(name)] = true
		}
		if len(wanted) != len(list) {
			return nil, errors.New("select up to 100 repositories")
		}
		// Validate against fresh GitHub metadata before atomically replacing grants.
		found := map[string]map[string]any{}
		var page int64 = 1
		for len(found) < len(wanted) {
			result, err := s.GitHub.Repositories(page)
			if err != nil {
				return nil, err
			}
			repos, _ := result["repositories"].([]any)
			for _, item := range repos {
				repo, _ := item.(map[string]any)
				name := lowerString(repo["full_name"])
				if wanted[name] {
					found[name] = repo
				}
			}
			next, ok := asInt(result["next_page"])
			if !ok || result["next_page"] == nil {
				break
			}
			page = next
		}
		if len(found) != len(wanted) {
			return nil, errors.New("repository is not owned by the connected account or available to Warden")
		}
		s.mu.Lock()
		tx, err := s.DB.Begin()
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if _, err = tx.Exec("DELETE FROM repositories WHERE sandbox=?", sandbox); err != nil {
			tx.Rollback()
			s.mu.Unlock()
			return nil, err
		}
		for _, repo := range found {
			id, _ := asInt(repo["id"])
			if _, err = tx.Exec("INSERT INTO repositories (chat,sandbox,owner,app,name,id,grant_id,principal,access) VALUES (?,?,?,?,?,?,?,?,?)", chat, sandbox, owner, app, lowerString(repo["full_name"]), id, randomHex(32), principalOf(data), access[lowerString(repo["full_name"])]); err != nil {
				tx.Rollback()
				s.mu.Unlock()
				return nil, err
			}
		}
		detail := map[string]any{}
		for _, repo := range found {
			detail[lowerString(repo["full_name"])] = access[lowerString(repo["full_name"])]
		}
		if _, err = tx.Exec("INSERT INTO repository_events (at,sandbox,actor,kind,detail) VALUES (?,?,?,?,?)", s.Clock(), sandbox, actorOf(data), "repositories_selected", string(mustJSON(detail))); err != nil {
			tx.Rollback()
			s.mu.Unlock()
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.mu.Unlock()
	} else if op != "github_list" {
		return nil, errors.New("unknown GitHub sharing operation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.DB.Query("SELECT id,name,access FROM repositories WHERE sandbox=? AND owner=? AND app=? ORDER BY name", sandbox, owner, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repositories := []any{}
	for rows.Next() {
		var id int64
		var name, stored string
		if err = rows.Scan(&id, &name, &stored); err != nil {
			return nil, err
		}
		categories := []any{}
		for _, c := range strings.Split(stored, ",") {
			if c != "" {
				categories = append(categories, c)
			}
		}
		repositories = append(repositories, map[string]any{"id": id, "full_name": name, "url": "https://github.com/" + name, "clone_url": "https://github.com/" + name + ".git", "api_url": "https://api.github.com/repos/" + name,
			"access": categories, "access_summary": "read: " + accessSummary(stored), "expires_at": nil})
	}
	return map[string]any{"owner": owner, "repositories": repositories}, rows.Err()
}

// repositoryAccess resolves the categories each selected repository gets:
// the given map's entry (validated) or every read category.
func repositoryAccess(list []any, raw any) (map[string]string, error) {
	valid := stringSet(RepositoryReadCategories...)
	given, _ := raw.(map[string]any)
	out := map[string]string{}
	for _, item := range list {
		name := lowerString(item)
		entry, present := given[name]
		if !present {
			out[name] = strings.Join(RepositoryReadCategories, ",")
			continue
		}
		items, ok := entry.([]any)
		if !ok || len(items) == 0 {
			return nil, errors.New("access must name at least one read category per repository")
		}
		chosen := map[string]bool{}
		for _, c := range items {
			s, ok := c.(string)
			if !ok || !valid[s] {
				return nil, errors.New("access categories are contents, issues and pull_requests")
			}
			chosen[s] = true
		}
		var sorted []string
		for _, c := range RepositoryReadCategories {
			if chosen[c] {
				sorted = append(sorted, c)
			}
		}
		out[name] = strings.Join(sorted, ",")
	}
	return out, nil
}

// accessSummary words a stored category list for people and agents.
func accessSummary(stored string) string {
	names := map[string]string{"contents": "code", "issues": "issues", "pull_requests": "pull requests"}
	var parts []string
	for _, c := range strings.Split(stored, ",") {
		if n, ok := names[c]; ok {
			parts = append(parts, n)
		}
	}
	if len(parts) == 0 {
		return "metadata only"
	}
	return strings.Join(parts, ", ")
}

// githubWrite performs one small GitHub write Warden executes itself after a
// one-shot owner approval: a comment on an issue or pull request, a new
// issue, or labels on an issue. The repository must be shared with the
// sandbox; the credential never leaves this process.
func (s *Sharing) githubWrite(data map[string]any) (map[string]any, error) {
	if s.GitHub == nil {
		return nil, errors.New("GitHub is not connected")
	}
	sandbox := stringField(data, "sandboxID")
	repo := strings.ToLower(stringField(data, "repository"))
	if !validIdentifier(sandbox) || !repositoryShape.MatchString(repo) {
		return nil, errors.New("invalid repository")
	}
	owner, app, err := s.GitHub.Identity()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var id int64
	err = s.DB.QueryRow("SELECT id FROM repositories WHERE sandbox=? AND name=? AND owner=? AND app=?", sandbox, repo, strings.ToLower(owner), app).Scan(&id)
	s.mu.Unlock()
	if err != nil {
		return nil, errors.New("repository is not shared with this workspace")
	}
	number, _ := asInt(data["number"])
	title := strings.TrimSpace(stringField(data, "title"))
	body := stringField(data, "body")
	var operation, method, path string
	var payload map[string]any
	switch stringField(data, "action") {
	case "comment_issue", "comment_pull_request":
		if number <= 0 || strings.TrimSpace(body) == "" || len(body) > 65536 {
			return nil, errors.New("a comment needs an issue or pull request number and a body")
		}
		operation, method, path = "issues/create-comment", "POST", "/repos/"+repo+"/issues/"+strconv.FormatInt(number, 10)+"/comments"
		payload = map[string]any{"body": body}
	case "create_issue":
		if title == "" || len(title) > 256 || len(body) > 65536 {
			return nil, errors.New("an issue needs a title (256 characters maximum) and an optional body")
		}
		operation, method, path = "issues/create", "POST", "/repos/"+repo+"/issues"
		payload = map[string]any{"title": title, "body": body}
	case "add_labels":
		labels, _ := data["labels"].([]any)
		if number <= 0 || len(labels) == 0 || len(labels) > 20 {
			return nil, errors.New("labels need an issue number and 1–20 label names")
		}
		for _, l := range labels {
			if s, ok := l.(string); !ok || strings.TrimSpace(s) == "" || len(s) > 50 {
				return nil, errors.New("invalid label")
			}
		}
		operation, method, path = "issues/add-labels", "POST", "/repos/"+repo+"/issues/"+strconv.FormatInt(number, 10)+"/labels"
		payload = map[string]any{"labels": labels}
	default:
		return nil, errors.New("action must be comment_issue, comment_pull_request, create_issue or add_labels")
	}
	authorization, err := s.GitHub.Authorization(repo, operation, &id)
	if err != nil {
		return nil, err
	}
	transport := GitHubRequest
	if s.PullRequests != nil && s.PullRequests.Transport != nil {
		transport = s.PullRequests.Transport
	}
	result, err := transport(method, path, strings.TrimPrefix(authorization, "Bearer "), payload)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"action": stringField(data, "action"), "repository": repo, "url": result["html_url"]}
	if n, ok := asInt(result["number"]); ok {
		out["number"] = n
	}
	detail := map[string]any{"action": stringField(data, "action"), "repository": repo, "number": number, "url": result["html_url"]}
	s.mu.Lock()
	_, err = s.DB.Exec("INSERT INTO repository_events (at,sandbox,actor,kind,detail) VALUES (?,?,?,?,?)", s.Clock(), sandbox, actorOf(data), "github_write", string(mustJSON(detail)))
	s.mu.Unlock()
	return out, err
}

// GitHubGrant returns the persistent read grant covering a GitHub request,
// or ok=false when none applies. Errors mean the request must be refused.
func (s *Sharing) GitHubGrant(chat, sandbox string, engine *Engine, request map[string]any) (grantID, authorization string, ok bool, err error) {
	if s.GitHub == nil || !engine.NetworkEnabled() {
		return "", "", false, nil
	}
	owner, app, err := s.GitHub.Identity()
	if err != nil {
		return "", "", false, err
	}
	host, _ := request["host"].(string)
	scheme := "https"
	if v, present := request["scheme"]; present {
		scheme, _ = v.(string)
	}
	var port int64 = 443
	if v, present := request["port"]; present {
		port, _ = asInt(v)
	}
	if (host != "api.github.com" && host != "github.com") || scheme != "https" || port != 443 {
		return "", "", false, nil
	}
	n, err := engine.Normalize(request)
	if err != nil {
		return "", "", false, err
	}
	if n.Operation == nil {
		return "", "", false, nil
	}
	perms, err := GitHubPermissions(n.Operation.OperationID)
	if err != nil {
		return "", "", false, err
	}
	for _, v := range perms {
		if v != "read" {
			return "", "", false, nil
		}
	}
	method, _ := request["method"].(string)
	body, _ := request["body_base64"].(string)
	if host == "api.github.com" && (method != "GET" || body != "") {
		return "", "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64
	var stored string
	err = s.DB.QueryRow("SELECT id,grant_id,access FROM repositories WHERE sandbox=? AND name=? AND owner=? AND app=?", sandbox, strings.ToLower(n.Repository), strings.ToLower(owner), app).Scan(&id, &grantID, &stored)
	if err != nil {
		return "", "", false, nil
	}
	// The selection covers only the read categories chosen for it;
	// metadata always. Anything else is refused as unshared.
	if category, ok := GitHubReadCategory(n.Operation.OperationID); !ok || (category != "metadata" && !stringSet(strings.Split(stored, ",")...)[category]) {
		return "", "", false, nil
	}
	authorization, err = s.GitHub.Authorization(n.Repository, n.Operation.OperationID, &id)
	if err != nil {
		return "", "", false, err
	}
	return grantID, authorization, true, nil
}

// GitHubActive reports whether a repository grant still exists.
func (s *Sharing) GitHubActive(grant, chat, sandbox string) bool {
	if s.GitHub == nil {
		return false
	}
	owner, app, err := s.GitHub.Identity()
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var one int
	err = s.DB.QueryRow("SELECT 1 FROM repositories WHERE grant_id=? AND sandbox=? AND owner=? AND app=?", grant, sandbox, strings.ToLower(owner), app).Scan(&one)
	return err == nil
}
