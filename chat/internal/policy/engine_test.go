package policy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testToken = "ghp_TESTONLYNOTAREALTOKENTESTONLYNOTAREALTOKENTESTONLYNOTAREALTOKEN"

func apiRequest(body map[string]any, overrides map[string]any) map[string]any {
	if body == nil {
		body = map[string]any{"title": "A change", "head": "work", "base": "main", "draft": true}
	}
	raw, _ := json.Marshal(body)
	result := map[string]any{"method": "POST", "host": "api.github.com", "path": "/repos/acme/demo/pulls", "scheme": "https", "port": json.Number("443"),
		"headers": []any{[]any{"content-type", "application/json"}}, "body_base64": base64.StdEncoding.EncodeToString(raw)}
	for k, v := range overrides {
		result[k] = v
	}
	return result
}

type engineFixture struct {
	t      *testing.T
	dir    string
	clock  *testClock
	engine *Engine
}

func newEngineFixture(t *testing.T) *engineFixture {
	dir := t.TempDir()
	clock := &testClock{now: 1000, mono: 500}
	engine := newTestEngine(t, dir, clock)
	if err := engine.SetToken(testToken); err != nil {
		t.Fatal(err)
	}
	return &engineFixture{t: t, dir: dir, clock: clock, engine: engine}
}

func (f *engineFixture) authorize(req map[string]any) map[string]any {
	f.t.Helper()
	result, err := f.engine.Authorize(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *engineFixture) grant(req map[string]any, kind string, ttl int64, predicates map[string]any) map[string]any {
	f.t.Helper()
	if req == nil {
		req = apiRequest(nil, nil)
	}
	pending := f.authorize(req)
	if pending["allow"] != false || statusOf(pending) != 428 {
		f.t.Fatalf("expected 428 pending, got %v", pending)
	}
	grant, err := f.engine.Approve(pending["request_id"].(string), kind, ttl, predicates)
	if err != nil {
		f.t.Fatal(err)
	}
	return grant
}

func statusOf(result map[string]any) int {
	n, _ := asInt(result["status"])
	return int(n)
}

func TestCatalogIdentifiesIndividualOperations(t *testing.T) {
	ops := testOperations(t)
	op, params := ops.Match("PUT", "/repos/acme/demo/pulls/12/merge")
	if op == nil || op.OperationID != "pulls/merge" || params["pull_number"] != "12" {
		t.Fatalf("match: %v %v", op, params)
	}
	if ops.Count() <= 1000 {
		t.Fatalf("catalog too small: %d", ops.Count())
	}
}

func TestDefaultDenyNeverReturnsToken(t *testing.T) {
	f := newEngineFixture(t)
	result := f.authorize(apiRequest(nil, nil))
	if result["allow"] != false || strings.Contains(Dumps(result), testToken) {
		t.Fatalf("token leaked: %v", result)
	}
}

func TestExactSingleUseAndConcurrentReplay(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "exact", 60, nil)
	var wg sync.WaitGroup
	results := make([]map[string]any, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = f.engine.Authorize(apiRequest(nil, nil))
		}(i)
	}
	wg.Wait()
	allowed := 0
	for _, r := range results {
		if r != nil && r["allow"] == true {
			allowed++
			if r["authorization"] != "Bearer "+testToken {
				t.Fatalf("authorization: %v", r)
			}
		}
	}
	if allowed != 1 {
		t.Fatalf("allowed %d times", allowed)
	}
}

func TestExactBindsEveryByteAndHeader(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "exact", 60, nil)
	for _, altered := range []map[string]any{
		apiRequest(map[string]any{"title": "evil", "head": "work", "base": "main", "draft": true}, nil),
		apiRequest(nil, map[string]any{"path": "/repos/acme/other/pulls"}),
		apiRequest(nil, map[string]any{"path": "/repos/acme/demo/pulls?x=1"}),
		apiRequest(nil, map[string]any{"headers": []any{[]any{"content-type", "application/json"}, []any{"accept", "application/vnd.github+json"}}}),
	} {
		if f.authorize(altered)["allow"] == true {
			t.Fatalf("altered request allowed: %v", altered)
		}
	}
	if f.authorize(apiRequest(nil, nil))["allow"] != true {
		t.Fatal("exact request denied")
	}
}

func TestScopedPredicatesAndRepetition(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "scoped", 60, map[string]any{"/base": "main", "/draft": true})
	if f.authorize(apiRequest(map[string]any{"title": "another", "base": "main", "draft": true}, nil))["allow"] != true {
		t.Fatal("scoped denied")
	}
	if f.authorize(apiRequest(nil, nil))["allow"] != true {
		t.Fatal("scoped repeat denied")
	}
	for _, body := range []map[string]any{{"base": "main", "draft": 1}, {"base": "release", "draft": true}, {"base": "main"}} {
		if f.authorize(apiRequest(body, nil))["allow"] == true {
			t.Fatalf("predicate mismatch allowed: %v", body)
		}
	}
}

