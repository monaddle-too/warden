package chats

import (
	"context"
	"warden/chat/internal/agent"
)

// Review is one request of the agent's that only the app can settle: a
// pull request proposal (request_pull_request), suggested document edits
// (propose_google_document_edit), a document selection or a document to
// create (request_google_docs_access, request_google_document_creation).
// It waits in the policy service, which the app's review pages read
// directly (sharing/pr_state, doc_state, state); the chat keeps a record
// of it while it waits so the terminal client can show it and the
// launcher can open the app on it. Nothing here answers a review.
type Review struct {
	// ID is the policy service's request_id.
	ID string `json:"id"`
	// Kind is pull_request, document_edit, document_access or
	// document_create.
	Kind string `json:"kind"`
	// Status is pending; a document edit is also listed while stale (the
	// document changed under it and the page rebases it) or applying (the
	// approved draft is being written).
	Status string `json:"status"`
	// Title is the one line of what is proposed: the pull request's
	// title, the edit's summary, the reason for the access or creation.
	Title string `json:"title,omitempty"`
	// Repository is the pull request's repository.
	Repository string `json:"repository,omitempty"`
	// Document is the edited document's title, or the title to create.
	Document string `json:"document,omitempty"`
	// Changes is the number of suggested changes of a document edit.
	Changes     int     `json:"changes,omitempty"`
	RequestedAt float64 `json:"requestedAt"`
}

// reviewOf reads the policy service's answer to a submission, a poll or a
// listing (op names which: pr_*, doc_*, or the document request ops) as a
// review, and says whether it is still open, that is, waiting for the
// person in the app. The web's request rows use the same rule.
func reviewOf(op string, result map[string]any) (Review, bool) {
	r := Review{ID: agent.String(result["request_id"]), Status: agent.String(result["status"])}
	if t, ok := result["created_at"].(float64); ok {
		r.RequestedAt = t
	}
	open := false
	switch {
	case op == "pr_submit" || op == "pr_get" || op == "pr_state":
		r.Kind = "pull_request"
		r.Title = agent.String(result["title"])
		r.Repository = agent.String(result["repository"])
		open = r.Status == "pending"
	case op == "doc_submit" || op == "doc_get" || op == "doc_state":
		r.Kind = "document_edit"
		r.Title = agent.String(result["summary"])
		r.Document = agent.String(result["title"])
		if n, ok := result["changes"].(float64); ok {
			r.Changes = int(n)
		}
		open = r.Status == "pending" || r.Status == "stale" || r.Status == "applying"
	default:
		r.Kind = "document_access"
		r.Title = agent.String(result["reason"])
		if agent.String(result["access"]) == "create" {
			r.Kind = "document_create"
			r.Document = agent.String(result["title"])
		}
		open = r.Status == "pending"
	}
	return r, open && r.ID != ""
}

// recordReview keeps the chat's record of a request in step with the
// policy service's answer to its submission or a poll of it.
func (e *Engine) recordReview(chatID, op string, result map[string]any) {
	r, open := reviewOf(op, result)
	e.setReview(chatID, r, open)
}

// setReview records a review on the chat while it is open and drops it
// once it is not; the requested time of a review already recorded stands.
func (e *Engine) setReview(chatID string, r Review, open bool) {
	if r.ID == "" {
		return
	}
	_ = e.Store.update(func(st *State) error {
		c := st.chat(chatID)
		if c == nil {
			return nil
		}
		kept := c.Reviews[:0:0]
		for _, old := range c.Reviews {
			if old.ID != r.ID {
				kept = append(kept, old)
				continue
			}
			if r.RequestedAt == 0 {
				r.RequestedAt = old.RequestedAt
			}
		}
		if open {
			if r.RequestedAt == 0 {
				r.RequestedAt = e.at()
			}
			kept = append(kept, r)
		}
		if len(kept) == 0 {
			kept = nil
		}
		c.Reviews = kept
		return nil
	})
}

// dropReview removes a review from whichever chat holds it (a resolved
// request the delivery loop sees names its chat, but a chat's record is
// the one to trust).
func (e *Engine) dropReview(id string) {
	if id == "" {
		return
	}
	_ = e.Store.update(func(st *State) error {
		for _, c := range st.Chats {
			for i, r := range c.Reviews {
				if r.ID == id {
					c.Reviews = append(c.Reviews[:i:i], c.Reviews[i+1:]...)
					if len(c.Reviews) == 0 {
						c.Reviews = nil
					}
					break
				}
			}
		}
		return nil
	})
}

// reconcileGrace is how long (seconds) a review recorded by a submission
// is kept even when a listing does not show it: a listing taken just
// before the submission must not undo it. reconcileEvery is how many
// ticks of the delivery loop (2 s each) pass between reconciliations.
const (
	reconcileGrace = 10
	reconcileEvery = 15
)

// reconcileReviews makes every chat's reviews match what waits in the
// policy service: a review still pending after a restart reappears (the
// run's poll ended with the process), one settled while the service was
// down goes. Called when the delivery loop starts and now and then after.
// A listing that fails leaves the chats as they are.
func (e *Engine) reconcileReviews(ctx context.Context) {
	type owner struct{ chat, sandbox string }
	open := map[owner][]Review{}
	for _, op := range []string{"pr_state", "doc_state", "state"} {
		result, err := e.sharingCall(ctx, op, map[string]any{})
		if err != nil {
			return
		}
		for _, value := range agent.Array(result["requests"]) {
			row := agent.Map(value)
			r, isOpen := reviewOf(op, row)
			if !isOpen {
				continue
			}
			key := owner{agent.String(row["chatID"]), agent.String(row["sandboxID"])}
			open[key] = append(open[key], r)
		}
	}
	now := e.at()
	_ = e.Store.update(func(st *State) error {
		for _, c := range st.Chats {
			// A request belongs to the chat on the workspace it was made
			// from, as the delivery loop attributes results.
			waiting := open[owner{c.ID, c.SandboxID}]
			var next []Review
			seen := map[string]bool{}
			for _, old := range c.Reviews {
				found := false
				for _, r := range waiting {
					if r.ID != old.ID {
						continue
					}
					found = true
					if r.RequestedAt == 0 {
						r.RequestedAt = old.RequestedAt
					}
					next = append(next, r)
				}
				if !found && now-old.RequestedAt < reconcileGrace {
					next = append(next, old)
				}
				seen[old.ID] = true
			}
			for _, r := range waiting {
				if seen[r.ID] {
					continue
				}
				if r.RequestedAt == 0 {
					r.RequestedAt = now
				}
				next = append(next, r)
			}
			c.Reviews = next
		}
		return nil
	})
}
