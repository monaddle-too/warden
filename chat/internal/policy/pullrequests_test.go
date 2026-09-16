package policy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
	shaD = "dddddddddddddddddddddddddddddddddddddddd"
	shaE = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

func TestOnlyChangedDirectoriesAreLoadedAndAncestorsPreserved(t *testing.T) {
	var calls []string
	trees := map[string][]any{
		shaA: {map[string]any{"path": "docs", "type": "tree", "sha": shaB}, map[string]any{"path": "huge-unrelated", "type": "tree", "sha": shaE}, map[string]any{"path": "link", "type": "blob", "mode": "120000", "sha": shaE}},
		shaB: {map[string]any{"path": "page.md", "type": "blob", "mode": "100644", "sha": shaC}},
	}
	call := func(method, path, op string, body map[string]any) (map[string]any, error) {
		calls = append(calls, path)
		return map[string]any{"tree": trees[path[strings.LastIndex(path, "/")+1:]], "truncated": false}, nil
	}
	entries, err := relevantEntries(call, shaA, map[string]bool{"docs/page.md": true, "docs/new.md": true, "link/secret": true, "missing/new": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "/git/trees/"+shaA+",/git/trees/"+shaB {
		t.Fatalf("calls: %v", calls)
	}
	if entries["link"]["mode"] != "120000" || entries["docs/page.md"]["sha"] != shaC {
		t.Fatal("entries")
	}
	if _, ok := entries["docs/new.md"]; ok {
		t.Fatal("missing file present")
	}
	for _, listing := range []map[string]any{{"truncated": true, "tree": []any{}}, {"tree": []any{map[string]any{"path": "../hidden"}}}, {"tree": []any{map[string]any{"path": "same"}, map[string]any{"path": "same"}}}} {
		if _, err := relevantEntries(func(string, string, string, map[string]any) (map[string]any, error) { return listing, nil }, shaA, map[string]bool{"file": true}); err == nil {
			t.Fatalf("%v accepted", listing)
		}
	}
}

type prFixture struct {
	t         *testing.T
	dir       string
	s         *Sharing
	github    *GitHubAppCredentials
	calls     [][3]any
	data      map[string]any
	transport GitHubTransport
	repos     []any
}

func newPRFixture(t *testing.T) *prFixture {
	f := &prFixture{t: t, dir: t.TempDir()}
	f.repos = []any{map[string]any{"id": 7, "full_name": "owner/repo"}}
	f.github = NewGitHubAppCredentials([]string{"/broker"}, "owner", 123, NewRedactor(), nil)
	f.github.run = func(input []byte) ([]byte, error) {
		var value map[string]any
		_ = json.Unmarshal(input, &value)
		if value["action"] == "repositories" {
			return mustJSON(map[string]any{"owner": "owner", "app_id": 123, "repositories": f.repos, "next_page": nil}), nil
		}
		perms, _ := GitHubPermissions(value["operation"].(string))
		result := brokerResult(value["repository"].(string), perms)
		result["token"] = "ghs_synthetic-app-credential-value-1234567890"
		result["repository_id"] = 7
		return mustJSON(result), nil
	}
	s, err := NewSharing(f.dir, nil, nil, f.github)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	f.s = s
	if _, err = s.Dispatch("github_select", map[string]any{"chatID": "chat", "sandboxID": "sandbox", "repositories": []any{"owner/repo"}}); err != nil {
		t.Fatal(err)
	}
	f.transport = f.defaultTransport
	s.PullRequests.Transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
		return f.transport(method, path, token, body)
	}
	f.data = map[string]any{"chatID": "chat", "sandboxID": "sandbox", "callID": "call", "repository": "owner/repo", "base": "main", "title": "Improve greeting", "body": "A clear description.",
		"files": []any{map[string]any{"path": "hello.txt", "content": "hello world\n"}}}
	return f
}

func (f *prFixture) defaultTransport(method, path, token string, body map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, [3]any{method, path, cloneJSON(body)})
	switch {
	case strings.Contains(path, "/git/ref/heads/"):
		return map[string]any{"object": map[string]any{"sha": shaA}}, nil
	case method == "GET" && strings.Contains(path, "/git/commits/"):
		return map[string]any{"tree": map[string]any{"sha": shaB}}, nil
	case method == "GET" && strings.Contains(path, "/git/trees/"):
		return map[string]any{"tree": []any{map[string]any{"path": "hello.txt", "sha": shaC, "type": "blob", "mode": "100644", "size": 6}}, "truncated": false}, nil
	case method == "GET" && strings.Contains(path, "/git/blobs/"):
		return map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("hello\n"))}, nil
	case strings.HasSuffix(path, "/git/trees"):
		return map[string]any{"sha": shaD}, nil
	case strings.HasSuffix(path, "/git/commits"):
		return map[string]any{"sha": shaE}, nil
	case strings.HasSuffix(path, "/git/refs"):
		return map[string]any{"ref": body["ref"]}, nil
	case strings.HasSuffix(path, "/pulls"):
		return map[string]any{"number": 42}, nil
	}
	return nil, errors.New("unexpected " + path)
}

