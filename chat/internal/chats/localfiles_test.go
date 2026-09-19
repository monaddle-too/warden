package chats

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A home directory with files to attach and complete; ~ resolves to it.
func localHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(home, "Documents", "notes"), 0700))
	must(os.MkdirAll(filepath.Join(home, ".hidden"), 0700))
	must(os.WriteFile(filepath.Join(home, "Documents", "report.txt"), []byte("the report"), 0600))
	must(os.WriteFile(filepath.Join(home, "Documents", "Readme.md"), []byte("read me"), 0600))
	must(os.WriteFile(filepath.Join(home, "Documents", "empty.log"), nil, 0600))
	must(os.WriteFile(filepath.Join(home, "Documents", "a.log"), []byte("a"), 0600))
	must(os.WriteFile(filepath.Join(home, "Documents", "b.log"), []byte("b"), 0600))
	must(os.WriteFile(filepath.Join(home, ".secret"), []byte("s"), 0600))
	return home
}

func TestLocalPathsCompleteLikeAShell(t *testing.T) {
	home := localHome(t)
	if got := LocalPaths("~/Documents/"); strings.Join(got, " ") != "~/Documents/notes/ ~/Documents/a.log ~/Documents/b.log ~/Documents/empty.log ~/Documents/Readme.md ~/Documents/report.txt" {
		t.Fatalf("listing: %q", got)
	}
	// The stem is case-folded and the query's spelling is kept.
	if got := LocalPaths("~/Documents/RE"); strings.Join(got, " ") != "~/Documents/Readme.md ~/Documents/report.txt" {
		t.Fatalf("prefix: %q", got)
	}
	// Hidden entries only when asked for; a relative path is from home.
	if got := LocalPaths("~/"); strings.Contains(strings.Join(got, " "), ".hidden") {
		t.Fatalf("hidden listed: %q", got)
	}
	if got := LocalPaths("./."); strings.Join(got, " ") != "./.hidden/ ./.secret" {
		t.Fatalf("dot prefix: %q", got)
	}
	if got := LocalPaths(home + "/Doc"); strings.Join(got, " ") != home+"/Documents/" {
		t.Fatalf("absolute: %q", got)
	}
	if got := LocalPaths("~/nowhere/"); got != nil {
		t.Fatalf("missing directory: %q", got)
	}
	if ExpandLocalPath("~") != home || ExpandLocalPath("../x") != filepath.Join(home, "..", "x") || ExpandLocalPath("/etc//hosts") != "/etc/hosts" {
		t.Fatal("expansion")
	}
}

func TestAttachLocalRoutesAreForALocalInstall(t *testing.T) {
	localHome(t)
	e, _, _ := setup(t)
	id, err := e.Create("Files", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &HTTP{Engine: e, Token: "owner-secret", Host: "localhost:18780", Origin: "http://localhost:18780"}
	call := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		var r *httptest.ResponseRecorder = httptest.NewRecorder()
		req := httptest.NewRequest(method, "http://localhost:18780/api/"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer owner-secret")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		h.ServeHTTP(r, req)
		return r
	}
	// Not a local install: both routes refuse.
	if rec := call("GET", "chats/"+id+"/local-paths?q=~/", "", nil); rec.Code != 403 || !strings.Contains(rec.Body.String(), "local Warden install") {
		t.Fatalf("paths off: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("POST", "chats/"+id+"/attach-local", `{"paths":["~/Documents/report.txt"]}`, nil); rec.Code != 403 {
		t.Fatalf("attach off: %d %s", rec.Code, rec.Body.String())
	}
	e.LocalMode = true
	// A collaborator admitted by the edge is not the owner.
	guest := map[string]string{"X-Warden-Principal": "someone", "X-Warden-Role": "member"}
	if rec := call("GET", "chats/"+id+"/local-paths?q=~/", "", guest); rec.Code != 403 {
		t.Fatalf("paths for a guest: %d", rec.Code)
	}
	if rec := call("POST", "chats/"+id+"/attach-local", `{"paths":["~/Documents/report.txt"]}`, guest); rec.Code != 403 {
		t.Fatalf("attach for a guest: %d", rec.Code)
	}
	rec := call("GET", "chats/"+id+"/local-paths?q=~/Documents/re", "", nil)
	var listing struct{ Paths []string }
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); rec.Code != 200 || err != nil || strings.Join(listing.Paths, " ") != "~/Documents/Readme.md ~/Documents/report.txt" {
		t.Fatalf("paths: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("GET", "chats/missing/local-paths?q=~/", "", nil); rec.Code != 404 {
		t.Fatalf("paths of a missing chat: %d", rec.Code)
	}
	if rec := call("POST", "chats/"+id+"/attach-local", `{"paths":[]}`, nil); rec.Code != 400 {
		t.Fatalf("no paths: %d", rec.Code)
	}
	// A plain path and a glob; the limit stops the batch at the third
	// file, naming the match the way it was typed.
	rec = call("POST", "chats/"+id+"/attach-local", `{"paths":["~/Documents/report.txt","~/Documents/*.log"],"limit":3}`, nil)
	var res LocalAttachResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); rec.Code != 200 || err != nil {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body.String())
	}
	var names []string
	for _, a := range res.Attached {
		names = append(names, a.Name)
		if a.Kind != "file" || !strings.HasPrefix(a.Path, ".warden/attachments/") {
			t.Fatalf("record: %+v", a)
		}
		if b, err := os.ReadFile(filepath.Join(e.Store.dir(), "attachments", id, a.ID)); err != nil || len(b) == 0 {
			t.Fatalf("stored copy of %s: %v", a.Name, err)
		}
	}
	if strings.Join(names, " ") != "report.txt a.log b.log" {
		t.Fatalf("attached: %q", names)
	}
	if strings.Join(res.Errors, "\n") != "a message can carry at most 8 attachments; ~/Documents/empty.log was not attached" {
		t.Fatalf("errors: %q", res.Errors)
	}
	// A directory, a missing file, a pattern that matches nothing, an
	// empty file, and a relative path (from home), each answered by name.
	rec = call("POST", "chats/"+id+"/attach-local", `{"paths":["~/Documents/notes","~/nowhere.txt","~/Documents/*.zip","~/Documents/empty.log","./Documents/Readme.md"]}`, nil)
	res = LocalAttachResult{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); rec.Code != 200 || err != nil || len(res.Attached) != 1 || res.Attached[0].Name != "Readme.md" {
		t.Fatalf("second attach: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Join(res.Errors, "\n") != "~/Documents/notes: is a directory; attach a file\n~/nowhere.txt: no such file\n~/Documents/*.zip: matches nothing\n~/Documents/empty.log: is empty" {
		t.Fatalf("errors: %q", res.Errors)
	}
	if strings.Join(res.Missing, " ") != "~/Documents/notes ~/nowhere.txt ~/Documents/*.zip" || res.Attached[0].Typed != "./Documents/Readme.md" {
		t.Fatalf("missing %q, typed %q", res.Missing, res.Attached[0].Typed)
	}
	// The state tells clients.
	if e.View().AgentOptions.LocalFiles != true {
		t.Fatal("agentOptions.localFiles")
	}
}
