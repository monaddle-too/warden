package cloudauth

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const externalSchema = `
CREATE TABLE IF NOT EXISTS warden_cloud.agent_clients (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, redirects JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS warden_cloud.agent_requests (
 id TEXT PRIMARY KEY, client_id TEXT NOT NULL REFERENCES warden_cloud.agent_clients(id),
 redirect_uri TEXT NOT NULL, state TEXT NOT NULL, challenge TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL);
CREATE TABLE IF NOT EXISTS warden_cloud.agent_connections (
 id TEXT PRIMARY KEY, client_id TEXT NOT NULL REFERENCES warden_cloud.agent_clients(id),
 user_id TEXT NOT NULL REFERENCES warden_cloud.users(id), organization_id TEXT NOT NULL REFERENCES warden_cloud.organizations(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_used_at TIMESTAMPTZ, revoked_at TIMESTAMPTZ);
CREATE INDEX IF NOT EXISTS agent_connections_user ON warden_cloud.agent_connections(user_id);
CREATE TABLE IF NOT EXISTS warden_cloud.agent_codes (
 digest TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES warden_cloud.agent_connections(id),
 redirect_uri TEXT NOT NULL, challenge TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL);
CREATE TABLE IF NOT EXISTS warden_cloud.agent_tokens (
 digest TEXT PRIMARY KEY, connection_id TEXT NOT NULL REFERENCES warden_cloud.agent_connections(id),
 kind TEXT NOT NULL CHECK(kind IN ('access','refresh')), expires_at TIMESTAMPTZ NOT NULL, used_at TIMESTAMPTZ);
CREATE INDEX IF NOT EXISTS agent_tokens_connection ON warden_cloud.agent_tokens(connection_id);
CREATE TABLE IF NOT EXISTS warden_cloud.shared_conversations (
 id TEXT PRIMARY KEY, organization_id TEXT NOT NULL REFERENCES warden_cloud.organizations(id),
 user_id TEXT NOT NULL REFERENCES warden_cloud.users(id), connection_id TEXT NOT NULL REFERENCES warden_cloud.agent_connections(id),
 operation_id TEXT NOT NULL, fingerprint TEXT NOT NULL, title TEXT NOT NULL, messages JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE(connection_id,operation_id));
CREATE INDEX IF NOT EXISTS shared_conversations_org ON warden_cloud.shared_conversations(organization_id,created_at);
`

// ExternalRoute is deliberately separate from browser Role: an agent bearer
// can never become a Warden runtime, preview, policy or administrator session.
func (a *Auth) ExternalRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if p != "/mcp" && !strings.HasPrefix(p, "/oauth/") && p != "/.well-known/oauth-authorization-server" && p != "/.well-known/oauth-protected-resource" && p != "/.well-known/oauth-protected-resource/mcp" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Public OAuth/MCP endpoints support browser-based clients without cookies.
	// Consent and account APIs use the existing same-origin session/CSRF checks.
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname()))) || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			http.Error(w, "origin refused", 403)
			return true
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version, MCP-Session-Id")
		w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return true
	}
	switch p {
	case "/.well-known/oauth-authorization-server":
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			break
		}
		respond(w, 200, map[string]any{"issuer": a.Origin, "authorization_endpoint": a.Origin + "/oauth/authorize", "token_endpoint": a.Origin + "/oauth/token", "registration_endpoint": a.Origin + "/oauth/register", "revocation_endpoint": a.Origin + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"panta"}})
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			break
		}
		respond(w, 200, map[string]any{"resource": a.Origin + "/mcp", "authorization_servers": []string{a.Origin}, "scopes_supported": []string{"panta"}, "bearer_methods_supported": []string{"header"}, "resource_name": "Warden documents and shared conversations"})
	case "/oauth/register":
		a.registerAgent(w, r)
	case "/oauth/authorize":
		a.authorizeAgent(w, r)
	case "/oauth/token":
		a.agentToken(w, r)
	case "/oauth/revoke":
		a.revokeAgentToken(w, r)
	case "/mcp":
		a.agentMCP(w, r)
	default:
		http.NotFound(w, r)
	}
	return true
}
func oauthError(w http.ResponseWriter, status int, code, description string) {
	respond(w, status, map[string]string{"error": code, "error_description": description})
}
func loopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && u.Host != "" && u.User == nil && u.Fragment == "" && u.Opaque == "" && !strings.ContainsAny(raw, "\r\n") && (u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname()))
}