func TestScopedDoesNotCrossOperationOrRepository(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "scoped", 60, nil)
	if f.authorize(apiRequest(nil, map[string]any{"path": "/repos/acme/demo/issues"}))["allow"] == true {
		t.Fatal("crossed operation")
	}
	if f.authorize(apiRequest(nil, map[string]any{"path": "/repos/acme/other/pulls"}))["allow"] == true {
		t.Fatal("crossed repository")
	}
}

func TestExpiryRevocationAndClockRollback(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "scoped", 10, nil)
	decision := f.authorize(apiRequest(nil, nil))
	if !f.engine.Active(decision["decision_id"].(string)) {
		t.Fatal("decision inactive")
	}
	f.clock.now = 900
	f.clock.mono += 11
	if f.engine.Active(decision["decision_id"].(string)) || f.authorize(apiRequest(nil, nil))["allow"] == true {
		t.Fatal("clock rollback extended the grant")
	}
	grant := f.grant(nil, "scoped", 30, nil)
	decision = f.authorize(apiRequest(nil, nil))
	if err := f.engine.Revoke(grant["grant_id"].(string)); err != nil {
		t.Fatal(err)
	}
	if f.engine.Active(decision["decision_id"].(string)) {
		t.Fatal("revoked decision active")
	}
}

func TestRestartRevokesAllGrants(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "scoped", 60, nil)
	f.engine.Close()
	restarted := newTestEngine(t, f.dir, nil)
	if err := restarted.SetToken(testToken); err != nil {
		t.Fatal(err)
	}
	result, err := restarted.Authorize(apiRequest(nil, nil))
	if err != nil || result["allow"] == true {
		t.Fatalf("grant survived restart: %v %v", result, err)
	}
}

func TestNoTokenInAuditDatabaseOrListing(t *testing.T) {
	f := newEngineFixture(t)
	req := apiRequest(map[string]any{"title": testToken, "password": "supersecret", "nested": map[string]any{"api_key": "hidden"}}, nil)
	f.grant(req, "exact", 60, nil)
	f.authorize(req)
	audit, err := os.ReadFile(filepath.Join(f.dir, "audit", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testToken, "supersecret", "hidden"} {
		if strings.Contains(string(audit), secret) {
			t.Fatalf("audit contains %s", secret)
		}
	}
	rows, err := f.engine.DB.Query("SELECT summary FROM requests")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var summary string
		_ = rows.Scan(&summary)
		if strings.Contains(summary, testToken) {
			t.Fatal("token stored in requests")
		}
	}
	if !f.engine.TokenConfigured() {
		t.Fatal("token not configured")
	}
}

