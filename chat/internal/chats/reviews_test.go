package chats

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"

	"warden/chat/internal/agent"
)

// pipeAgent is an agent.Client over a pipe whose far end answers the
// handshake and keeps every reply the engine sends the "agent".
func pipeAgent(t *testing.T) (*agent.Client, func() []agent.Frame) {
	t.Helper()
	near, far := net.Pipe()
	var mu sync.Mutex
	var replies []agent.Frame
	go func() {
		scan := bufio.NewScanner(far)
		for scan.Scan() {
			var f agent.Frame
			if json.Unmarshal(scan.Bytes(), &f) != nil {
				return
			}
			if f.Method == "initialize" {
				_ = json.NewEncoder(far).Encode(map[string]any{"id": f.ID, "result": map[string]any{}})
				continue
			}
			if f.Method == "" && len(f.ID) > 0 {
				mu.Lock()
				replies = append(replies, f)
				mu.Unlock()
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	client, err := agent.StartStream(ctx, near, func(*agent.Client, agent.Frame) {})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); far.Close() })
	return client, func() []agent.Frame {
		mu.Lock()
		defer mu.Unlock()
		return append([]agent.Frame(nil), replies...)
	}
}

const reviewID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// A pull request proposal is recorded on the chat while the policy
// service holds it pending, and dropped when the poll sees it settled;
// the tool's reply carries the outcome as before.
func TestPullRequestReviewRecordedUntilSettled(t *testing.T) {
	e, _, c := portEngine(t, "")
	sharing, socket := newFakeSharing(t)
	e.PolicyAddress = "unix://" + socket
	pending := map[string]any{"request_id": reviewID, "kind": "pull_request", "chatID": c.ID, "sandboxID": c.SandboxID, "status": "pending", "title": "Fix the README", "repository": "owner/repo"}
	sharing.results["pr_submit"] = pending
	sharing.results["pr_get"] = pending
	client, replies := pipeAgent(t)
	args := map[string]any{"repository": "owner/repo", "base": "main", "title": "Fix the README", "body": "typo", "files": []any{map[string]any{"path": "README.md", "content": "Warden\n"}}}
	if err := e.sharingTool(context.Background(), c, client, agent.Frame{ID: json.RawMessage(`7`), Params: map[string]any{"tool": "request_pull_request", "arguments": args}}); err != nil {
		t.Fatal(err)
	}
	reviews := e.Store.Snapshot().chat(c.ID).Reviews
	if len(reviews) != 1 || reviews[0].ID != reviewID || reviews[0].Kind != "pull_request" || reviews[0].Title != "Fix the README" || reviews[0].Repository != "owner/repo" || reviews[0].Status != "pending" || reviews[0].RequestedAt == 0 {
		t.Fatalf("review not recorded: %+v", reviews)
	}
	if len(replies()) != 0 {
		t.Fatal("replied while pending")
	}
	// The state clients stream carries it.
	if b, _ := json.Marshal(e.View()); !jsonContains(b, `"reviews":[{"id":"`+reviewID+`"`) {
		t.Fatalf("view lacks the review: %s", b)
	}
	sharing.mu.Lock()
	sharing.results["pr_get"] = map[string]any{"request_id": reviewID, "kind": "pull_request", "chatID": c.ID, "sandboxID": c.SandboxID, "status": "rejected", "title": "Fix the README", "repository": "owner/repo", "feedback": "not now"}
	sharing.mu.Unlock()
	until(t, func() bool { return len(e.Store.Snapshot().chat(c.ID).Reviews) == 0 && len(replies()) == 1 })
	reply := replies()[0]
	if string(reply.ID) != "7" || !jsonContains(reply.Result, `\"status\":\"rejected\"`) || !jsonContains(reply.Result, `not now`) {
		t.Fatalf("reply: %s", reply.Result)
	}
}

// A document edit stays listed while stale or applying (the app still has
// it open), a document request while pending; the kinds carry what the
// clients word their cards with.
func TestReviewOfKinds(t *testing.T) {
	r, open := reviewOf("doc_submit", map[string]any{"request_id": "d1", "status": "stale", "title": "Roadmap", "summary": "Tighten the intro", "changes": float64(3), "created_at": 1700000000.5})
	if !open || r.Kind != "document_edit" || r.Document != "Roadmap" || r.Title != "Tighten the intro" || r.Changes != 3 || r.RequestedAt != 1700000000.5 {
		t.Fatalf("doc edit: %+v %v", r, open)
	}
	if _, open = reviewOf("doc_get", map[string]any{"request_id": "d1", "status": "applying"}); !open {
		t.Fatal("applying is still the app's")
	}
	if _, open = reviewOf("doc_get", map[string]any{"request_id": "d1", "status": "applied"}); open {
		t.Fatal("applied is settled")
	}
	r, open = reviewOf("request", map[string]any{"request_id": "a1", "status": "pending", "reason": "read the brief", "access": "read"})
	if !open || r.Kind != "document_access" || r.Title != "read the brief" {
		t.Fatalf("access: %+v %v", r, open)
	}
	r, open = reviewOf("get", map[string]any{"request_id": "c1", "status": "pending", "reason": "a report", "access": "create", "title": "Q3 report"})
	if !open || r.Kind != "document_create" || r.Document != "Q3 report" || r.Title != "a report" {
		t.Fatalf("create: %+v %v", r, open)
	}
	if _, open = reviewOf("get", map[string]any{"request_id": "c1", "status": "creating", "access": "create"}); open {
		t.Fatal("creating is past the person")
	}
	if _, open = reviewOf("pr_get", map[string]any{"request_id": "p1", "status": "publishing"}); open {
		t.Fatal("publishing is past the person")
	}
	if _, open = reviewOf("pr_get", map[string]any{"status": "pending"}); open {
		t.Fatal("no id, no review")
	}
}

