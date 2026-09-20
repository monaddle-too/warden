package policy

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// EngineOptions configures a per-sandbox policy engine.
type EngineOptions struct {
	Operations     *Operations
	PolicyTemplate string // path of config/policy.template.json
	Clock          Clock  // wall clock
	Monotonic      Clock  // monotonic clock
	GitHubApp      GitHubCredentials
	// EgressMode, when set ("restricted" or "public"), overrides the egress
	// mode of the sandbox's stored policy at every load: the operator's
	// sandboxes.egress setting is authoritative, whatever the policy file
	// copied from the template said when the sandbox was created.
	EgressMode string
}

// Engine holds one sandbox's policy, approvals, grants, decisions and audit.
type Engine struct {
	State      string
	Redactor   *Redactor
	Audit      *Audit
	Operations *Operations
	Figma      *OAuthConnection
	GoogleDocs *OAuthConnection
	// GitHubApp is the host GitHub credential source (App broker or user
	// token); the field name predates the user-token source.
	GitHubApp GitHubCredentials
	DB        *sql.DB
	Policy    map[string]any

	mu               sync.Mutex
	clock, monotonic Clock
	startedWall      float64
	startedMono      float64
	token            string
	gitReviews       map[string]reviewEntry
	restReviews      map[string]reviewEntry
	reviewOrder      []string
	restOrder        []string
	networkDecisions map[string]float64
	networkEnabled   bool
	hostAllows       map[string]float64 // host -> unix expiry of an owner-approved temporary allow
	fingerprintKey   []byte
	policyPath       string
}

type reviewEntry struct {
	expires float64
	value   map[string]any
}

var methodShape = regexp.MustCompile(`^[A-Z]+$`)
var canonicalHostShape = regexp.MustCompile(`^[a-z0-9.-]+$`)
var headerNameShape = regexp.MustCompile("^[a-z0-9!#$%&'*+.^_`|~-]+$")
var jsonPointerShape = regexp.MustCompile(`^(?:/(?:[^~/]|~[01])*)+$`)
var manualTokenShape = regexp.MustCompile(`^(?:github_pat_|ghp_|gho_)[A-Za-z0-9_]{20,}$`)

var strippedRequestHeaders = stringSet("authorization", "proxy-authorization", "cookie", "host", "content-length", "transfer-encoding", "connection", "x-figma-token", "x-goog-api-key", "x-goog-user-project")

