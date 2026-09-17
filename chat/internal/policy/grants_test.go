package policy

import (
	"path/filepath"
	"strings"
	"testing"
)

type fakeNetwork struct{ calls []string }

func (f *fakeNetwork) AllowHost(sandbox, host string, until float64) error {
	f.calls = append(f.calls, sandbox+" "+host)
	return nil
}

// A temporary host grant lets ordinary egress through for one host until it
// lapses, beside the destination list; the sharing operation validates the
// request, applies it to the sandbox and records it in the history.
func TestNetworkAllowGrantsOneHostUntilExpiry(t *testing.T) {
	dir := t.TempDir()
	clock := &testClock{now: 1000, mono: 1000}
	engine := newTestEngine(t, dir, clock)
	decide := func(host string) bool {
		r, err := engine.AuthorizeEgress(map[string]any{"host": host, "method": "GET", "scheme": "https"})
		if err != nil {
			t.Fatal(err)
		}
		return r["allow"] == true
	}
	if decide("example.org") {
		t.Fatal("restricted policy let example.org through")
	}
	if err := engine.AllowHost("example.org", 1600); err != nil {
		t.Fatal(err)
	}
	if err := engine.AllowHost("bad host", 1600); err == nil {
		t.Fatal("invalid host accepted")
	}
	if !decide("example.org") || decide("example.net") || !engine.HostAllowed("example.org") {
		t.Fatal("grant not applied to exactly one host")
	}
	clock.now = 1601
	if decide("example.org") || engine.HostAllowed("example.org") {
		t.Fatal("grant outlived its expiry")
	}

	f := newSharingFixture(t)
	network := &fakeNetwork{}
	f.s.Network = network
	if _, err := f.s.Dispatch("network_allow", map[string]any{"sandboxID": "sbx-a", "host": "Example.org.", "duration": 3600, "reason": "docs", "actor": "Ada"}); err != nil {
		t.Fatal(err)
	}
	if len(network.calls) != 1 || network.calls[0] != "sbx-a example.org" {
		t.Fatalf("calls: %v", network.calls)
	}
	for _, bad := range []map[string]any{{"sandboxID": "sbx-a", "host": "example.org", "duration": 30}, {"sandboxID": "sbx-a", "host": "example.org", "duration": 90000}, {"sandboxID": "sbx-a", "host": "127.0.0.1", "duration": 600}, {"sandboxID": "", "host": "example.org", "duration": 600}} {
		if _, err := f.s.Dispatch("network_allow", bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	history := f.dispatch("history", map[string]any{"sandboxID": "sbx-a"})["events"].([]any)
	if e := history[0].(map[string]any); e["kind"] != "network_allowed" || e["resolved_by"] != "Ada" {
		t.Fatalf("history: %v", e)
	}
}

// github_write posts one approved comment, issue or label set with the
// owner's credential to a repository shared with the sandbox, and nothing
// else.
func TestGitHubWritePostsApprovedPayloadOnly(t *testing.T) {
	api := newUserAPI(t, 1)
	source, _ := newUserSource(t, api)
	s, err := NewSharing(t.TempDir(), nil, nil, source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var posts []string
	s.PullRequests.Transport = func(method, path, token string, body map[string]any) (map[string]any, error) {
		posts = append(posts, method+" "+path+" "+Dumps(body)+" "+token)
		return map[string]any{"html_url": "https://github.com/owner/repo1/issues/7#issuecomment-1", "number": 7}, nil
	}
	if _, err = s.Dispatch("github_write", map[string]any{"sandboxID": "s1", "repository": "owner/repo1", "action": "comment_issue", "number": 7, "body": "hi"}); err == nil {
		t.Fatal("write to an unshared repository accepted")
	}
	if _, err = s.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{"owner/repo1"}}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Dispatch("github_write", map[string]any{"sandboxID": "s1", "repository": "owner/repo1", "action": "comment_issue", "number": 7, "body": "hi", "actor": "Ada"})
	if err != nil || r["url"] != "https://github.com/owner/repo1/issues/7#issuecomment-1" {
		t.Fatalf("comment: %v %v", r, err)
	}
	if _, err = s.Dispatch("github_write", map[string]any{"sandboxID": "s1", "repository": "owner/repo1", "action": "create_issue", "title": "Bug", "body": "details"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Dispatch("github_write", map[string]any{"sandboxID": "s1", "repository": "owner/repo1", "action": "add_labels", "number": 7, "labels": []any{"bug"}}); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 3 || !strings.HasPrefix(posts[0], "POST /repos/owner/repo1/issues/7/comments {\"body\":\"hi\"} "+userToken) || !strings.HasPrefix(posts[1], "POST /repos/owner/repo1/issues {\"body\":\"details\",\"title\":\"Bug\"}") || !strings.HasPrefix(posts[2], "POST /repos/owner/repo1/issues/7/labels {\"labels\":[\"bug\"]}") {
		t.Fatalf("posts: %v", posts)
	}
	for _, bad := range []map[string]any{{"action": "delete_issue", "number": 7}, {"action": "comment_issue", "body": "no number"}, {"action": "create_issue", "body": "no title"}, {"action": "add_labels", "number": 7, "labels": []any{}}} {
		bad["sandboxID"], bad["repository"] = "s1", "owner/repo1"
		if _, err = s.Dispatch("github_write", bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	history, _ := s.Dispatch("history", map[string]any{"sandboxID": "s1"})
	if e := history["events"].([]any)[0].(map[string]any); e["kind"] != "github_write" {
		t.Fatalf("history: %v", e)
	}
	_ = filepath.Join
}

// Sheets requests classify like Docs: reads of a spreadsheet or its values,
// writes of values and structure; anything else is refused.
func TestGoogleSheetsOperations(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, access, id string
	}{
		{"GET", "/v4/spreadsheets/abc", "", "read", "abc"},
		{"GET", "/v4/spreadsheets/abc/values/Sheet1!A1:B2", "", "read", "abc"},
		{"GET", "/v4/spreadsheets/abc/values:batchGet", "", "read", "abc"},
		{"POST", "/v4/spreadsheets/abc:batchUpdate", `{"requests":[]}`, "structure", "abc"},
		{"PUT", "/v4/spreadsheets/abc/values/A1", `{"values":[[1]]}`, "write", "abc"},
		{"POST", "/v4/spreadsheets/abc/values/A1:append", `{"values":[[1]]}`, "write", "abc"},
		{"POST", "/v4/spreadsheets/abc/values:batchUpdate", `{"data":[]}`, "write", "abc"},
		{"DELETE", "/v4/spreadsheets/abc", "", "", ""},
		{"GET", "/v4/spreadsheets", "", "", ""},
		{"POST", "/v4/spreadsheets/abc/values/A1", "", "", ""},
		{"POST", "/v4/spreadsheets", `{"properties":{}}`, "", ""},
	} {
		access, id, err := DocumentWriteOperation(tc.method, tc.path, nil, []byte(tc.body))
		if tc.access == "" {
			if err == nil {
				t.Fatalf("%s %s accepted", tc.method, tc.path)
			}
			continue
		}
		if err != nil || access != tc.access || id != tc.id {
			t.Fatalf("%s %s: %s %s %v", tc.method, tc.path, access, id, err)
		}
	}
	if _, _, err := GoogleSheetsOperation("GET", "/v4/spreadsheets/abc", []QueryPair{{Key: "key", Value: "x"}}, nil); err == nil {
		t.Fatal("unknown query parameter accepted")
	}
}