// After a restart the runs' polls are gone: the delivery loop's first
// act lists what the policy service still holds and the chats' records
// follow it — a pending review reappears, one settled meanwhile goes, and
// a chat's own request on another workspace is not its review.
func TestReviewsReconciledWithPolicyService(t *testing.T) {
	e, _, c := portEngine(t, "")
	sharing, socket := newFakeSharing(t)
	e.PolicyAddress = "unix://" + socket
	other, err := e.Create("Other", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// What the chats remember: a pull request the service settled while
	// Warden was down, and nothing of the two requests it still holds.
	_ = e.Store.update(func(st *State) error {
		st.chat(c.ID).Reviews = []Review{{ID: "gone", Kind: "pull_request", Status: "pending", Title: "Old", RequestedAt: 1}}
		return nil
	})
	sharing.results["pr_state"] = map[string]any{"requests": []any{
		map[string]any{"request_id": "gone", "kind": "pull_request", "chatID": c.ID, "sandboxID": c.SandboxID, "status": "rejected"},
		map[string]any{"request_id": reviewID, "kind": "pull_request", "chatID": c.ID, "sandboxID": c.SandboxID, "status": "pending", "title": "Fix the README", "repository": "owner/repo"},
		map[string]any{"request_id": "elsewhere", "kind": "pull_request", "chatID": other, "sandboxID": "not-its-workspace", "status": "pending", "title": "Stray"},
	}}
	sharing.results["doc_state"] = map[string]any{"requests": []any{
		map[string]any{"request_id": "d1", "kind": "document_proposal", "chatID": other, "sandboxID": e.Store.Snapshot().chat(other).SandboxID, "status": "pending", "title": "Roadmap", "summary": "Tighten", "changes": float64(2), "created_at": 1700000000.0},
	}}
	sharing.results["state"] = map[string]any{"requests": []any{
		map[string]any{"request_id": "a1", "chatID": c.ID, "sandboxID": c.SandboxID, "status": "granted", "reason": "done"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.sharingDelivery(ctx) }()
	defer func() { cancel(); <-done }()
	until(t, func() bool {
		st := e.Store.Snapshot()
		mine, theirs := st.chat(c.ID).Reviews, st.chat(other).Reviews
		return len(mine) == 1 && mine[0].ID == reviewID && mine[0].RequestedAt > 0 && len(theirs) == 1 && theirs[0].ID == "d1" && theirs[0].Kind == "document_edit" && theirs[0].Changes == 2
	})
	// A review just recorded by a submission survives a listing taken
	// before it (the grace period), one older than that does not.
	_ = e.Store.update(func(st *State) error {
		st.chat(c.ID).Reviews = append(st.chat(c.ID).Reviews, Review{ID: "fresh", Kind: "pull_request", Status: "pending", RequestedAt: e.at()}, Review{ID: "stale", Kind: "pull_request", Status: "pending", RequestedAt: e.at() - 60})
		return nil
	})
	e.reconcileReviews(ctx)
	ids := ""
	for _, r := range e.Store.Snapshot().chat(c.ID).Reviews {
		ids += r.ID + " "
	}
	if ids != reviewID+" fresh " {
		t.Fatalf("after reconcile: %q", ids)
	}
	// A settled result the delivery loop sees drops the review at once.
	sharing.mu.Lock()
	sharing.results["undelivered"] = map[string]any{"requests": []any{map[string]any{"request_id": reviewID, "kind": "pull_request", "chatID": c.ID, "sandboxID": c.SandboxID, "status": "rejected", "feedback": "no"}}}
	sharing.mu.Unlock()
	until(t, func() bool {
		for _, r := range e.Store.Snapshot().chat(c.ID).Reviews {
			if r.ID == reviewID {
				return false
			}
		}
		return true
	})
}

func jsonContains(b []byte, s string) bool { return strings.Contains(string(b), s) }
