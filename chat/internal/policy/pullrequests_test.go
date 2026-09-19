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
		{"files": []any{map[string]any{"path": "a", "content": strings.Repeat("x", 1048577)}}}, {"files": []any{map[string]any{"path": "a", "content": "x"}, map[string]any{"path": "a/b", "content": "y"}}}} {
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

// An update proposal (pull_request set) is reviewed against the open pull
// request's head and, approved, committed onto its warden/pr-… branch:
// one tree, one commit, one ref update, no new branch or pull request.
func TestUpdateProposalCommitsOntoTheWardenBranch(t *testing.T) {
	f := newPRFixture(t)
	var patches [][3]any
	f.transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
		switch {
		case method == "GET" && strings.HasSuffix(path, "/pulls/42"):
			return map[string]any{"number": 42, "state": "open", "head": map[string]any{"ref": "warden/pr-abc", "sha": shaA, "repo": map[string]any{"full_name": "owner/repo"}}, "base": map[string]any{"ref": "main"}}, nil
		case method == "GET" && strings.HasSuffix(path, "/pulls/43"):
			return map[string]any{"number": 43, "state": "open", "head": map[string]any{"ref": "feature/hand-made", "sha": shaA, "repo": map[string]any{"full_name": "owner/repo"}}, "base": map[string]any{"ref": "main"}}, nil
		case method == "PATCH":
			patches = append(patches, [3]any{method, path, cloneJSON(body)})
			return map[string]any{"ref": "refs/heads/warden/pr-abc", "object": map[string]any{"sha": body["sha"]}}, nil
		}
		return f.defaultTransport(method, path, token, body)
	}
	delete(f.data, "base")
	f.data["pull_request"] = 43
	if r, err := f.s.Dispatch("pr_submit", f.data); err != nil || r["status"] != "invalid" || !strings.Contains(r["error"].(string), "warden/pr-") {
		t.Fatalf("a hand-made branch was accepted for update: %v %v", r, err)
	}
	f.data["pull_request"] = 42
	f.data["callID"] = "update"
	r := f.submit()
	id := r["request_id"].(string)
	if r["status"] != "pending" || r["update"] != true || r["head"] != "warden/pr-abc" {
		t.Fatalf("submit: %v", r)
	}
	preview, _ := f.s.Dispatch("pr_preview", map[string]any{"id": id})
	proposal := preview["proposal"].(map[string]any)
	if proposal["base"] != "warden/pr-abc" || proposal["head"] != "warden/pr-abc" || proposal["pull_request"].(map[string]any)["base"] != "main" {
		t.Fatalf("proposal: %v", proposal)
	}
	if _, err := f.resolve(id, true, "", nil); err != nil {
		t.Fatal(err)
	}
	if f.waitPublished(id) != "published" {
		t.Fatal("not published")
	}
	out, _ := f.s.Dispatch("pr_get", map[string]any{"id": id, "chatID": "chat", "sandboxID": "sandbox"})
	if out["url"] != "https://github.com/owner/repo/pull/42" || out["commit"] != shaE || out["pull_request"] != 42.0 && out["pull_request"] != int64(42) {
		t.Fatalf("out: %v", out)
	}
	writes := f.posts()
	if len(writes) != 2 || !strings.HasSuffix(writes[0][1].(string), "/git/trees") || !strings.HasSuffix(writes[1][1].(string), "/git/commits") {
		t.Fatalf("writes: %v", writes)
	}
	if msg := writes[1][2].(map[string]any)["message"]; msg != "Improve greeting\n\nA clear description." {
		t.Fatalf("commit message: %q", msg)
	}
	if len(patches) != 1 || patches[0][1] != "/repos/owner/repo/git/refs/heads/warden/pr-abc" || patches[0][2].(map[string]any)["sha"] != shaE || patches[0][2].(map[string]any)["force"] != false {
		t.Fatalf("patches: %v", patches)
	}
	// A base that is not the pull request's is refused.
	f.data["callID"], f.data["base"] = "wrong-base", "develop"
	if r, err := f.s.Dispatch("pr_submit", f.data); err != nil || r["status"] != "invalid" {
		t.Fatalf("a foreign base was accepted: %v %v", r, err)
	}
}

