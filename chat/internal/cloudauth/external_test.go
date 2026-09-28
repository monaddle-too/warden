package cloudauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedirectValidation(t *testing.T) {
	for _, v := range []string{"http://127.0.0.1:9876/callback", "http://localhost:1234/cb", "http://[::1]:555/cb", "https://agent.example/cb?x=y"} {
		if !validRedirect(v) {
			t.Errorf("rejected %s", v)
		}
	}
	for _, v := range []string{"http://example.com/cb", "https://example.com/cb#frag", "https://u:p@example.com/cb", "javascript:alert(1)", "//example.com/cb", "https://example.com/\r\n"} {
		if validRedirect(v) {
			t.Errorf("accepted %s", v)
		}
	}
}
func TestExternalAgentsPostgres(t *testing.T) {
	dsn := os.Getenv("WARDEN_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, _ := store.UserByEmail(ctx, "owner@example.com")
	org, err := store.CreateOrganization(ctx, owner, "External test "+ID())
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateOrganization(ctx, owner, "External other "+ID())
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.PutMember(ctx, owner, org.ID, ID()+"@example.com", "External Tester", "user")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.PutMember(ctx, owner, other.ID, member.Email, member.Name, "user")
	if err != nil {
		t.Fatal(err)
	}
	raw, csrf := randomSecret(), randomSecret()
	_, err = store.db.ExecContext(ctx, `INSERT INTO warden_cloud.sessions(digest,user_id,organization_id,csrf,expires_at) VALUES($1,$2,$3,$4,now()+interval '1 hour')`, hashSecret(raw), member.ID, org.ID, csrf)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/warden-tools" || r.Header.Get("X-Panta-Organization") != org.ID || r.Header.Get("X-Panta-Key") != "private-key" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("private bridge scope/headers incorrect")
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if !strings.HasPrefix(fmt.Sprint(in["chatId"]), "external-") {
			t.Error("external attribution missing")
		}
		called = true
		respond(w, 200, map[string]any{"documents": []any{}})
	}))
	defer docs.Close()
	a := &Auth{Login: &Login{Store: store}, DocsAddress: docs.URL, DocsKey: "private-key"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.ExternalRoute(w, r) {
			a.Handler().ServeHTTP(w, r)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	a.Origin = server.URL
	request := func(method, path string, body any, cookie, token string) (int, []byte, http.Header) {
		t.Helper()
		var reader io.Reader
		ct := "application/json"
		switch b := body.(type) {
		case url.Values:
			reader = strings.NewReader(b.Encode())
			ct = "application/x-www-form-urlencoded"
		case nil:
		default:
			v, _ := json.Marshal(b)
			reader = bytes.NewReader(v)
		}
		r := httptest.NewRequest(method, a.Origin+path, reader)
		r.Header.Set("Content-Type", ct)
		r.RemoteAddr = "198.51.100.3:1000"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
			r.Header.Set("Origin", a.Origin)
			r.Header.Set("X-Warden-CSRF", csrf)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code, w.Body.Bytes(), w.Header()
	}
	obj := func(raw []byte) map[string]any {
		t.Helper()
		var v map[string]any
		if json.Unmarshal(raw, &v) != nil {
			t.Fatalf("invalid JSON: %s", raw)
		}
		return v
	}
	status, _, headers := request("POST", "/mcp", map[string]any{}, "", "")
	if status != 401 || !strings.Contains(headers.Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatal("missing auth discovery")
	}
	status, b, _ := request("GET", "/.well-known/oauth-authorization-server", nil, "", "")
	if status != 200 || obj(b)["code_challenge_methods_supported"] == nil {
		t.Fatal("missing PKCE discovery")
	}
	status, b, _ = request("POST", "/oauth/register", map[string]any{"client_name": "Test agent", "redirect_uris": []string{"http://127.0.0.1:8765/callback"}, "token_endpoint_auth_method": "none"}, "", "")
	if status != 201 {
		t.Fatalf("register: %d %s", status, b)
	}
	client := obj(b)["client_id"].(string)
	verifier := randomSecret() + randomSecret()
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	authorize := url.Values{"client_id": {client}, "redirect_uri": {"http://127.0.0.1:8765/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {challenge}, "resource": {a.Origin + "/mcp"}, "state": {"callback-state"}}
	authorize.Set("redirect_uri", "https://evil.example/cb")
	status, _, _ = request("GET", "/oauth/authorize?"+authorize.Encode(), nil, "", "")
	if status != 400 {
		t.Fatal("accepted unregistered callback")
	}
	authorize.Set("redirect_uri", "http://127.0.0.1:8765/callback")
	connect := func() (string, url.Values) {
		t.Helper()
		status, b, h := request("GET", "/oauth/authorize?"+authorize.Encode(), nil, "", "")
		if status != 303 {
			t.Fatalf("authorize: %d %s", status, b)
		}
		u, _ := url.Parse(h.Get("Location"))
		path := "/auth/agent-authorization/" + u.Query().Get("request")
		status, _, _ = request("GET", path, nil, "", "")
		if status != 401 {
			t.Fatal("anonymous consent allowed")
		}
		status, _, _ = request("POST", path, map[string]any{"allow": true, "organizationId": ID()}, raw, "")
		if status != 403 {
			t.Fatal("cross org consent allowed")
		}
		status, b, _ = request("POST", path, map[string]any{"allow": true, "organizationId": org.ID}, raw, "")
		if status != 200 {
			t.Fatalf("consent: %d %s", status, b)
		}
		callback, _ := url.Parse(obj(b)["redirect"].(string))
		if callback.Query().Get("state") != "callback-state" {
			t.Fatal("callback state lost")
		}
		status, _, _ = request("POST", path, map[string]any{"allow": true, "organizationId": org.ID}, raw, "")
		if status != 410 {
			t.Fatal("consent replayed")
		}
		return callback.Query().Get("code"), url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "redirect_uri": {authorize.Get("redirect_uri")}, "resource": {a.Origin + "/mcp"}, "code_verifier": {verifier}}
	}
	code, form := connect()
	form.Set("code", code)
	form.Set("code_verifier", randomSecret()+randomSecret())
	status, _, _ = request("POST", "/oauth/token", form, "", "")
	if status != 400 {
		t.Fatal("wrong PKCE accepted")
	}
	form.Set("code_verifier", verifier)
	form.Set("resource", "https://other.example/mcp")
	status, _, _ = request("POST", "/oauth/token", form, "", "")
	if status != 400 {
		t.Fatal("wrong resource accepted")
	}
	form.Set("resource", a.Origin+"/mcp")
	status, b, _ = request("POST", "/oauth/token", form, "", "")
	if status != 200 {
		t.Fatalf("token: %d %s", status, b)
	}
	tokens := obj(b)
	access := tokens["access_token"].(string)
	refresh := tokens["refresh_token"].(string)
	status, _, _ = request("POST", "/oauth/token", form, "", "")
	if status != 400 {
		t.Fatal("code replayed")
	}
	mcp := func(token, name string, args any) (int, map[string]any) {
		t.Helper()
		status, b, _ := request("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}, "", token)
		return status, obj(b)
	}
	status, v := mcp(access, "panta_list_documents", map[string]any{})
	if status != 200 || !called || v["result"].(map[string]any)["isError"] != false {
		t.Fatalf("MCP document bridge: %v", v)
	}
	// A valid agent token never authenticates the browser/admin surface.
	status, _, _ = request("GET", "/auth/session", nil, "", access)
	if status != 200 {
		t.Fatal("session request failed")
	}
	r := httptest.NewRequest("GET", a.Origin+"/api/state", nil)
	r.Header.Set("Authorization", "Bearer "+access)
	if a.Role(r) != "" {
		t.Fatal("agent became browser")
	}
	upload := map[string]any{"operationId": "aabbbbbb-1111-4111-8111-111111111111", "title": "Shared test", "messages": []any{map[string]any{"role": "user", "content": "Question"}, map[string]any{"role": "assistant", "content": "**Answer**"}}}
	_, v = mcp(access, "warden_share_conversation", upload)
	result := v["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("upload %v", v)
	}
	shared := obj([]byte(result["content"].([]any)[0].(map[string]any)["text"].(string)))
	sharedID := shared["id"].(string)
	_, v = mcp(access, "warden_share_conversation", upload)
	again := obj([]byte(v["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)))
	if again["id"] != sharedID {
		t.Fatal("duplicate snapshot")
	}
	upload["title"] = "Changed"
	_, v = mcp(access, "warden_share_conversation", upload)
	if v["result"].(map[string]any)["isError"] != true {
		t.Fatal("conflicting retry accepted")
	}
	status, b, _ = request("GET", "/auth/shared-conversations/"+sharedID, nil, raw, "")
	if status != 200 || obj(b)["title"] != "Shared test" {
		t.Fatal("shared view failed")
	}
	_, err = store.db.ExecContext(ctx, `UPDATE warden_cloud.sessions SET organization_id=$1 WHERE digest=$2`, other.ID, hashSecret(raw))
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ = request("GET", "/auth/shared-conversations/"+sharedID, nil, raw, "")
	if status != 404 {
		t.Fatal("cross organization shared read")
	}
	called = false
	_, _ = mcp(access, "panta_list_documents", map[string]any{})
	if !called {
		t.Fatal("session switch changed connection organization")
	}
	// A second process sees the same durable grants and uploaded snapshots.
	reopened, err := Open(ctx, dsn, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	a.Login.Store = reopened
	defer reopened.Close()
	status, _ = mcp(access, "panta_list_documents", map[string]any{})
	if status != 200 {
		t.Fatal("token lost after reopen")
	}
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "resource": {a.Origin + "/mcp"}, "refresh_token": {refresh}}
	status, b, _ = request("POST", "/oauth/token", refreshForm, "", "")
	if status != 200 {
		t.Fatalf("refresh %d %s", status, b)
	}
	rotated := obj(b)["access_token"].(string)
	status, _ = mcp(rotated, "panta_list_documents", map[string]any{})
	if status != 200 {
		t.Fatal("rotated token failed")
	}
	status, _, _ = request("POST", "/oauth/token", refreshForm, "", "")
	if status != 400 {
		t.Fatal("refresh replay accepted")
	}
	status, _ = mcp(rotated, "panta_list_documents", map[string]any{})
	if status != 401 {
		t.Fatal("refresh replay did not revoke family")
	}
	// A new grant is revoked permanently on membership removal, including re-add.
	code, form = connect()
	form.Set("code", code)
	status, b, _ = request("POST", "/oauth/token", form, "", "")
	if status != 200 {
		t.Fatalf("new token %s", b)
	}
	access = obj(b)["access_token"].(string)
	if sdk := os.Getenv("WARDEN_MCP_SDK_ROOT"); sdk != "" {
		script, _ := filepath.Abs("../../../scripts/test-external-mcp.mjs")
		oldAddress := a.DocsAddress
		if realPanta := os.Getenv("WARDEN_TEST_PANTA_URL"); realPanta != "" {
			a.DocsAddress = realPanta
		}
		cmd := exec.Command("node", script)
		cmd.Env = append(os.Environ(), "WARDEN_MCP_URL="+a.Origin+"/mcp", "WARDEN_MCP_ACCESS_TOKEN="+access, "WARDEN_TEST_SESSION="+raw, "WARDEN_TEST_CSRF="+csrf, "WARDEN_TEST_ORGANIZATION="+org.ID)
		if os.Getenv("WARDEN_TEST_PANTA_URL") != "" {
			cmd.Env = append(cmd.Env, "WARDEN_MCP_FULL_TEST=1")
		}
		output, err := cmd.CombinedOutput()
		a.DocsAddress = oldAddress
		if err != nil {
			t.Fatalf("real MCP SDK: %v %s", err, output)
		}
		t.Log(string(output))
	}

	status, b, _ = request("GET", "/auth/agent-connections", nil, raw, "")
	if status != 200 {
		t.Fatalf("connection list: %s", b)
	}
	var connectionID string
	if err = store.db.QueryRowContext(ctx, `SELECT connection_id FROM warden_cloud.agent_tokens WHERE digest=$1`, hashSecret(access)).Scan(&connectionID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), connectionID) || !strings.Contains(string(b), org.Name) {
		t.Fatal("connection list omitted organization/connection")
	}
	bad := httptest.NewRequest("DELETE", a.Origin+"/auth/agent-connections/"+connectionID, nil)
	bad.AddCookie(&http.Cookie{Name: sessionCookie, Value: raw})
	bad.Header.Set("Origin", a.Origin)
	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, bad)
	if refused.Code != 403 {
		t.Fatal("disconnect accepted without CSRF")
	}
	status, _, _ = request("DELETE", "/auth/agent-connections/"+connectionID, nil, raw, "")
	if status != 204 {
		t.Fatal("disconnect failed")
	}
	status, _ = mcp(access, "panta_list_documents", map[string]any{})
	if status != 401 {
		t.Fatal("disconnected token still works")
	}
	code, form = connect()
	form.Set("code", code)
	status, b, _ = request("POST", "/oauth/token", form, "", "")
	if status != 200 {
		t.Fatalf("reconnect failed: %s", b)
	}
	access = obj(b)["access_token"].(string)
	if err = store.RemoveMember(ctx, owner, org.ID, member.Email); err != nil {
		t.Fatal(err)
	}
	status, _ = mcp(access, "panta_list_documents", map[string]any{})
	if status != 401 {
		t.Fatal("removed member retained access")
	}
	if _, err = store.PutMember(ctx, owner, org.ID, member.Email, member.Name, "user"); err != nil {
		t.Fatal(err)
	}
	status, _ = mcp(access, "panta_list_documents", map[string]any{})
	if status != 401 {
		t.Fatal("re-add revived revoked grant")
	}
}
