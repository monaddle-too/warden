package cloudauth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestReferencesPostgres(t *testing.T) {
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
	org, err := store.CreateOrganization(ctx, owner, "References "+ID())
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateOrganization(ctx, owner, "References other "+ID())
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.PutMember(ctx, owner, org.ID, ID()+"@example.com", "Reader", "user")
	if err != nil {
		t.Fatal(err)
	}
	session, token, err := store.newPasskeySession(ctx, member.User)
	if err != nil {
		t.Fatal(err)
	}
	// Production's metadata projection, no transcript query required.
	_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS warden_cloud.chat_records(instance TEXT NOT NULL,id TEXT NOT NULL,position INTEGER NOT NULL,record JSONB NOT NULL,PRIMARY KEY(instance,id))`)
	if err != nil {
		t.Fatal(err)
	}
	localID, foreignID := ID(), ID()
	for _, row := range []struct{ id, org, title string }{{localID, org.ID, "Annual planning"}, {foreignID, other.ID, "Annual planning secret"}} {
		data, _ := json.Marshal(map[string]any{"title": row.title, "organizationID": row.org})
		_, err = store.db.Exec(`INSERT INTO warden_cloud.chat_records VALUES('main',$1,0,$2::jsonb)`, row.id, string(data))
		if err != nil {
			t.Fatal(err)
		}
	}
	defer store.db.Exec(`DELETE FROM warden_cloud.chat_records WHERE id IN ($1,$2)`, localID, foreignID)
	client, connection, shared := ID(), ID(), ID()
	_, err = store.db.Exec(`INSERT INTO warden_cloud.agent_clients(id,name,redirects) VALUES($1,'Test','[]')`, client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO warden_cloud.agent_connections(id,client_id,user_id,organization_id) VALUES($1,$2,$3,$4)`, connection, client, member.ID, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`INSERT INTO warden_cloud.shared_conversations(id,organization_id,user_id,connection_id,operation_id,fingerprint,title,messages) VALUES($1,$2,$3,$4,'test','test','Annual planning','[{"role":"user","content":"TRANSCRIPT MUST NOT LEAK"}]')`, shared, org.ID, member.ID, connection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.db.Exec(`DELETE FROM warden_cloud.shared_conversations WHERE id=$1`, shared)
		store.db.Exec(`DELETE FROM warden_cloud.agent_connections WHERE id=$1`, connection)
		store.db.Exec(`DELETE FROM warden_cloud.agent_clients WHERE id=$1`, client)
	}()
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Panta-Organization") != org.ID || r.Header.Get("X-Panta-Key") != "secret" || r.Header.Get("Cookie") != "" {
			t.Error("wrong scope")
		}
		respond(w, 200, map[string]any{"items": []Reference{{Kind: "document", ID: "doc", Title: "Annual planning", URL: "/documents/doc", Status: "deleted"}}})
	}))
	defer docs.Close()
	a := &Auth{Login: &Login{Store: store}, Origin: "https://app.example", DocsAddress: docs.URL, DocsKey: "secret"}
	call := func(method, path string, input any, cookie, csrf string, want int) string {
		t.Helper()
		data, _ := json.Marshal(input)
		r := httptest.NewRequest(method, a.Origin+path, bytes.NewReader(data))
		r.Header.Set("Origin", a.Origin)
		r.Header.Set("X-Warden-CSRF", csrf)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		if !a.ReferenceRoute(w, r) {
			t.Fatal("route not handled")
		}
		if w.Code != want {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	call("GET", "/api/references/search", nil, "", "", 401)
	result := call("GET", "/api/references/search?q=Annual%20planning", nil, token, "", 200)
	for _, forbidden := range []string{foreignID, "TRANSCRIPT MUST NOT LEAK", "messages", "secret"} {
		if strings.Contains(result, forbidden) {
			t.Fatalf("leaked %s: %s", forbidden, result)
		}
	}
	for _, id := range []string{localID, shared, "doc"} {
		if !strings.Contains(result, id) {
			t.Fatalf("missing %s: %s", id, result)
		}
	}
	if !strings.Contains(result, `"status":"deleted"`) {
		t.Fatal("deleted status lost: " + result)
	}
	input := map[string]any{"references": []referenceKey{{Kind: "chat", ID: localID}, {Kind: "chat", ID: foreignID}, {Kind: "chat", ID: ID()}}}
	call("POST", "/api/references/resolve", input, token, "", 403)
	result = call("POST", "/api/references/resolve", input, token, session.CSRF, 200)
	if strings.Contains(result, foreignID) || !strings.Contains(result, localID) {
		t.Fatal(result)
	}
	_, err = store.db.Exec(`UPDATE warden_cloud.chat_records SET record=jsonb_set(record,'{title}','"Renamed"') WHERE id=$1`, localID)
	if err != nil {
		t.Fatal(err)
	}
	if result = call("POST", "/api/references/resolve", input, token, session.CSRF, 200); !strings.Contains(result, "Renamed") {
		t.Fatal(result)
	}
	if result = call("GET", "/api/references/search?q=%25", nil, token, "", 200); strings.Contains(result, localID) {
		t.Fatal("wildcard interpreted")
	}
	_, err = store.db.Exec(`DELETE FROM warden_cloud.memberships WHERE user_id=$1 AND organization_id=$2`, member.ID, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	call("GET", "/api/references/search", nil, token, "", 401)
}