// NewEngine opens or initialises the engine state directory.
func NewEngine(state string, options EngineOptions) (*Engine, error) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return nil, err
	}
	e := &Engine{State: state, Redactor: NewRedactor(), Operations: options.Operations, GitHubApp: options.GitHubApp,
		clock: options.Clock, monotonic: options.Monotonic, gitReviews: map[string]reviewEntry{}, restReviews: map[string]reviewEntry{},
		networkDecisions: map[string]float64{}, fingerprintKey: randomBytes(32), policyPath: filepath.Join(state, "policy.json")}
	if e.clock == nil {
		e.clock = wallClock
	}
	if e.monotonic == nil {
		e.monotonic = monotonicClock
	}
	e.startedWall, e.startedMono = e.clock(), e.monotonic()
	e.Figma = NewFigmaConnection(e.Redactor, e.Now)
	e.GoogleDocs = NewGoogleDocsConnection(e.Redactor, e.Now)
	_, err := os.Stat(filepath.Join(state, "network-disconnected"))
	e.networkEnabled = err != nil
	audit, err := NewAudit(filepath.Join(state, "audit", "events.jsonl"), e.Redactor)
	if err != nil {
		return nil, err
	}
	e.Audit = audit
	fail := func(err error) (*Engine, error) {
		audit.Close()
		if e.DB != nil {
			e.DB.Close()
		}
		return nil, err
	}
	if e.Operations == nil {
		return fail(errors.New("operation catalog required"))
	}
	if _, err = os.Stat(e.policyPath); err != nil {
		template, err := os.ReadFile(options.PolicyTemplate)
		if err != nil {
			return fail(err)
		}
		if err = os.WriteFile(e.policyPath, template, 0o600); err != nil {
			return fail(err)
		}
	}
	raw, err := os.ReadFile(e.policyPath)
	if err != nil {
		return fail(err)
	}
	parsed, err := StrictJSON(raw)
	if err != nil {
		return fail(err)
	}
	policy, ok := parsed.(map[string]any)
	if !ok {
		return fail(errors.New("policy has missing or unknown fields"))
	}
	if options.EgressMode != "" {
		egress, _ := policy["egress"].(map[string]any)
		if egress == nil {
			egress = map[string]any{"destinations": []any{}}
			policy["egress"] = egress
		}
		egress["mode"] = options.EgressMode
	}
	if err = ValidatePolicy(policy); err != nil {
		return fail(err)
	}
	e.Policy = policy
	db, err := openSQLite(filepath.Join(state, "control.sqlite"))
	if err != nil {
		return fail(err)
	}
	e.DB = db
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY, fingerprint TEXT, operation TEXT, path TEXT, repository TEXT, summary TEXT, status TEXT, created REAL)`,
		`CREATE TABLE IF NOT EXISTS grants(id TEXT PRIMARY KEY, request_id TEXT, kind TEXT, fingerprint TEXT, operation TEXT, path TEXT, predicates TEXT, expires REAL, remaining INTEGER, revoked INTEGER DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS decisions(id TEXT PRIMARY KEY, grant_id TEXT, expires REAL)`,
		`CREATE INDEX IF NOT EXISTS requests_created_id ON requests(created DESC,id DESC)`,
		`CREATE INDEX IF NOT EXISTS requests_status_created_id ON requests(status,created DESC,id DESC)`,
	} {
		if _, err = db.Exec(statement); err != nil {
			return fail(err)
		}
	}
	if _, err = scrubRequests(db); err != nil {
		return fail(err)
	}
	// Authorization never survives a control-plane restart or clock reset.
	if _, err = db.Exec("UPDATE grants SET revoked=1"); err != nil {
		return fail(err)
	}
	if _, err = db.Exec("UPDATE requests SET status='stale' WHERE status='pending'"); err != nil {
		return fail(err)
	}
	if _, err = audit.Emit("system.started", map[string]any{"operation_catalog_revision": e.Operations.Revision}); err != nil {
		return fail(err)
	}
	return e, nil
}

func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the database and audit file.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.DB != nil {
		e.DB.Close()
		e.DB = nil
	}
	e.Audit.Close()
}

// Now is a wall clock that cannot run backwards within this process.
func (e *Engine) Now() float64 {
	now := e.clock()
	if mono := e.startedWall + e.monotonic() - e.startedMono; mono > now {
		return mono
	}
	return now
}

// NetworkEnabled reports the owner's network switch.
func (e *Engine) NetworkEnabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.networkEnabled
}

// ValidatePolicy checks the local policy document.
func ValidatePolicy(value map[string]any) error {
	required := stringSet("version", "max_grant_seconds", "deny_operations", "deny_repositories", "max_request_bytes")
	optional := stringSet("egress", "allowed_repositories", "allowed_figma_files", "allowed_google_documents")
	for key := range required {
		if _, ok := value[key]; !ok {
			return errors.New("policy has missing or unknown fields")
		}
	}
	for key := range value {
		if !required[key] && !optional[key] {
			return errors.New("policy has missing or unknown fields")
		}
	}
	if v, ok := asInt(value["version"]); !ok || v != 1 {
		return errors.New("unsupported policy version")
	}
	for name, maximum := range map[string]int64{"max_grant_seconds": 86400, "max_request_bytes": 8388608} {
		v, ok := asInt(value[name])
		if _, isBool := value[name].(bool); !ok || isBool || v < 1 || v > maximum {
			return errors.New("invalid " + name)
		}
	}
	for _, name := range []string{"deny_operations", "deny_repositories"} {
		list, ok := value[name].([]any)
		if !ok {
			return errors.New("invalid " + name)
		}
		for _, item := range list {
			if _, ok := item.(string); !ok {
				return errors.New("invalid " + name)
			}
		}
	}
	if egress, ok := value["egress"]; ok {
		if err := ValidateEgress(egress); err != nil {
			return err
		}
	}
	if raw, ok := value["allowed_repositories"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return errors.New("invalid repository allowlist")
		}
		for _, item := range list {
			if s, ok := item.(string); !ok || !repoPattern.MatchString(s) {
				return errors.New("invalid repository allowlist")
			}
		}
	}
	if raw, ok := value["allowed_figma_files"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return errors.New("invalid Figma file allowlist")
		}
		for _, item := range list {
			if s, ok := item.(string); !ok || !figmaFileKey.MatchString(s) {
				return errors.New("invalid Figma file allowlist")
			}
		}
	}
	if raw, ok := value["allowed_google_documents"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return errors.New("invalid Google document allowlist")
		}
		for _, item := range list {
			if s, ok := item.(string); !ok || !googleDocumentID.MatchString(s) {
				return errors.New("invalid Google document allowlist")
			}
		}
	}
	return nil
}

// githubOwnerMismatch applies the App broker's owner boundary: installation
// tokens only ever cover the configured owner. A user token has no owner
// boundary (the selection and approvals are the boundary), and a user file
// that cannot be read fails later at credential time.
func githubOwnerMismatch(source GitHubCredentials, repository string) bool {
	owner, appID, err := source.Identity()
	if err != nil || appID == 0 {
		return false
	}
	return repository == "" || strings.ToLower(strings.SplitN(repository, "/", 2)[0]) != strings.ToLower(owner)
}

func (e *Engine) policyInt(name string) int64 {
	v, _ := asInt(e.Policy[name])
	return v
}

// SetToken installs a manual GitHub token (unavailable in App mode).
func (e *Engine) SetToken(token string) error {
	if e.GitHubApp != nil {
		if _, appID, _ := e.GitHubApp.Identity(); appID == 0 {
			return errors.New("the GitHub sign-in file manages credentials; manual tokens are disabled")
		}
		return errors.New("GitHub App broker manages credentials; manual tokens are disabled")
	}
	if !manualTokenShape.MatchString(token) {
		return errors.New("expected a GitHub personal access or OAuth token")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Redactor.Register(token)
	e.token = token
	_, err := e.Audit.Emit("credential.configured", map[string]any{"credential": map[string]any{"provider": "github", "storage": "host_memory"}})
	return err
}

// TokenConfigured reports whether a manual token is present.
func (e *Engine) TokenConfigured() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.token != ""
}

// SavePolicy replaces the policy and revokes every grant.
func (e *Engine) SavePolicy(policy map[string]any) error {
	if err := ValidatePolicy(policy); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.Audit.Emit("policy.updated", map[string]any{"policy": policy}); err != nil {
		return err
	}
	tmp := e.policyPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(Dumps(policy)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, e.policyPath); err != nil {
		return err
	}
	e.Policy = policy
	e.networkDecisions = map[string]float64{}
	_, err := e.DB.Exec("UPDATE grants SET revoked=1")
	return err
}

// SetEgressMode changes only the egress mode of the stored policy
// ("restricted" or "public"). Unlike SavePolicy it keeps every grant: they
// cover brokered operations, which the mode does not govern. In-flight
// external egress leases are dropped so a narrowing takes effect on open
// connections, whose watchers then close them.
func (e *Engine) SetEgressMode(mode string) error {
	if mode != "restricted" && mode != "public" {
		return errors.New("egress mode must be restricted or public")
	}
	policy := e.PolicyCopy()
	egress, _ := policy["egress"].(map[string]any)
	if egress == nil {
		egress = map[string]any{"destinations": []any{}}
		policy["egress"] = egress
	}
	if egress["mode"] == mode {
		return nil
	}
	egress["mode"] = mode
	if err := ValidatePolicy(policy); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.Audit.Emit("policy.updated", map[string]any{"policy": policy, "reason": "egress mode " + mode}); err != nil {
		return err
	}
	tmp := e.policyPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(Dumps(policy)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, e.policyPath); err != nil {
		return err
	}
	e.Policy = policy
	e.networkDecisions = map[string]float64{}
	return nil
}

// PolicyDigest identifies the current policy for enforcement proofs.
func (e *Engine) PolicyDigest() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return sha256Hex([]byte(Dumps(e.Policy)))
}

// PolicyCopy returns the current policy document.
func (e *Engine) PolicyCopy() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	copied, _ := cloneJSON(e.Policy).(map[string]any)
	return copied
}

// RevokeAll revokes every grant and in-flight network decision.
func (e *Engine) RevokeAll() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.revokeAllLocked()
}

func (e *Engine) revokeAllLocked() error {
	if _, err := e.Audit.Emit("approval.revoked_all", map[string]any{"actor": map[string]any{"type": "human"}}); err != nil {
		return err
	}
	if _, err := e.DB.Exec("UPDATE grants SET revoked=1"); err != nil {
		return err
	}
	e.networkDecisions = map[string]float64{}
	return nil
}

// ClearNetworkDecisions drops in-flight egress decisions without audit.
func (e *Engine) ClearNetworkDecisions() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.networkDecisions = map[string]float64{}
}

// SetNetwork persists the deny state before acknowledging it.
func (e *Engine) SetNetwork(enabled bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	flag := filepath.Join(e.State, "network-disconnected")
	if !enabled {
		if err := os.WriteFile(flag, []byte("disconnected\n"), 0o600); err != nil {
			return err
		}
		e.networkEnabled = false
	} else {
		if err := os.Remove(flag); err != nil && !os.IsNotExist(err) {
			return err
		}
		e.networkEnabled = true
	}
	if err := e.revokeAllLocked(); err != nil {
		return err
	}
	event := "network.disconnected"
	if enabled {
		event = "network.connected"
	}
	_, err := e.Audit.Emit(event, nil)
	return err
}

func (e *Engine) egressPolicy() map[string]any {
	if policy, ok := e.Policy["egress"].(map[string]any); ok {
		return policy
	}
	return map[string]any{"mode": "public", "destinations": []any{}}
}

// AuthorizeEgress decides a non-provider destination request.
func (e *Engine) AuthorizeEgress(request map[string]any) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	host, _ := request["host"].(string)
	method, _ := request["method"].(string)
	scheme := "https"
	if s, ok := request["scheme"].(string); ok {
		scheme = s
	}
	tls, _ := request["tls"].(bool)
	reason := "destination policy"
	allowed := e.networkEnabled && EgressPermits(e.egressPolicy(), host, method, scheme, tls)
	if !allowed && e.networkEnabled && !tls && (scheme == "https" || scheme == "http") && e.hostAllowedLocked(host) {
		allowed, reason = true, "temporary grant"
	}
	event := "egress.denied"
	if allowed {
		event = "egress.allowed"
	}
	if _, err := e.Audit.Emit(event, map[string]any{"hostname": host, "request": map[string]any{"method": method, "host": host, "scheme": scheme}, "reason": reason}); err != nil {
		return nil, err
	}
	if !allowed {
		return map[string]any{"allow": false, "reason": "network disconnected or destination not allowed", "status": 403}, nil
	}
	now := e.Now()
	for id, expires := range e.networkDecisions {
		if expires <= now {
			delete(e.networkDecisions, id)
		}
	}
	if len(e.networkDecisions) >= 4096 {
		return nil, errors.New("too many active network requests")
	}
	decision := UUID4()
	e.networkDecisions[decision] = now + 3600
	return map[string]any{"allow": true, "decision_id": decision, "remaining_seconds": 3600, "request_id": UUID4()}, nil
}

// AllowHost grants this sandbox HTTP and HTTPS egress to one public host
// until the given unix time, beside the policy's destination list: the
// owner approved an agent's request_network_access. It lives in memory (a
// policy service restart forgets it; the agent asks again) and is audited.
func (e *Engine) AllowHost(host string, until float64) error {
	if !ValidHost(host) {
		return errors.New("a canonical public DNS hostname is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.hostAllows == nil {
		e.hostAllows = map[string]float64{}
	}
	e.hostAllows[host] = until
	_, err := e.Audit.Emit("egress.granted", map[string]any{"hostname": host, "expires_at": until, "reason": "owner approved network access"})
	return err
}

// HostAllowed reports whether a temporary grant covers host right now.
func (e *Engine) HostAllowed(host string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hostAllowedLocked(host)
}

func (e *Engine) hostAllowedLocked(host string) bool {
	until, ok := e.hostAllows[host]
	if !ok {
		return false
	}
	if until <= e.Now() {
		delete(e.hostAllows, host)
		return false
	}
	return true
}

// FinishEgress releases an in-flight decision.
func (e *Engine) FinishEgress(decisionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.networkDecisions, decisionID)
}

// Normalized is the canonical request plus the derived classification.
type Normalized struct {
	Request     map[string]any
	Fingerprint string
	Operation   *Operation
	Repository  string
	BodyJSON    map[string]any
	Summary     map[string]any
	HasBodyJSON bool
	FileKey     string
	DocumentID  string
}

func headerPairs(value any) ([][]string, error) {
	list, ok := value.([]any)
	if !ok || len(list) > 100 {
		return nil, errors.New("invalid headers")
	}
	pairs := make([][]string, 0, len(list))
	for _, item := range list {
		pair, ok := item.([]any)
		if !ok || len(pair) != 2 {
			return nil, errors.New("invalid header")
		}
		k, kOK := pair[0].(string)
		v, vOK := pair[1].(string)
		if !kOK || !vOK {
			return nil, errors.New("invalid header")
		}
		pairs = append(pairs, []string{k, v})
	}
	return pairs, nil
}

func printableASCII(s string, min byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < min || s[i] > 126 {
			return false
		}
	}
	return true
}

// Normalize validates a proxy request and derives its fingerprint.
func (e *Engine) Normalize(request map[string]any) (*Normalized, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.normalizeLocked(request)
}

func (e *Engine) normalizeLocked(request map[string]any) (*Normalized, error) {
	method, _ := request["method"].(string)
	host, _ := request["host"].(string)
	path, _ := request["path"].(string)
	scheme := "https"
	if s, ok := request["scheme"].(string); ok {
		scheme = s
	}
	var port int64 = 443
	if p, ok := request["port"]; ok {
		if n, ok := asInt(p); ok {
			port = n
		} else {
			port = -1
		}
	}
	if !methodShape.MatchString(method) {
		return nil, errors.New("invalid method")
	}
	if !canonicalHostShape.MatchString(host) {
		return nil, errors.New("noncanonical host")
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || len(path) > 16384 || !printableASCII(path, 33) {
		return nil, errors.New("invalid request target")
	}
	if strings.ContainsAny(path, "#\\") {
		return nil, errors.New("ambiguous request target")
	}
	pathOnly, rawQuery, _ := strings.Cut(path, "?")
	// Encoded path delimiters/dot segments can change GitHub routing semantics.
	if strings.Contains(pathOnly, "%") {
		return nil, errors.New("encoded or ambiguous API path unsupported")
	}
	for _, segment := range strings.Split(pathOnly, "/") {
		if segment == "." || segment == ".." {
			return nil, errors.New("encoded or ambiguous API path unsupported")
		}
	}
	query := parseQSL(rawQuery)
	seenQuery := map[string]bool{}
	for _, pair := range query {
		if sensitiveKey.MatchString(pair.Key) {
			return nil, errors.New("credentials in query are forbidden")
		}
		seenQuery[pair.Key] = true
	}
	if len(seenQuery) != len(query) {
		return nil, errors.New("duplicate query parameters unsupported")
	}
	bodyEncoded, _ := request["body_base64"].(string)
	body, err := base64.StdEncoding.Strict().DecodeString(bodyEncoded)
	if err != nil {
		return nil, errors.New("invalid request body encoding")
	}
	if int64(len(body)) > e.policyInt("max_request_bytes") {
		return nil, errors.New("request body too large")
	}
	var headers [][]string
	if raw, ok := request["headers"]; ok {
		headers, err = headerPairs(raw)
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	lowered := make([][]string, 0, len(headers))
	for _, pair := range headers {
		k := strings.ToLower(pair[0])
		if !headerNameShape.MatchString(k) || !printableASCII(pair[1], 32) {
			return nil, errors.New("invalid header")
		}
		if seen[k] {
			return nil, errors.New("duplicate headers unsupported")
		}
		seen[k] = true
		if strippedRequestHeaders[k] {
			return nil, errors.New("proxy must strip transport and credential headers")
		}
		lowered = append(lowered, []string{k, pair[1]})
	}
	sortPairs(lowered)
	normalized := map[string]any{"method": method, "host": host, "path": path, "scheme": scheme, "port": port, "headers": pairsToAny(lowered), "body_base64": bodyEncoded}
	fingerprint := e.fingerprint(Dumps(normalized))
	result := &Normalized{Request: normalized}
	if host == "github.com" && scheme == "https" && port == 443 {
		git, err := GitInspect(method, path, headers, body)
		if err != nil {
			return nil, err
		}
		operation := "git/read"
		if git.Write {
			operation = "git/push"
		}
		summary := map[string]any{"method": method, "host": host, "path": path, "operation": operation, "repository": git.Repository, "body": e.Redactor.Body(len(body))}
		normalized["path"] = "/" + git.Repository + ".git"
		if git.Write {
			summary["update"] = map[string]any{"old": git.Update.Old, "new": git.Update.New, "ref": git.Update.Ref}
			review, ok := request["git_review"].(map[string]any)
			if !ok || !jsonEqual(review["update"], summary["update"]) || review["pack_request_sha256"] != sha256Hex(body) {
				return nil, errors.New("verified Git review is required before requesting push permission")
			}
			if !sameKeys(review, "update", "pack_request_sha256", "base", "patch", "stat", "commits", "truncated") {
				return nil, errors.New("invalid Git review")
			}
			for _, key := range []string{"patch", "stat", "commits", "base"} {
				s, ok := review[key].(string)
				if !ok || len(s) > 262144 {
					return nil, errors.New("invalid Git review")
				}
			}
			truncated, ok := review["truncated"].(bool)
			if !ok {
				return nil, errors.New("invalid Git review")
			}
			if truncated {
				return nil, errors.New("push review exceeds limit; split the change into smaller pushes")
			}
			e.pruneReviewsLocked()
			if _, exists := e.gitReviews[fingerprint]; !exists && len(e.gitReviews) >= 64 {
				oldest := e.reviewOrder[0]
				e.reviewOrder = e.reviewOrder[1:]
				delete(e.gitReviews, oldest)
			}
			if _, exists := e.gitReviews[fingerprint]; !exists {
				e.reviewOrder = append(e.reviewOrder, fingerprint)
			}
			e.gitReviews[fingerprint] = reviewEntry{e.Now() + 600, cloneJSON(review).(map[string]any)}
		} else {
			// Discovery for both services and upload-pack share one explicitly
			// approved repository read session. receive-pack never matches it.
			fingerprint = e.fingerprint("git/read:" + git.Repository)
		}
		result.Fingerprint = fingerprint
		result.Operation = &Operation{OperationID: operation}
		result.Repository = git.Repository
		result.Summary = summary
		return result, nil
	}
	if host == "api.figma.com" {
		opID, fileKey, err := FigmaOperation(method, pathOnly, query, body)
		if err != nil {
			return nil, err
		}
		result.Fingerprint = fingerprint
		result.Operation = &Operation{OperationID: opID}
		result.FileKey = fileKey
		result.Summary = map[string]any{"method": method, "host": host, "path": e.Redactor.Text(path), "operation": opID, "file_key": fileKey, "provider": "figma", "body": e.Redactor.Body(len(body))}
		return result, nil
	}
	if host == "sheets.googleapis.com" {
		access, spreadsheetID, err := GoogleSheetsOperation(method, pathOnly, query, body)
		if err != nil {
			return nil, err
		}
		opID := "google_sheets/spreadsheets/" + access
		result.Fingerprint = fingerprint
		result.Operation = &Operation{OperationID: opID}
		result.DocumentID = spreadsheetID
		result.Summary = map[string]any{"method": method, "host": host, "path": e.Redactor.Text(path), "operation": opID, "document_id": spreadsheetID, "provider": "google_sheets", "body": e.Redactor.Body(len(body))}
		return result, nil
	}
	if host == "docs.googleapis.com" {
		opID, documentID, err := GoogleDocsOperation(method, pathOnly, query, body)
		if err != nil {
			return nil, err
		}
		result.Fingerprint = fingerprint
		result.Operation = &Operation{OperationID: opID}
		result.DocumentID = documentID
		result.Summary = map[string]any{"method": method, "host": host, "path": e.Redactor.Text(path), "operation": opID, "document_id": documentID, "provider": "google_docs", "body": e.Redactor.Body(len(body))}
		return result, nil
	}
	op, params := e.Operations.Match(method, pathOnly)
	repository := strings.Trim(params["owner"]+"/"+params["repo"], "/")
	if len(body) > 0 {
		jsonBody := false
		for _, pair := range headers {
			if strings.ToLower(pair[0]) == "content-type" && strings.ToLower(strings.TrimSpace(strings.SplitN(pair[1], ";", 2)[0])) == "application/json" {
				jsonBody = true
			}
		}
		if !jsonBody {
			return nil, errors.New("only JSON API request bodies are supported")
		}
		parsed, err := StrictJSON(body)
		if err != nil {
			return nil, err
		}
		object, ok := parsed.(map[string]any)
		if !ok {
			return nil, errors.New("JSON request body must be an object")
		}
		result.BodyJSON = object
		result.HasBodyJSON = true
	}
	var opID any
	if op != nil {
		opID = op.OperationID
	}
	result.Summary = map[string]any{"method": method, "host": host, "path": e.Redactor.Text(path), "headers": pairsToAny(e.Redactor.Headers(headers)), "body": e.Redactor.Body(len(body)), "operation": opID, "repository": repository}
	if op != nil && op.OperationID == "pulls/create" && host == "api.github.com" && result.HasBodyJSON {
		if len(body) > 65536 {
			return nil, errors.New("pull request exceeds review limit")
		}
		e.pruneReviewsLocked()
		if _, exists := e.restReviews[fingerprint]; !exists && len(e.restReviews) >= 64 {
			oldest := e.restOrder[0]
			e.restOrder = e.restOrder[1:]
			delete(e.restReviews, oldest)
		}
		if _, exists := e.restReviews[fingerprint]; !exists {
			e.restOrder = append(e.restOrder, fingerprint)
		}
		e.restReviews[fingerprint] = reviewEntry{e.Now() + 600, result.BodyJSON}
	}
	result.Fingerprint = fingerprint
	result.Operation = op
	result.Repository = repository
	return result, nil
}

func (e *Engine) fingerprint(data string) string {
	mac := hmac.New(sha256.New, e.fingerprintKey)
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func sortPairs(pairs [][]string) {
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && lessPair(pairs[j], pairs[j-1]); j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}
}

func lessPair(a, b []string) bool {
	if a[0] != b[0] {
		return a[0] < b[0]
	}
	return a[1] < b[1]
}

func pairsToAny(pairs [][]string) []any {
	out := make([]any, len(pairs))
	for i, pair := range pairs {
		out[i] = []any{pair[0], pair[1]}
	}
	return out
}

func lowerList(value any) map[string]bool {
	out := map[string]bool{}
	list, _ := value.([]any)
	for _, item := range list {
		if s, ok := item.(string); ok {
			out[strings.ToLower(s)] = true
		}
	}
	return out
}

func stringList(value any) map[string]bool {
	out := map[string]bool{}
	list, _ := value.([]any)
	for _, item := range list {
		if s, ok := item.(string); ok {
			out[s] = true
		}
	}
	return out
}

type grantRow struct {
	id, requestID, kind, fingerprint, operation, path, predicates string
	expires                                                       float64
	remaining                                                     int64
	revoked                                                       int64
}

func (e *Engine) deny(requestID, reason string, status int, severity string) map[string]any {
	e.Audit.EmitSeverity("request.denied", severity, map[string]any{"request_id": requestID, "reason": reason})
	return map[string]any{"allow": false, "status": status, "reason": reason, "request_id": requestID}
}

// Authorize decides one protected-provider request. The first return value
// is the proxy decision; the error covers audit/storage failures only.
func (e *Engine) Authorize(request map[string]any) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	requestID := UUID4()
	n, err := e.normalizeLocked(request)
	if err != nil {
		if _, auditErr := e.Audit.EmitSeverity("request.denied", "warning", map[string]any{"request_id": requestID, "reason": err.Error()}); auditErr != nil {
			return nil, auditErr
		}
		return map[string]any{"allow": false, "status": 403, "reason": err.Error(), "request_id": requestID}, nil
	}
	if _, err = e.Audit.Emit("http.request", map[string]any{"request_id": requestID, "request": n.Summary}); err != nil {
		return nil, err
	}
	host, _ := n.Request["host"].(string)
	scheme, _ := n.Request["scheme"].(string)
	port, _ := asInt(n.Request["port"])
	isGoogle := host == "docs.googleapis.com" || host == "sheets.googleapis.com"
	isFigma := host == "api.figma.com"
	isGit := n.Operation != nil && (n.Operation.OperationID == "git/read" || n.Operation.OperationID == "git/push") && host == "github.com"
	reason := ""
	_, hasFigmaList := e.Policy["allowed_figma_files"]
	_, hasGoogleList := e.Policy["allowed_google_documents"]
	_, hasRepoList := e.Policy["allowed_repositories"]
	switch {
	case !e.networkEnabled:
		reason = "network disconnected"
	case !(isFigma || isGoogle) && n.Operation != nil && actionsRead[n.Operation.OperationID]:
		// Warden reads check runs and job logs itself (view_ci_results,
		// pullrequests.go Checks); the sandbox never gets them through
		// the proxy, with or without an approval, shared repository or
		// not: said first, so the answer never reads "not shared".
		reason = "CI results are read by Warden: call view_ci_results instead of the GitHub Actions API"
	case isFigma && hasFigmaList && n.FileKey != "" && !stringList(e.Policy["allowed_figma_files"])[n.FileKey]:
		reason = "Figma file outside the allowed files"
	case isGoogle && hasGoogleList && !stringList(e.Policy["allowed_google_documents"])[n.DocumentID]:
		reason = "Google document outside the allowed documents"
	case !(isFigma || isGoogle) && hasRepoList && !lowerList(e.Policy["allowed_repositories"])[strings.ToLower(n.Repository)]:
		reason = "repository not shared with this workspace; an agent asks with request_repository_access"
	case !isGit && (!(host == "api.github.com" || isFigma || isGoogle) || scheme != "https" || port != 443):
		reason = "unsupported provider channel; use approved HTTPS REST or Git smart HTTP"
	case e.GitHubApp != nil && !(isFigma || isGoogle) && githubOwnerMismatch(e.GitHubApp, n.Repository):
		reason = "repository outside configured GitHub App owner"
	case n.Operation == nil:
		reason = "operation absent from pinned REST catalog"
	case stringList(e.Policy["deny_operations"])[n.Operation.OperationID] || lowerList(e.Policy["deny_repositories"])[strings.ToLower(n.Repository)]:
		reason = "denied by local policy"
	}
	if reason == "" && e.GitHubApp != nil && !(isFigma || isGoogle) {
		if _, err := GitHubPermissions(n.Operation.OperationID); err != nil {
			reason = "operation is not supported by the GitHub App broker"
			if _, appID, _ := e.GitHubApp.Identity(); appID == 0 {
				reason = "operation is not supported for GitHub repositories"
			}
		}
	}
	if reason != "" {
		if _, err = e.Audit.EmitSeverity("request.denied", "warning", map[string]any{"request_id": requestID, "reason": reason}); err != nil {
			return nil, err
		}
		return map[string]any{"allow": false, "status": 403, "reason": reason, "request_id": requestID}, nil
	}
	now := e.Now()
	rows, err := e.DB.Query(`SELECT id,request_id,kind,fingerprint,operation,path,predicates,expires,remaining,revoked FROM grants WHERE revoked=0 AND expires>? AND remaining!=0 ORDER BY CASE kind WHEN 'exact' THEN 0 ELSE 1 END`, now)
	if err != nil {
		return nil, err
	}
	var candidates []grantRow
	for rows.Next() {
		var g grantRow
		if err = rows.Scan(&g.id, &g.requestID, &g.kind, &g.fingerprint, &g.operation, &g.path, &g.predicates, &g.expires, &g.remaining, &g.revoked); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, g)
	}
	rows.Close()
	normalizedPath, _ := n.Request["path"].(string)
	for _, grant := range candidates {
		match := false
		if grant.kind == "exact" {
			match = subtle.ConstantTimeCompare([]byte(grant.fingerprint), []byte(n.Fingerprint)) == 1
		} else {
			match = n.Operation.OperationID != "git/push" && grant.operation == n.Operation.OperationID && grant.path == normalizedPath
			var predicates map[string]any
			_ = json.Unmarshal([]byte(grant.predicates), &predicates)
			for pointer, expected := range predicates {
				var current any = n.BodyJSON
				if !n.HasBodyJSON {
					current = nil
				}
				for _, part := range strings.Split(strings.Trim(pointer, "/"), "/") {
					part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
					object, ok := current.(map[string]any)
					if !ok {
						match = false
						break
					}
					next, present := object[part]
					if !present {
						match = false
						break
					}
					current = next
				}
				if !jsonEqual(current, expected) {
					match = false
				}
			}
		}
		if !match {
			continue
		}
		var authorization string
		var credentialErr error
		switch {
		case isGoogle:
			authorization, credentialErr = e.GoogleDocs.Authorization()
		case isFigma:
			authorization, credentialErr = e.Figma.Authorization()
		case e.GitHubApp != nil:
			authorization, credentialErr = e.GitHubApp.Authorization(n.Repository, n.Operation.OperationID, nil)
		case e.token != "":
			authorization = "Bearer " + e.token
		default:
			credentialErr = errors.New("missing credential")
		}
		if credentialErr != nil || authorization == "" {
			provider := "GitHub"
			if isGoogle {
				provider = "Google Docs"
			} else if isFigma {
				provider = "Figma"
			}
			if _, err = e.Audit.Emit("request.denied", map[string]any{"request_id": requestID, "reason": "host " + provider + " credential unavailable"}); err != nil {
				return nil, err
			}
			return map[string]any{"allow": false, "status": 503, "reason": "connect " + provider + " on the host", "request_id": requestID}, nil
		}
		now = e.Now()
		if grant.expires <= now {
			continue
		}
		decisionID := UUID4()
		if _, err = e.Audit.Emit("request.allowed", map[string]any{"request_id": requestID, "decision_id": decisionID, "grant_id": grant.id, "expires_at": grant.expires, "operation": n.Operation.OperationID}); err != nil {
			return nil, err
		}
		tx, err := e.DB.Begin()
		if err != nil {
			return nil, err
		}
		if grant.remaining > 0 {
			if _, err = tx.Exec("UPDATE grants SET remaining=remaining-1 WHERE id=?", grant.id); err != nil {
				tx.Rollback()
				return nil, err
			}
		}
		if _, err = tx.Exec("INSERT INTO decisions VALUES(?,?,?)", decisionID, grant.id, grant.expires); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err = tx.Exec("UPDATE requests SET status='executed' WHERE fingerprint=? AND status='pending'", n.Fingerprint); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return map[string]any{"allow": true, "request_id": requestID, "decision_id": decisionID, "grant_id": grant.id, "expires_at": grant.expires, "remaining_seconds": grant.expires - now, "authorization": authorization}, nil
	}
	var existing string
	err = e.DB.QueryRow("SELECT id FROM requests WHERE fingerprint=? AND status='pending'", n.Fingerprint).Scan(&existing)
	switch {
	case err == nil:
		requestID = existing
	case errors.Is(err, sql.ErrNoRows):
		var count int
		if err = e.DB.QueryRow("SELECT count(*) FROM requests WHERE status='pending'").Scan(&count); err != nil {
			return nil, err
		}
		if count >= 1000 {
			return map[string]any{"allow": false, "status": 429, "reason": "approval queue full", "request_id": requestID}, nil
		}
		if _, err = e.DB.Exec("INSERT INTO requests VALUES(?,?,?,?,?,?,?,?)", requestID, n.Fingerprint, n.Operation.OperationID, normalizedPath, n.Repository, Dumps(n.Summary), "pending", now); err != nil {
			return nil, err
		}
		if _, err = e.Audit.Emit("approval.requested", map[string]any{"request_id": requestID, "operation": n.Operation.OperationID, "request": n.Summary}); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return map[string]any{"allow": false, "status": 428, "reason": "human approval required; approve on host, then retry this exact request", "request_id": requestID}, nil
}

// Approve turns a pending request into an exact or scoped grant.
func (e *Engine) Approve(requestID, kind string, ttl int64, predicates map[string]any) (map[string]any, error) {
	if kind != "exact" && kind != "scoped" {
		return nil, errors.New("invalid grant kind")
	}
	if ttl < 1 || ttl > e.policyInt("max_grant_seconds") {
		return nil, errors.New("invalid duration")
	}
	if predicates == nil {
		predicates = map[string]any{}
	}
	if len(predicates) > 100 {
		return nil, errors.New("predicates must map JSON pointers to exact values")
	}
	for key := range predicates {
		if !jsonPointerShape.MatchString(key) {
			return nil, errors.New("predicates must map JSON pointers to exact values")
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var fingerprint, operation, path string
	err := e.DB.QueryRow("SELECT fingerprint,operation,path FROM requests WHERE id=? AND status='pending'", requestID).Scan(&fingerprint, &operation, &path)
	if err != nil {
		return nil, errors.New("pending request not found")
	}
	now := e.Now()
	if operation == "git/push" {
		if kind != "exact" || len(predicates) > 0 {
			return nil, errors.New("Git push permissions must be exact and single use")
		}
		if entry, ok := e.gitReviews[fingerprint]; !ok || entry.expires <= now {
			return nil, errors.New("Git review expired; retry the push to regenerate it")
		}
	}
	if operation == "git/read" && len(predicates) > 0 {
		return nil, errors.New("Git read sessions do not support body predicates")
	}
	if operation == "pulls/create" {
		if entry, ok := e.restReviews[fingerprint]; !ok || entry.expires <= now {
			return nil, errors.New("pull request review expired; retry the request")
		}
	}
	grantID := UUID4()
	expires := now + float64(ttl)
	var maxUses any
	remaining := int64(-1)
	if kind == "exact" {
		maxUses = 1
		remaining = 1
	}
	if _, err = e.Audit.Emit("approval.granted", map[string]any{"request_id": requestID, "grant_id": grantID, "actor": map[string]any{"type": "human", "interface": "host_browser"},
		"grant": map[string]any{"kind": kind, "expires_at": expires, "operation": operation, "path": path, "body_equals": predicates, "max_uses": maxUses}}); err != nil {
		return nil, err
	}
	tx, err := e.DB.Begin()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("INSERT INTO grants(id,request_id,kind,fingerprint,operation,path,predicates,expires,remaining) VALUES(?,?,?,?,?,?,?,?,?)", grantID, requestID, kind, fingerprint, operation, path, Dumps(predicates), expires, remaining); err != nil {
		tx.Rollback()
		return nil, err
	}
	if _, err = tx.Exec("UPDATE requests SET status='approved' WHERE id=?", requestID); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"grant_id": grantID, "expires_at": expires}, nil
}

// GitReview returns the cached push review for a pending push request.
func (e *Engine) GitReview(requestID string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gitReviewLocked(requestID)
}

func (e *Engine) gitReviewLocked(requestID string) (map[string]any, error) {
	var fingerprint string
	err := e.DB.QueryRow("SELECT fingerprint FROM requests WHERE id=? AND operation=?", requestID, "git/push").Scan(&fingerprint)
	entry, ok := e.gitReviews[fingerprint]
	if err != nil || !ok || entry.expires <= e.Now() {
		return nil, errors.New("review expired; retry the push")
	}
	out := map[string]any{"expires_at": entry.expires}
	for k, v := range entry.value {
		out[k] = v
	}
	return out, nil
}

// RequestReview returns the push or pull-request review for a request.
func (e *Engine) RequestReview(requestID string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var fingerprint, operation string
	err := e.DB.QueryRow("SELECT fingerprint,operation FROM requests WHERE id=?", requestID).Scan(&fingerprint, &operation)
	if err == nil && operation == "git/push" {
		return e.gitReviewLocked(requestID)
	}
	if err != nil || operation != "pulls/create" {
		return nil, errors.New("review expired; retry the request")
	}
	entry, ok := e.restReviews[fingerprint]
	if !ok || entry.expires <= e.Now() {
		return nil, errors.New("review expired; retry the request")
	}
	return map[string]any{"expires_at": entry.expires, "pull_request": entry.value}, nil
}

// PruneReviews drops expired cached reviews.
func (e *Engine) PruneReviews() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruneReviewsLocked()
}

func (e *Engine) pruneReviewsLocked() {
	now := e.Now()
	e.reviewOrder = pruneOrder(e.reviewOrder, e.gitReviews, now)
	e.restOrder = pruneOrder(e.restOrder, e.restReviews, now)
}

func pruneOrder(order []string, entries map[string]reviewEntry, now float64) []string {
	kept := order[:0]
	for _, key := range order {
		if entry, ok := entries[key]; ok && entry.expires > now {
			kept = append(kept, key)
		} else {
			delete(entries, key)
		}
	}
	return kept
}

// ExpireGitReview forces a cached review to expire (tests and operators).
func (e *Engine) ExpireGitReview(requestID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var fingerprint string
	if e.DB.QueryRow("SELECT fingerprint FROM requests WHERE id=?", requestID).Scan(&fingerprint) == nil {
		if entry, ok := e.gitReviews[fingerprint]; ok {
			e.gitReviews[fingerprint] = reviewEntry{0, entry.value}
		}
	}
}

// Deny records a human denial.
func (e *Engine) Deny(requestID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.Audit.Emit("approval.denied", map[string]any{"request_id": requestID, "actor": map[string]any{"type": "human"}}); err != nil {
		return err
	}
	_, err := e.DB.Exec("UPDATE requests SET status='denied' WHERE id=?", requestID)
	return err
}

// Revoke revokes one grant.
func (e *Engine) Revoke(grantID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.Audit.Emit("approval.revoked", map[string]any{"grant_id": grantID, "actor": map[string]any{"type": "human"}}); err != nil {
		return err
	}
	_, err := e.DB.Exec("UPDATE grants SET revoked=1 WHERE id=?", grantID)
	return err
}

// Active reports whether a decision still permits dispatch or delivery.
func (e *Engine) Active(decisionID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.networkEnabled {
		return false
	}
	if expires, ok := e.networkDecisions[decisionID]; ok {
		return expires > e.Now()
	}
	var expires float64
	var revoked int64
	if err := e.DB.QueryRow("SELECT d.expires,g.revoked FROM decisions d JOIN grants g ON d.grant_id=g.id WHERE d.id=?", decisionID).Scan(&expires, &revoked); err != nil {
		return false
	}
	return revoked == 0 && expires > e.Now()
}

// PendingRequests lists pending approval requests (operators and tests).
func (e *Engine) PendingRequests() ([]map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows, err := e.DB.Query("SELECT id,operation,path,repository,summary,status,created FROM requests WHERE status='pending' ORDER BY created DESC,id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, operation, path, repository, summary, status string
		var created float64
		if err = rows.Scan(&id, &operation, &path, &repository, &summary, &status, &created); err != nil {
			return nil, err
		}
		var parsed any
		_ = json.Unmarshal([]byte(summary), &parsed)
		out = append(out, map[string]any{"id": id, "operation": operation, "path": path, "repository": repository, "summary": e.Redactor.Clean(parsed), "status": status, "created": created})
	}
	return out, rows.Err()
}

// ParseRequestPath splits a request target for callers outside the engine.
func ParseRequestPath(path string) (string, []QueryPair) {
	u, err := url.Parse(path)
	if err != nil {
		p, q, _ := strings.Cut(path, "?")
		return p, parseQSL(q)
	}
	return u.Path, parseQSL(u.RawQuery)
}