func (f *prFixture) submit() map[string]any {
	f.t.Helper()
	result, err := f.s.Dispatch("pr_submit", f.data)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *prFixture) resolve(id string, allow bool, feedback string, body *string) (map[string]any, error) {
	text := ""
	if body == nil {
		_, proposal, _ := f.s.PullRequests.Row(id)
		text, _ = proposal["body"].(string)
	} else {
		text = *body
	}
	return f.s.Dispatch("pr_resolve", map[string]any{"id": id, "allow": allow, "feedback": feedback, "body": text})
}

func (f *prFixture) waitPublished(id string) string {
	f.t.Helper()
	for i := 0; i < 200; i++ {
		status, _, _ := f.s.PullRequests.Row(id)
		if status != "publishing" {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("publication did not finish")
	return ""
}

func (f *prFixture) posts() [][3]any {
	var out [][3]any
	for _, c := range f.calls {
		if c[0] == "POST" {
			out = append(out, c)
		}
	}
	return out
}

func TestPreviewComputedByHostAndNoWritesBeforeApproval(t *testing.T) {
	f := newPRFixture(t)
	r := f.submit()
	if r["status"] != "pending" {
		t.Fatalf("submit: %v", r)
	}
	preview, _ := f.s.Dispatch("pr_preview", map[string]any{"id": r["request_id"]})
	p := preview["proposal"].(map[string]any)
	diff := Dumps(p["files"].([]any)[0].(map[string]any)["diff"])
	if p["base_sha"] != shaA || !strings.Contains(diff, "-hello") || !strings.Contains(diff, "+hello world") {
		t.Fatalf("proposal: %v", p)
	}
	if len(f.posts()) != 0 {
		t.Fatal("writes before approval")
	}
	if f.submit()["request_id"] != r["request_id"] {
		t.Fatal("not idempotent")
	}
	if _, err := f.s.Dispatch("pr_get", map[string]any{"id": r["request_id"], "chatID": "other", "sandboxID": "sandbox"}); err == nil {
		t.Fatal("other conversation read proposal")
	}
}

func TestRejectionFeedbackDurableAndNoWrite(t *testing.T) {
	f := newPRFixture(t)
	r := f.submit()
	id := r["request_id"].(string)
	if _, err := f.resolve(id, false, "Use a shorter greeting", nil); err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	s, err := NewSharing(f.dir, nil, nil, f.github)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	f.s = s
	delivery := s.Dispatchable(t, "undelivered")["requests"].([]any)[0].(map[string]any)
	if delivery["feedback"] != "Use a shorter greeting" || delivery["status"] != "rejected" {
		t.Fatalf("delivery: %v", delivery)
	}
	f.resolve(id, true, "", nil)
	if status, _, _ := f.s.PullRequests.Row(id); status != "rejected" {
		t.Fatal("rejection changed")
	}
	s.Dispatch("ack", map[string]any{"id": id})
	if len(s.Dispatchable(t, "undelivered")["requests"].([]any)) != 0 {
		t.Fatal("ack")
	}
	if len(f.posts()) != 0 {
		t.Fatal("writes after rejection")
	}
}

func (s *Sharing) Dispatchable(t *testing.T, op string) map[string]any {
	t.Helper()
	result, err := s.Dispatch(op, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestApprovedSnapshotPublishesOnce(t *testing.T) {
	f := newPRFixture(t)
	r := f.submit()
	id := r["request_id"].(string)
	f.data["files"] = []any{map[string]any{"path": "hello.txt", "content": "unreviewed mutation"}}
	if _, err := f.resolve(id, true, "", nil); err != nil {
		t.Fatal(err)
	}
	if f.waitPublished(id) != "published" {
		t.Fatal("not published")
	}
	out, _ := f.s.Dispatch("pr_get", map[string]any{"id": id, "chatID": "chat", "sandboxID": "sandbox"})
	if out["url"] != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("out: %v", out)
	}
	writes := f.posts()
	if len(writes) != 4 {
		t.Fatalf("writes: %d", len(writes))
	}
	tree := writes[0][2].(map[string]any)["tree"].([]any)[0].(map[string]any)
	if tree["content"] != "hello world\n" {
		t.Fatal("reviewed snapshot not used")
	}
	if !jsonEqual(writes[1][2].(map[string]any)["parents"], []any{shaA}) || writes[3][2].(map[string]any)["body"] != "A clear description." || writes[3][2].(map[string]any)["head"] != "warden/pr-"+id[:24] {
		t.Fatalf("writes: %v", writes)
	}
	f.resolve(id, true, "", nil)
	f.s.PullRequests.Publish(id)
	if len(f.posts()) != 4 {
		t.Fatal("republished")
	}
}

func TestChangedBaseAndRevokedRepositoryBlockWrites(t *testing.T) {
	for _, mode := range []string{"base", "revoke"} {
		f := newPRFixture(t)
		f.data["callID"] = mode
		id := f.submit()["request_id"].(string)
		f.s.PullRequests.Transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
			if mode == "base" {
				return map[string]any{"object": map[string]any{"sha": shaE}}, nil
			}
			return f.defaultTransport(method, path, token, body)
		}
		if mode == "revoke" {
			f.s.Dispatch("github_select", map[string]any{"chatID": "chat", "sandboxID": "sandbox", "repositories": []any{}})
		}
		if _, err := f.resolve(id, true, "", nil); err != nil {
			t.Fatal(err)
		}
		if f.waitPublished(id) != "failed" {
			t.Fatalf("%s: not failed", mode)
		}
		if len(f.posts()) != 0 {
			t.Fatalf("%s: wrote", mode)
		}
	}
}

func TestAmbiguousFailureAndRestartNeverRepublish(t *testing.T) {
	f := newPRFixture(t)
	id := f.submit()["request_id"].(string)
	f.transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
		if strings.HasSuffix(path, "/pulls") {
			return nil, errors.New("sensitive upstream diagnostic")
		}
		return f.defaultTransport(method, path, token, body)
	}
	f.resolve(id, true, "", nil)
	if f.waitPublished(id) != "failed" {
		t.Fatal("not failed")
	}
	out, _ := f.s.Dispatch("pr_get", map[string]any{"id": id, "chatID": "chat", "sandboxID": "sandbox"})
	if out["status"] != "failed" || strings.Contains(Dumps(out), "sensitive") || out["branch_url"] == nil {
		t.Fatalf("out: %v", out)
	}
	writes := len(f.calls)
	f.resolve(id, true, "", nil)
	f.s.PullRequests.Publish(id)
	if len(f.calls) != writes {
		t.Fatal("republished")
	}
	f.data["callID"] = "interrupted"
	f.transport = f.defaultTransport
	id = f.submit()["request_id"].(string)
	// Mark publishing durably and restart before the goroutine finishes.
	f.s.mu.Lock()
	f.s.DB.Exec("UPDATE pull_requests SET status='publishing' WHERE id=?", id)
	f.s.mu.Unlock()
	f.s.Close()
	s, err := NewSharing(f.dir, nil, nil, f.github)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if status, _, _ := s.PullRequests.Row(id); status != "failed" {
		t.Fatalf("interrupted publication status %s", status)
	}
}

func TestInputBoundariesAndUnselectedRepositories(t *testing.T) {
	f := newPRFixture(t)
	original := cloneJSON(f.data).(map[string]any)
	for _, changed := range []map[string]any{{"repository": "other/repo"}, {"base": "../main"}, {"files": []any{map[string]any{"path": "../secret", "content": "x"}}},
		{"files": []any{map[string]any{"path": ".github/workflows/run.yml", "content": "x"}}}, {"files": []any{map[string]any{"path": "a", "content": "\x00"}}},
		{"files": []any{map[string]any{"path": "a", "content": strings.Repeat("x", 262145)}}}, {"files": []any{map[string]any{"path": "a", "content": "x"}, map[string]any{"path": "a/b", "content": "y"}}}} {
		f.data = cloneJSON(original).(map[string]any)
		for k, v := range changed {
			f.data[k] = v
		}
		if f.submit()["status"] != "invalid" {
			t.Fatalf("%v accepted", changed)
		}
	}
	if len(f.posts()) != 0 {
		t.Fatal("writes")
	}
}

func TestDeletionAdditionAndDiffMarkerContent(t *testing.T) {
	f := newPRFixture(t)
	f.data["files"] = []any{map[string]any{"path": "hello.txt", "content": nil}, map[string]any{"path": "new.txt", "content": "++literal\n--literal"}}
	id := f.submit()["request_id"].(string)
	preview, _ := f.s.Dispatch("pr_preview", map[string]any{"id": id})
	files := preview["proposal"].(map[string]any)["files"].([]any)
	added := files[1].(map[string]any)
	if n, _ := asInt(added["additions"]); n != 2 {
		t.Fatalf("additions: %v", added)
	}
	if !strings.Contains(Dumps(added["diff"]), "\\\\ No newline at end of file") {
		t.Fatalf("diff: %v", added["diff"])
	}
	f.resolve(id, true, "", nil)
	f.waitPublished(id)
	for _, c := range f.posts() {
		if strings.HasSuffix(c[1].(string), "/git/trees") {
			entry := c[2].(map[string]any)["tree"].([]any)[0].(map[string]any)
			if sha, present := entry["sha"]; !present || sha != nil {
				t.Fatalf("deletion entry: %v", entry)
			}
		}
	}
}

func TestExactEditedBodyIsThePublishedBody(t *testing.T) {
	f := newPRFixture(t)
	id := f.submit()["request_id"].(string)
	edited := "## Reviewed by owner\n\nKeep **this** wording — exactly.\n"
	if _, err := f.resolve(id, true, "", &edited); err != nil {
		t.Fatal(err)
	}
	preview, _ := f.s.Dispatch("pr_preview", map[string]any{"id": id})
	if preview["proposal"].(map[string]any)["body"] != edited {
		t.Fatal("body not stored")
	}
	f.waitPublished(id)
	for _, c := range f.calls {
		if strings.HasSuffix(c[1].(string), "/pulls") && c[2].(map[string]any)["body"] != edited {
			t.Fatal("published body differs")
		}
	}
}

func TestBodyMismatchAndMissingReviewFailClosed(t *testing.T) {
	f := newPRFixture(t)
	id := f.submit()["request_id"].(string)
	if _, err := f.s.Dispatch("pr_resolve", map[string]any{"id": id, "allow": true}); err == nil {
		t.Fatal("approval without body")
	}
	if status, _, _ := f.s.PullRequests.Row(id); status != "pending" {
		t.Fatal("status changed")
	}
	approved := "Approved body\n"
	if _, err := f.resolve(id, true, "", &approved); err != nil {
		t.Fatal(err)
	}
	retry := "Unreviewed retry"
	if _, err := f.resolve(id, true, "", &retry); err == nil {
		t.Fatal("unreviewed retry accepted")
	}
	f.waitPublished(id)
	_, proposal, _ := f.s.PullRequests.Row(id)
	for _, candidate := range []string{"Changed body\n", "Approved body", "Approved body\r\n"} {
		if err := reviewedBody(proposal, candidate); err == nil {
			t.Fatalf("%q accepted", candidate)
		}
	}
	f.calls = nil
	f.s.mu.Lock()
	f.s.DB.Exec("UPDATE pull_requests SET status='publishing' WHERE id=?", id)
	f.s.mu.Unlock()
	proposal["body"] = "Changed after approval"
	f.s.PullRequests.SetProposal(id, proposal)
	f.s.PullRequests.Publish(id)
	if status, _, _ := f.s.PullRequests.Row(id); status != "failed" || len(f.posts()) != 0 {
		t.Fatal("tampered body published")
	}
}

func TestRejectionReturnsOwnerBodyEditsForRevision(t *testing.T) {
	f := newPRFixture(t)
	id := f.submit()["request_id"].(string)
	suggested := "Suggested replacement body"
	f.resolve(id, false, "Revise the code too", &suggested)
	result, _ := f.s.Dispatch("pr_get", map[string]any{"id": id, "chatID": "chat", "sandboxID": "sandbox"})
	if result["edited_body"] != suggested || result["feedback"] != "Revise the code too" || len(f.posts()) != 0 {
		t.Fatalf("result: %v", result)
	}
}

func TestReviewNeverGrantsAgentPermissionToPostADifferentBody(t *testing.T) {
	f := newPRFixture(t)
	engine := newTestEngine(t, f.dir+"/engine", nil)
	id := f.submit()["request_id"].(string)
	approved := "Owner approved body"
	f.resolve(id, true, "", &approved)
	body, _ := json.Marshal(map[string]any{"title": "Improve greeting", "head": "branch", "base": "main", "body": "Unreviewed agent body"})
	req := map[string]any{"host": "api.github.com", "scheme": "https", "port": 443, "method": "POST", "path": "/repos/owner/repo/pulls",
		"headers": []any{[]any{"content-type", "application/json"}}, "body_base64": base64.StdEncoding.EncodeToString(body)}
	if _, _, ok, _ := f.s.GitHubGrant("chat", "sandbox", engine, req); ok {
		t.Fatal("write granted through sharing")
	}
	decision, err := engine.Authorize(req)
	if err != nil || decision["allow"] == true || decision["authorization"] != nil {
		t.Fatalf("decision: %v", decision)
	}
}

func TestImagesAreReviewedAndPublishedWithoutRewritingBody(t *testing.T) {
	f := newPRFixture(t)
	image, err := f.s.Dispatch("image_add", map[string]any{"chatID": "chat", "sandboxID": "sandbox", "caption": "Screenshot", "png": base64.StdEncoding.EncodeToString(testPNG)})
	if err != nil {
		t.Fatal(err)
	}
	f.data["images"] = []any{image["image_id"]}
	id := f.submit()["request_id"].(string)
	preview, _ := f.s.Dispatch("pr_preview", map[string]any{"id": id})
	p := preview["proposal"].(map[string]any)
	body, _ := p["body"].(string)
	if !strings.Contains(body, "../blob/"+p["head"].(string)+"/.warden/images/") || p["images"].([]any)[0].(map[string]any)["image_id"] != image["image_id"] {
		t.Fatalf("proposal: %v", p)
	}
	f.transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
		if method == "POST" && strings.HasSuffix(path, "/git/blobs") {
			f.calls = append(f.calls, [3]any{method, path, cloneJSON(body)})
			return map[string]any{"sha": shaC}, nil
		}
		return f.defaultTransport(method, path, token, body)
	}
	f.resolve(id, true, "", nil)
	if f.waitPublished(id) != "published" {
		t.Fatal("not published")
	}
	for _, c := range f.posts() {
		path := c[1].(string)
		payload := c[2].(map[string]any)
		switch {
		case strings.HasSuffix(path, "/pulls"):
			if payload["body"] != body {
				t.Fatal("body rewritten")
			}
		case strings.HasSuffix(path, "/git/blobs"):
			decoded, _ := base64.StdEncoding.DecodeString(payload["content"].(string))
			if string(decoded) != string(testPNG) {
				t.Fatal("blob content")
			}
		case strings.HasSuffix(path, "/git/trees"):
			entries := payload["tree"].([]any)
			if entries[len(entries)-1].(map[string]any)["sha"] != shaC {
				t.Fatal("image tree entry")
			}
		}
	}
}