// view_ci_results: the head commit's check runs, and for a failed
// Actions job its steps and log tail with the error lines; nothing is
// written and the log's credential never reaches the download host.
func TestChecksReadRunsStepsAndLogTail(t *testing.T) {
	f := newPRFixture(t)
	f.transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
		f.calls = append(f.calls, [3]any{method, path, cloneJSON(body)})
		switch {
		case method == "GET" && strings.HasSuffix(path, "/pulls/9"):
			return map[string]any{"number": 9, "state": "open", "head": map[string]any{"ref": "warden/pr-x", "sha": shaB}}, nil
		case method == "GET" && strings.Contains(path, "/commits/"+shaB+"/check-runs"):
			return map[string]any{"check_runs": []any{
				map[string]any{"id": 501, "name": "Go", "status": "completed", "conclusion": "failure", "html_url": "https://github.com/owner/repo/actions/runs/1/job/501", "app": map[string]any{"slug": "github-actions"}, "output": map[string]any{"title": "Process completed with exit code 1.", "summary": ""}},
				map[string]any{"id": 502, "name": "Web", "status": "completed", "conclusion": "success", "app": map[string]any{"slug": "github-actions"}, "output": map[string]any{}},
			}}, nil
		case method == "GET" && strings.HasSuffix(path, "/actions/jobs/501"):
			return map[string]any{"run_id": 1, "steps": []any{map[string]any{"name": "Set up job", "status": "completed", "conclusion": "success"}, map[string]any{"name": "gofmt, vet, test", "status": "completed", "conclusion": "failure"}}}, nil
		}
		return nil, errors.New("unexpected " + path)
	}
	logPath := ""
	f.s.PullRequests.Logs = func(path, token string) (string, error) {
		logPath = path
		return "2026-09-19T19:00:00Z ok  \tpkg/a\n2026-09-19T19:00:01Z ##[error]internal/x/x.go:3:1: undefined: nope\n2026-09-19T19:00:02Z ##[error]Process completed with exit code 1.\n", nil
	}
	out, err := f.s.Dispatch("pr_checks", map[string]any{"chatID": "chat", "sandboxID": "sandbox", "repository": "owner/repo", "pull_request": 9})
	if err != nil {
		t.Fatal(err)
	}
	if out["conclusion"] != "failure" || out["ref"] != shaB || out["url"] != "https://github.com/owner/repo/pull/9" || len(out["checks"].([]any)) != 2 {
		t.Fatalf("out: %v", out)
	}
	failed := out["failed_jobs"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed: %v", failed)
	}
	job := failed[0].(map[string]any)
	if job["name"] != "Go" || len(job["steps"].([]any)) != 2 || len(job["errors"].([]string)) != 2 || !strings.Contains(job["log_tail"].(string), "undefined: nope") || logPath != "/repos/owner/repo/actions/jobs/501/logs" {
		t.Fatalf("job: %v", job)
	}
	if len(f.posts()) != 0 {
		t.Fatal("a read wrote")
	}
	// Pending and absent checks say so; a bad ref is refused.
	f.transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
		if strings.Contains(path, "/check-runs") {
			if strings.Contains(path, "/commits/main/") {
				return map[string]any{"check_runs": []any{map[string]any{"id": 7, "name": "Go", "status": "in_progress", "app": map[string]any{"slug": "github-actions"}}}}, nil
			}
			return map[string]any{"check_runs": []any{}}, nil
		}
		return nil, errors.New("unexpected " + path)
	}
	if out, err = f.s.Dispatch("pr_checks", map[string]any{"chatID": "chat", "sandboxID": "sandbox", "repository": "owner/repo", "ref": "main"}); err != nil || out["conclusion"] != "pending" {
		t.Fatalf("pending: %v %v", out, err)
	}
	if out, err = f.s.Dispatch("pr_checks", map[string]any{"chatID": "chat", "sandboxID": "sandbox", "repository": "owner/repo", "ref": shaC}); err != nil || out["conclusion"] != "none" {
		t.Fatalf("none: %v %v", out, err)
	}
	if _, err = f.s.Dispatch("pr_checks", map[string]any{"chatID": "chat", "sandboxID": "sandbox", "repository": "owner/repo", "ref": "../x"}); err == nil {
		t.Fatal("bad ref accepted")
	}
	tail, lines := logTail("a\nb\n##[error]c\nd\n", 4)
	if tail != "d\n" || len(lines) != 1 {
		t.Fatalf("logTail: %q %v", tail, lines)
	}
}