// DB-backed limits are shared by replicas and survive process restarts.
func (a *Auth) externalLimit(r *http.Request, category string, count int) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	key := "external:" + category + ":" + host
	tx, err := a.Login.Store.db.BeginTx(r.Context(), nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return false
	}
	var n int
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM warden_cloud.login_attempts WHERE key=$1 AND at>now()-interval '1 hour'`, key).Scan(&n); err != nil || n >= count {
		return false
	}
	if _, err = tx.ExecContext(r.Context(), `DELETE FROM warden_cloud.login_attempts WHERE key=$1 AND at<now()-interval '1 hour'`, key); err != nil {
		return false
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO warden_cloud.login_attempts(key) VALUES($1)`, key); err != nil {
		return false
	}
	return tx.Commit() == nil
}
func (a *Auth) registerAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !a.externalLimit(r, "register", 60) {
		oauthError(w, 429, "slow_down", "Try registration later")
		return
	}
	// RFC 7591 clients often include optional metadata. Ignore unknown metadata;
	// never fetch client URLs or accept client secrets / additional grant types.
	var in struct {
		Name      string   `json:"client_name"`
		Redirects []string `json:"redirect_uris"`
		Method    string   `json:"token_endpoint_auth_method"`
		Grants    []string `json:"grant_types"`
		Responses []string `json:"response_types"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if d.Decode(&in) != nil {
		oauthError(w, 400, "invalid_client_metadata", "Invalid client metadata")
		return
	}
	name, err := ValidName(in.Name)
	if err != nil {
		name = "External agent"
	}
	if len(in.Redirects) == 0 || len(in.Redirects) > 10 || in.Method != "" && in.Method != "none" {
		oauthError(w, 400, "invalid_client_metadata", "A public client and 1–10 callback URLs are required")
		return
	}
	for _, v := range in.Redirects {
		if !validRedirect(v) {
			oauthError(w, 400, "invalid_redirect_uri", "Use HTTPS or a loopback HTTP callback without a fragment")
			return
		}
	}
	for _, v := range in.Grants {
		if v != "authorization_code" && v != "refresh_token" {
			oauthError(w, 400, "invalid_client_metadata", "Unsupported grant type")
			return
		}
	}
	for _, v := range in.Responses {
		if v != "code" {
			oauthError(w, 400, "invalid_client_metadata", "Only code responses are supported")
			return
		}
	}
	id := ID()
	raw, _ := json.Marshal(in.Redirects)
	if _, err = a.Login.Store.db.ExecContext(r.Context(), `INSERT INTO warden_cloud.agent_clients(id,name,redirects) VALUES($1,$2,$3)`, id, name, raw); err != nil {
		oauthError(w, 503, "server_error", "Registration unavailable")
		return
	}
	respond(w, 201, map[string]any{"client_id": id, "client_id_issued_at": time.Now().Unix(), "client_name": name, "redirect_uris": in.Redirects, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

var pkceChallenge = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var pkceVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

func (a *Auth) authorizeAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	q := r.URL.Query()
	var redirects []string
	var raw []byte
	err := a.Login.Store.db.QueryRowContext(r.Context(), `SELECT redirects FROM warden_cloud.agent_clients WHERE id=$1`, q.Get("client_id")).Scan(&raw)
	_ = json.Unmarshal(raw, &redirects)
	valid := false
	for _, v := range redirects {
		if v == q.Get("redirect_uri") {
			valid = true
		}
	}
	if err != nil || !valid {
		oauthError(w, 400, "invalid_request", "Unknown client or callback URL")
		return
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !pkceChallenge.MatchString(q.Get("code_challenge")) || q.Get("resource") != a.Origin+"/mcp" || len(q.Get("state")) > 2048 || q.Get("scope") != "" && q.Get("scope") != "panta" {
		oauthError(w, 400, "invalid_request", "Use code, S256 PKCE, the MCP resource URL and panta scope")
		return
	}
	if !a.externalLimit(r, "authorize", 120) {
		oauthError(w, 429, "slow_down", "Try authorization later")
		return
	}
	id := randomSecret()
	_, err = a.Login.Store.db.ExecContext(r.Context(), `INSERT INTO warden_cloud.agent_requests(id,client_id,redirect_uri,state,challenge,expires_at) VALUES($1,$2,$3,$4,$5,now()+interval '10 minutes')`, id, q.Get("client_id"), q.Get("redirect_uri"), q.Get("state"), q.Get("code_challenge"))
	if err != nil {
		oauthError(w, 503, "server_error", "Authorization unavailable")
		return
	}
	http.Redirect(w, r, "/connect?request="+url.QueryEscape(id), http.StatusSeeOther)
}
func (a *Auth) agentAuthorization(w http.ResponseWriter, r *http.Request) {
	s, err := a.current(r)
	if err != nil {
		http.Error(w, "Sign in required", 401)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/auth/agent-authorization/")
	var client, name, redirect, state, challenge string
	err = a.Login.Store.db.QueryRowContext(r.Context(), `SELECT c.id,c.name,q.redirect_uri,q.state,q.challenge FROM warden_cloud.agent_requests q JOIN warden_cloud.agent_clients c ON c.id=q.client_id WHERE q.id=$1 AND q.expires_at>now()`, id).Scan(&client, &name, &redirect, &state, &challenge)
	if err != nil {
		http.Error(w, "Connection request expired. Start again from your agent.", 410)
		return
	}
	if r.Method == "GET" {
		respond(w, 200, map[string]string{"clientName": name, "redirectUri": redirect})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !a.csrf(r, s) {
		http.Error(w, "Reload before connecting", 403)
		return
	}
	var in struct {
		Organization string `json:"organizationId"`
		Allow        bool   `json:"allow"`
	}
	if decode(r, &in) != nil {
		http.Error(w, "Invalid consent", 400)
		return
	}
	if in.Allow {
		orgs, e := a.Login.Store.Organizations(r.Context(), s.User.ID, s.User.FullAdmin)
		found := false
		for _, o := range orgs {
			if o.ID == in.Organization {
				found = true
			}
		}
		if e != nil || !found {
			http.Error(w, "Organization unavailable", 403)
			return
		}
	}
	tx, err := a.Login.Store.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "Connection unavailable", 503)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `DELETE FROM warden_cloud.agent_requests WHERE id=$1 AND expires_at>now()`, id)
	if err != nil {
		http.Error(w, "Connection unavailable", 503)
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		http.Error(w, "Connection request expired", 410)
		return
	}
	callback, _ := url.Parse(redirect)
	q := callback.Query()
	q.Set("state", state)
	if in.Allow {
		connection, code := ID(), randomSecret()
		_, err = tx.ExecContext(r.Context(), `INSERT INTO warden_cloud.agent_connections(id,client_id,user_id,organization_id) VALUES($1,$2,$3,$4)`, connection, client, s.User.ID, in.Organization)
		if err == nil {
			_, err = tx.ExecContext(r.Context(), `INSERT INTO warden_cloud.agent_codes(digest,connection_id,redirect_uri,challenge,expires_at) VALUES($1,$2,$3,$4,now()+interval '2 minutes')`, hashSecret(code), connection, redirect, challenge)
		}
		if err != nil {
			http.Error(w, "Connection unavailable", 503)
			return
		}
		q.Set("code", code)
	} else {
		q.Set("error", "access_denied")
	}
	if tx.Commit() != nil {
		http.Error(w, "Connection unavailable", 503)
		return
	}
	callback.RawQuery = q.Encode()
	respond(w, 200, map[string]string{"redirect": callback.String()})
}

// The predicate is evaluated on every token use, including refresh. Session
// organization switches have no effect on this explicitly chosen grant.
const activeConnection = `c.revoked_at IS NULL AND NOT u.disabled AND (u.full_admin OR EXISTS(SELECT 1 FROM warden_cloud.memberships m WHERE m.user_id=c.user_id AND m.organization_id=c.organization_id))`

func (a *Auth) agentToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		oauthError(w, 400, "invalid_request", "Invalid token request")
		return
	}
	p := r.PostForm
	grant := p.Get("grant_type")
	if (grant != "authorization_code" && grant != "refresh_token") || p.Get("resource") != a.Origin+"/mcp" || p.Get("client_id") == "" || p.Get("scope") != "" && p.Get("scope") != "panta" {
		oauthError(w, 400, "invalid_request", "Use the registered client and MCP resource URL")
		return
	}
	tx, err := a.Login.Store.db.BeginTx(r.Context(), nil)
	if err != nil {
		oauthError(w, 503, "server_error", "Token service unavailable")
		return
	}
	defer tx.Rollback()
	var connection, redirect, challenge string
	if grant == "authorization_code" {
		err = tx.QueryRowContext(r.Context(), `SELECT c.id,k.redirect_uri,k.challenge FROM warden_cloud.agent_codes k JOIN warden_cloud.agent_connections c ON c.id=k.connection_id JOIN warden_cloud.users u ON u.id=c.user_id WHERE k.digest=$1 AND k.expires_at>now() AND c.client_id=$2 AND `+activeConnection+` FOR UPDATE OF k,c`, hashSecret(p.Get("code")), p.Get("client_id")).Scan(&connection, &redirect, &challenge)
		hash := sha256.Sum256([]byte(p.Get("code_verifier")))
		if err != nil || redirect != p.Get("redirect_uri") || !pkceVerifier.MatchString(p.Get("code_verifier")) || base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
			oauthError(w, 400, "invalid_grant", "Invalid or expired authorization code")
			return
		}
		_, err = tx.ExecContext(r.Context(), `DELETE FROM warden_cloud.agent_codes WHERE digest=$1`, hashSecret(p.Get("code")))
	} else {
		var used sql.NullTime
		err = tx.QueryRowContext(r.Context(), `SELECT c.id,t.used_at FROM warden_cloud.agent_tokens t JOIN warden_cloud.agent_connections c ON c.id=t.connection_id JOIN warden_cloud.users u ON u.id=c.user_id WHERE t.digest=$1 AND t.kind='refresh' AND t.expires_at>now() AND c.client_id=$2 AND `+activeConnection+` FOR UPDATE OF t,c`, hashSecret(p.Get("refresh_token")), p.Get("client_id")).Scan(&connection, &used)
		if err != nil {
			oauthError(w, 400, "invalid_grant", "Refresh token expired or disconnected")
			return
		}
		if used.Valid {
			if _, err = tx.ExecContext(r.Context(), `UPDATE warden_cloud.agent_connections SET revoked_at=now() WHERE id=$1`, connection); err == nil {
				err = tx.Commit()
			}
			oauthError(w, 400, "invalid_grant", "Refresh token already used. Reconnect the agent.")
			return
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE warden_cloud.agent_tokens SET used_at=now() WHERE digest=$1`, hashSecret(p.Get("refresh_token")))
	}
	if err != nil {
		oauthError(w, 503, "server_error", "Token service unavailable")
		return
	}
	access, refresh := randomSecret(), randomSecret()
	_, err = tx.ExecContext(r.Context(), `INSERT INTO warden_cloud.agent_tokens(digest,connection_id,kind,expires_at) VALUES($1,$3,'access',now()+interval '1 hour'),($2,$3,'refresh',now()+interval '30 days')`, hashSecret(access), hashSecret(refresh), connection)
	if err != nil || tx.Commit() != nil {
		oauthError(w, 503, "server_error", "Token service unavailable")
		return
	}
	respond(w, 200, map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600, "scope": "panta"})
}
func (a *Auth) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		oauthError(w, 400, "invalid_request", "Invalid revocation")
		return
	}
	_, err := a.Login.Store.db.ExecContext(r.Context(), `UPDATE warden_cloud.agent_connections c SET revoked_at=now() FROM warden_cloud.agent_tokens t WHERE t.connection_id=c.id AND t.digest=$1 AND c.client_id=$2`, hashSecret(r.PostForm.Get("token")), r.PostForm.Get("client_id"))
	if err != nil {
		oauthError(w, 503, "server_error", "Revocation unavailable")
		return
	}
	w.WriteHeader(200)
}

