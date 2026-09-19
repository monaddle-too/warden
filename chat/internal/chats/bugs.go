package chats

import (
	"errors"
	"log"
	"strings"
	"warden/chat/internal/bugreport"
	cv "warden/chat/internal/conversation"
)

// Bug reporting from a chat (docs/bug-reporting-plan.md): "/bug text"
// drafts a user report with the chat's ids and the chat service's log
// tail, "/test bugreporting" raises a deliberate panic that the service's
// own recovery drafts. Both leave a pending draft the launcher presents;
// neither carries any message text.

// BugResult is what the two routes answer.
type BugResult struct {
	Drafted bool   `json:"drafted"`
	ID      string `json:"id,omitempty"`
	Notice  string `json:"notice"`
}

// MaxBugText bounds a /bug description.
const MaxBugText = bugreport.MaxDescription

// Bug drafts a user report from chat id: the person's text as the
// description, the chat's ids (never its content) as the context.
func (e *Engine) Bug(id, text string, actor cv.Actor) (BugResult, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > MaxBugText || strings.ContainsRune(text, 0) {
		return BugResult{}, errors.New("a description of at most 16384 characters is required")
	}
	c := e.Store.Chat(id)
	if c == nil {
		return BugResult{}, errors.New("chat not found")
	}
	if !e.Bugs.Enabled() {
		return BugResult{Notice: bugreport.OffNotice}, nil
	}
	r := e.Bugs.Draft(bugreport.KindUser, bugreport.TriggerUser, text)
	r.Description = text
	model := c.Model
	if c.Session != nil && c.Session.Model != "" {
		model = c.Session.Model
	}
	r.Context = &bugreport.Context{ChatID: c.ID, RunID: c.RunID, Provider: c.Provider, Model: model}
	e.Bugs.AddServiceLog(&r, "warden-chat")
	e.Bugs.AddServiceLog(&r, "warden")
	_, written, err := e.Bugs.Capture(r)
	if err != nil {
		return BugResult{}, err
	}
	if !written {
		return BugResult{Notice: bugreport.OffNotice}, nil
	}
	log.Printf("chat %s: %s drafted bug report %s", id, actorLabel(actor), r.ID)
	return BugResult{Drafted: true, ID: r.ID, Notice: bugreport.DraftedNotice}, nil
}

// TestException is the message of the deliberate panic.
const TestException = "test exception from /test bugreporting"

// BugTest raises the test exception on a goroutine of this service and
// recovers it the way a real panic is recovered, so the draft comes from
// the same path; it waits for the draft so the answer can name it.
func (e *Engine) BugTest() (BugResult, error) {
	if !e.Bugs.Enabled() {
		return BugResult{Notice: bugreport.OffNotice}, nil
	}
	before, _ := bugreport.Pending(e.Bugs.State)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer e.Bugs.Trap(bugreport.TriggerTest, "POST /api/bug-test")
		panic(TestException)
	}()
	<-done
	after, err := bugreport.Pending(e.Bugs.State)
	if err != nil {
		return BugResult{}, err
	}
	known := map[string]bool{}
	for _, d := range before {
		known[d.Report.ID] = true
	}
	for _, d := range after {
		if !known[d.Report.ID] && d.Report.Trigger == bugreport.TriggerTest {
			return BugResult{Drafted: true, ID: d.Report.ID, Notice: bugreport.DraftedNotice}, nil
		}
	}
	return BugResult{}, errors.New("the test exception was raised but no draft was written")
}