func TestAuditChainVerifies(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "exact", 60, nil)
	f.authorize(apiRequest(nil, nil))
	audit, err := os.ReadFile(filepath.Join(f.dir, "audit", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	previous := strings.Repeat("0", 64)
	for _, line := range strings.Split(strings.TrimSpace(string(audit)), "\n") {
		parsed, err := StrictJSON([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		event := parsed.(map[string]any)
		digest := event["event_hash"].(string)
		delete(event, "event_hash")
		if event["previous_hash"] != previous {
			t.Fatalf("chain broken at %v", event["event_type"])
		}
		sum := sha256.Sum256([]byte(Dumps(event)))
		if hex.EncodeToString(sum[:]) != digest {
			t.Fatalf("hash mismatch for %v", event["event_type"])
		}
		previous = digest
	}
}

func TestAuditFailureNeverAllows(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "exact", 60, nil)
	f.engine.Audit.failWith = errors.New("disk full")
	if _, err := f.engine.Authorize(apiRequest(nil, nil)); err == nil {
		t.Fatal("audit failure allowed the request")
	}
	f.engine.Audit.failWith = nil
	if f.authorize(apiRequest(nil, nil))["allow"] != true {
		t.Fatal("grant consumed by failed audit")
	}
}

func TestMissingTokenFailsClosed(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "exact", 60, nil)
	f.engine.token = ""
	result := f.authorize(apiRequest(nil, nil))
	if result["allow"] == true || statusOf(result) != 503 {
		t.Fatalf("expected 503, got %v", result)
	}
}

func TestOAuthTokenRequiresApprovalAndStaysOutOfStorage(t *testing.T) {
	f := newEngineFixture(t)
	token := "gho_TESTONLYNOTAREALTOKENTESTONLYNOTAREALTOKENTESTONLYNOTAREALTOKEN"
	if err := f.engine.SetToken(token); err != nil {
		t.Fatal(err)
	}
	pending := f.authorize(apiRequest(nil, nil))
	if statusOf(pending) != 428 || strings.Contains(Dumps(pending), token) {
		t.Fatalf("pending: %v", pending)
	}
	if _, err := f.engine.Approve(pending["request_id"].(string), "exact", 60, nil); err != nil {
		t.Fatal(err)
	}
	if f.authorize(apiRequest(nil, nil))["authorization"] != "Bearer "+token {
		t.Fatal("oauth token not returned")
	}
	if f.engine.Redactor.Text(token) != "[REDACTED]" {
		t.Fatal("token not redacted")
	}
	if hits := filesContain(t, f.dir, []byte(token)); len(hits) > 0 {
		t.Fatalf("token persisted in %v", hits)
	}
	if err := f.engine.SetToken("ghr_" + strings.Repeat("a", 40)); err == nil {
		t.Fatal("refresh token accepted")
	}
	if err := f.engine.SetToken(token + "\r\nInjected: yes"); err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestUnsupportedChannelsCannotBeApproved(t *testing.T) {
	f := newEngineFixture(t)
	for _, altered := range []map[string]any{
		apiRequest(nil, map[string]any{"host": "github.com"}),
		apiRequest(nil, map[string]any{"host": "evil.example"}),
		apiRequest(nil, map[string]any{"path": "/graphql"}),
		apiRequest(nil, map[string]any{"path": "/unknown-new-operation"}),
		apiRequest(nil, map[string]any{"scheme": "http", "port": json.Number("80")}),
		apiRequest(nil, map[string]any{"port": json.Number("8443")}),
	} {
		if statusOf(f.authorize(altered)) != 403 {
			t.Fatalf("expected 403 for %v", altered)
		}
	}
}

func TestAmbiguousPathsHeadersAndJSONRejected(t *testing.T) {
	f := newEngineFixture(t)
	bad := []map[string]any{
		apiRequest(nil, map[string]any{"path": "//api.github.com/repos/acme/demo/pulls"}),
		apiRequest(nil, map[string]any{"path": "/repos/acme/%2e%2e/pulls"}),
		apiRequest(nil, map[string]any{"path": "/repos/acme/demo/pulls#x"}),
		apiRequest(nil, map[string]any{"path": "/repos/acme/demo/pulls?access_token=secret"}),
		apiRequest(nil, map[string]any{"path": "/repos/acme/demo/pulls?a=1&a=2"}),
		apiRequest(nil, map[string]any{"headers": []any{[]any{"Authorization", "Bearer fake"}}}),
		apiRequest(nil, map[string]any{"headers": []any{[]any{"x", "a"}, []any{"X", "b"}}}),
		apiRequest(nil, map[string]any{"body_base64": base64.StdEncoding.EncodeToString([]byte(`{"a":1,"a":2}`))}),
		apiRequest(nil, map[string]any{"body_base64": base64.StdEncoding.EncodeToString([]byte(`{"x":NaN}`))}),
		apiRequest(nil, map[string]any{"body_base64": "%%%"}),
	}
	for _, altered := range bad {
		if statusOf(f.authorize(altered)) != 403 {
			t.Fatalf("expected 403 for %v", altered)
		}
	}
}

func TestPolicyDenyAndPolicyChangeRevokes(t *testing.T) {
	f := newEngineFixture(t)
	f.grant(nil, "scoped", 60, nil)
	decision := f.authorize(apiRequest(nil, nil))
	policy := f.engine.PolicyCopy()
	policy["deny_operations"] = []any{"pulls/create"}
	if err := f.engine.SavePolicy(policy); err != nil {
		t.Fatal(err)
	}
	if f.engine.Active(decision["decision_id"].(string)) {
		t.Fatal("decision survived policy change")
	}
	if statusOf(f.authorize(apiRequest(nil, nil))) != 403 {
		t.Fatal("denied operation not 403")
	}
}

func TestInvalidGrantCannotBeCreated(t *testing.T) {
	f := newEngineFixture(t)
	pending := f.authorize(apiRequest(nil, nil))
	for _, ttl := range []int64{-1, 0, 3601} {
		if _, err := f.engine.Approve(pending["request_id"].(string), "exact", ttl, nil); err == nil {
			t.Fatalf("ttl %d accepted", ttl)
		}
	}
	if _, err := f.engine.Approve(pending["request_id"].(string), "exact", 60, map[string]any{"not a pointer": 1}); err == nil {
		t.Fatal("invalid predicate accepted")
	}
}

func TestEgressDecisionsAndNetworkSwitch(t *testing.T) {
	f := newEngineFixture(t)
	decision, err := f.engine.AuthorizeEgress(map[string]any{"host": "api.openai.com", "method": "POST", "scheme": "https"})
	if err != nil || decision["allow"] != true {
		t.Fatalf("egress: %v %v", decision, err)
	}
	if !f.engine.Active(decision["decision_id"].(string)) {
		t.Fatal("egress decision inactive")
	}
	denied, _ := f.engine.AuthorizeEgress(map[string]any{"host": "evil.example", "method": "GET", "scheme": "https"})
	if denied["allow"] != false || statusOf(denied) != 403 {
		t.Fatalf("evil allowed: %v", denied)
	}
	if err := f.engine.SetNetwork(false); err != nil {
		t.Fatal(err)
	}
	if f.engine.Active(decision["decision_id"].(string)) {
		t.Fatal("decision active while disconnected")
	}
	if !f.engine.NetworkEnabled() == false {
		t.Fatal("network flag")
	}
	f.engine.Close()
	restarted := newTestEngine(t, f.dir, nil)
	if restarted.NetworkEnabled() {
		t.Fatal("disconnect not durable")
	}
}