type agentConnection struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	OrganizationID   string     `json:"organizationId"`
	OrganizationName string     `json:"organizationName"`
	UserID           string     `json:"-"`
	CreatedAt        time.Time  `json:"createdAt"`
	LastUsedAt       *time.Time `json:"lastUsedAt"`
}

func (a *Auth) bearerConnection(r *http.Request) (agentConnection, error) {
	var c agentConnection
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) > 128 {
		return c, errors.New("bearer required")
	}
	err := a.Login.Store.db.QueryRowContext(r.Context(), `SELECT c.id,c.user_id,c.organization_id FROM warden_cloud.agent_tokens t JOIN warden_cloud.agent_connections c ON c.id=t.connection_id JOIN warden_cloud.users u ON u.id=c.user_id WHERE t.digest=$1 AND t.kind='access' AND t.expires_at>now() AND `+activeConnection, hashSecret(fields[1])).Scan(&c.ID, &c.UserID, &c.OrganizationID)
	if err == nil {
		_, err = a.Login.Store.db.ExecContext(r.Context(), `UPDATE warden_cloud.agent_connections SET last_used_at=now() WHERE id=$1 AND (last_used_at IS NULL OR last_used_at<now()-interval '1 minute')`, c.ID)
	}
	return c, err
}
func (a *Auth) agentConnections(w http.ResponseWriter, r *http.Request) {
	s, err := a.current(r)
	if err != nil {
		http.Error(w, "Sign in required", 401)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/auth/agent-connections" {
		rows, err := a.Login.Store.db.QueryContext(r.Context(), `SELECT c.id,a.name,c.organization_id,o.name,c.created_at,c.last_used_at FROM warden_cloud.agent_connections c JOIN warden_cloud.agent_clients a ON a.id=c.client_id JOIN warden_cloud.organizations o ON o.id=c.organization_id WHERE c.user_id=$1 AND c.revoked_at IS NULL ORDER BY c.created_at DESC`, s.User.ID)
		if err != nil {
			http.Error(w, "Connections unavailable", 503)
			return
		}
		defer rows.Close()
		items := []agentConnection{}
		for rows.Next() {
			var c agentConnection
			if rows.Scan(&c.ID, &c.Name, &c.OrganizationID, &c.OrganizationName, &c.CreatedAt, &c.LastUsedAt) != nil {
				http.Error(w, "Connections unavailable", 503)
				return
			}
			items = append(items, c)
		}
		if rows.Err() != nil {
			http.Error(w, "Connections unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"url": a.Origin + "/mcp", "connections": items})
		return
	}
	if r.Method == "DELETE" && a.csrf(r, s) {
		id := strings.TrimPrefix(r.URL.Path, "/auth/agent-connections/")
		_, err = a.Login.Store.db.ExecContext(r.Context(), `UPDATE warden_cloud.agent_connections SET revoked_at=now() WHERE id=$1 AND user_id=$2`, id, s.User.ID)
		if err != nil {
			http.Error(w, "Disconnect unavailable", 503)
			return
		}
		w.WriteHeader(204)
		return
	}
	http.Error(w, "Request refused", 403)
}