func TestForeignImageCannotBeAttachedToProposal(t *testing.T) {
	f := newPRFixture(t)
	image, _ := f.s.Dispatch("image_add", map[string]any{"chatID": "other", "sandboxID": "sandbox", "caption": "Private", "png": base64.StdEncoding.EncodeToString(testPNG)})
	f.data["images"] = []any{image["image_id"]}
	if f.submit()["status"] != "invalid" || len(f.posts()) != 0 {
		t.Fatal("foreign image attached")
	}
}

func TestUnifiedDiffShape(t *testing.T) {
	lines := UnifiedDiff(splitKeepEnds("a\nb\nc\nd\ne\nf\ng\n"), splitKeepEnds("a\nb\nc\nD\ne\nf\ng\n"), "a/x", "b/x")
	want := []string{"--- a/x\n", "+++ b/x\n", "@@ -1,7 +1,7 @@\n", " a\n", " b\n", " c\n", "-d\n", "+D\n", " e\n", " f\n", " g\n"}
	if strings.Join(lines, "") != strings.Join(want, "") {
		t.Fatalf("diff:\n%s", strings.Join(lines, ""))
	}
	if UnifiedDiff(splitKeepEnds("same\n"), splitKeepEnds("same\n"), "a", "b") != nil {
		t.Fatal("identical diff not empty")
	}
}
