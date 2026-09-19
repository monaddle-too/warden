package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Permission rules and history (docs/claude-parity.md, round 2 B): the
// workspace's and the chat's allow / deny / ask rules in Claude Code's
// own syntax (chats/rules.go), listed, added and removed with /rules;
// `A` on a tool ask (or /allow) remembers the call's rule for the whole
// workspace where `a` remembers it for the chat; /permissions lists how
// the chat's asks were decided and by whom.

// Rule mirrors chats.Rule.
type Rule struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"`
	Pattern string  `json:"pattern"`
	Origin  string  `json:"origin"`
	ChatID  string  `json:"chatID"`
	By      *Actor  `json:"by"`
	At      float64 `json:"at"`
}

// Actor mirrors conversation.Actor: who added a rule or answered a card.
type Actor struct {
	PrincipalID string `json:"principalID"`
	Email       string `json:"email"`
	Name        string `json:"name"`
}

// Label is the person's name, email or principal.
func (a *Actor) Label() string {
	switch {
	case a == nil:
		return ""
	case a.Name != "":
		return a.Name
	case a.Email != "":
		return a.Email
	case a.PrincipalID == "owner":
		return "the owner"
	}
	return a.PrincipalID
}

// RulesView mirrors chats.RulesView: the workspace's rules and its chats'.
type RulesView struct {
	Workspace string      `json:"workspace"`
	Rules     []Rule      `json:"rules"`
	Chats     []ChatRules `json:"chats"`
}
type ChatRules struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Rules []Rule `json:"rules"`
}

// PermissionEvent mirrors chats.PermissionEvent: one decision on a tool
// ask.
type PermissionEvent struct {
	ID       string  `json:"id"`
	At       float64 `json:"at"`
	Tool     string  `json:"tool"`
	Summary  string  `json:"summary"`
	Decision string  `json:"decision"`
	How      string  `json:"how"`
	Rule     *Rule   `json:"rule"`
	Scope    string  `json:"scope"`
	By       *Actor  `json:"by"`
	Message  string  `json:"message"`
}

// Rules lists a chat's workspace's rules and its chats' own.
func (c *Client) Rules(ctx context.Context, chatID string) (RulesView, error) {
	var out RulesView
	err := c.do(ctx, "GET", "chats/"+chatID+"/rules", nil, &out)
	return out, err
}

// AddRule adds a rule to a workspace (sandbox id) or a chat (chat id).
func (c *Client) AddRule(ctx context.Context, kind, id, ruleKind, pattern string) (Rule, error) {
	var out Rule
	err := c.do(ctx, "POST", kind+"/"+id+"/rules", map[string]any{"kind": ruleKind, "pattern": pattern}, &out)
	return out, err
}

// RemoveRule removes a rule from a workspace or a chat; kind is
// "environments" or "chats".
func (c *Client) RemoveRule(ctx context.Context, kind, id, ruleID string) error {
	return c.do(ctx, "POST", kind+"/"+id+"/rules/"+ruleID+"/remove", map[string]any{}, nil)
}

// Permissions is a chat's permission history, oldest first.
func (c *Client) Permissions(ctx context.Context, chatID string) ([]PermissionEvent, error) {
	var out struct {
		Events []PermissionEvent `json:"events"`
	}
	err := c.do(ctx, "GET", "chats/"+chatID+"/permissions", nil, &out)
	return out.Events, err
}

// ruleRef is one numbered line of the last /rules listing: where the rule
// lives (environments or chats), the owner's id and the rule's.
type ruleRef struct {
	kind, owner, id string
}

// RulesListing lays the rules out numbered, the workspace's first then
// each chat's, and returns the references the numbers stand for.
func RulesListing(view RulesView, current string) (string, []ruleRef) {
	var b strings.Builder
	var refs []ruleRef
	line := func(r Rule) {
		refs = append(refs, ruleRef{})
		fmt.Fprintf(&b, "%2d  %-5s %-36s %s\n", len(refs), r.Kind, truncate(sanitize(r.Pattern), 36), sanitize(ruleOrigin(r, view)))
	}
	b.WriteString("workspace rules (every chat of this workspace)\n")
	if len(view.Rules) == 0 {
		b.WriteString("    none\n")
	}
	for _, r := range view.Rules {
		line(r)
		refs[len(refs)-1] = ruleRef{"environments", view.Workspace, r.ID}
	}
	for _, c := range view.Chats {
		if len(c.Rules) == 0 {
			continue
		}
		title := sanitize(c.Title)
		if c.ID == current {
			title += " (this chat)"
		}
		b.WriteString("chat " + title + "\n")
		for _, r := range c.Rules {
			line(r)
			refs[len(refs)-1] = ruleRef{"chats", c.ID, r.ID}
		}
	}
	b.WriteString("/rules add allow|deny|ask PATTERN adds a workspace rule (Bash(git *), Edit(src/**), Read, WebFetch(domain:x)) · /rules rm N removes one\ndeny answers without asking in every mode, ask makes a card even in auto, allow answers in ask mode")
	return b.String(), refs
}

// ruleOrigin says where a rule came from: the editor or an "allow always"
// answer, in which chat, by whom.
func ruleOrigin(r Rule, view RulesView) string {
	out := "from the editor"
	if r.Origin == "always" {
		out = "allow always"
		if r.ChatID != "" {
			for _, c := range view.Chats {
				if c.ID == r.ChatID {
					out += " in " + c.Title
				}
			}
		}
	}
	if by := r.By.Label(); by != "" {
		out += " by " + by
	}
	return out
}

// PermissionLines lays the history out, newest last, as the /permissions
// notice.
func PermissionLines(events []PermissionEvent, loc *time.Location) []string {
	if len(events) == 0 {
		return []string{"no tool asks decided in this chat yet"}
	}
	var out []string
	for _, ev := range events {
		when := time.Unix(int64(ev.At), 0).In(loc).Format("Jan 2 15:04")
		out = append(out, fmt.Sprintf("%s  %-5s %-12s %s  — %s", when, ev.Decision, truncate(ev.Tool, 12), truncate(sanitize(ev.Summary), 50), sanitize(permissionBy(ev))))
	}
	return out
}

// permissionBy says who or what decided an event.
func permissionBy(ev PermissionEvent) string {
	switch ev.How {
	case "auto":
		return "auto mode"
	case "rule":
		if ev.Rule != nil {
			return ev.Scope + " rule " + ev.Rule.Kind + " " + ev.Rule.Pattern
		}
		return "a rule"
	}
	by := ev.By.Label()
	if by == "" {
		by = "a card"
	}
	if ev.Rule != nil {
		by += ", always for the " + ev.Scope + " (" + ev.Rule.Pattern + ")"
	}
	if ev.Message != "" {
		by += ": " + ev.Message
	}
	return by
}

// rules runs /rules: the listing, `add KIND PATTERN` (a workspace rule)
// or `rm N` (a number from the listing).
func (a *App) rules(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	verb, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(verb) {
	case "":
		view, err := a.Client.Rules(ctx, c.ID)
		if err != nil {
			a.setNotice(err.Error())
			return
		}
		text, refs := RulesListing(view, c.ID)
		a.ruleRefs = refs
		a.setNotice(text)
	case "add":
		kind, pattern, _ := strings.Cut(rest, " ")
		pattern = strings.Trim(strings.TrimSpace(pattern), "\"'")
		if kind == "" || pattern == "" {
			a.setNotice("/rules add allow|deny|ask PATTERN, such as /rules add deny \"Bash(rm *)\"")
			return
		}
		view, err := a.Client.Rules(ctx, c.ID)
		if err != nil {
			a.setNotice(err.Error())
			return
		}
		r, err := a.Client.AddRule(ctx, "environments", view.Workspace, kind, pattern)
		if err != nil {
			a.setNotice(err.Error())
			return
		}
		a.ruleRefs = nil
		a.setNotice("workspace rule added: " + r.Kind + " " + sanitize(r.Pattern))
	case "rm", "remove", "del":
		if a.ruleRefs == nil {
			view, err := a.Client.Rules(ctx, c.ID)
			if err != nil {
				a.setNotice(err.Error())
				return
			}
			_, a.ruleRefs = RulesListing(view, c.ID)
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 || n > len(a.ruleRefs) {
			a.setNotice("/rules rm N with N from /rules")
			return
		}
		ref := a.ruleRefs[n-1]
		if err := a.Client.RemoveRule(ctx, ref.kind, ref.owner, ref.id); err != nil {
			a.setNotice(err.Error())
			return
		}
		a.ruleRefs = nil
		a.setNotice(fmt.Sprintf("rule %d removed", n))
	default:
		a.setNotice("/rules · /rules add allow|deny|ask PATTERN · /rules rm N")
	}
}

// permissions runs /permissions: the chat's decided asks.
func (a *App) permissions(ctx context.Context, c *Chat) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	events, err := a.Client.Permissions(ctx, c.ID)
	if err != nil {
		a.setNotice(err.Error())
		return
	}
	a.setNotice(strings.Join(PermissionLines(events, a.now().Location()), "\n"))
}

// allowAlways runs /allow [chat]: the first pending tool ask allowed
// always for the workspace (or the chat).
func (a *App) allowAlways(ctx context.Context, c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	scope := "workspace"
	if strings.EqualFold(arg, "chat") {
		scope = "chat"
	}
	for _, ap := range c.Pending() {
		if p := ap.Permission(); p != nil && !p.IsPlan() {
			a.answerAlways(ctx, c.ID, ap, p, scope)
			return
		}
	}
	a.setNotice("no tool ask pending")
}

// answerAlways allows an ask and remembers its rule at the scope.
func (a *App) answerAlways(ctx context.Context, chatID string, ap Approval, p *Permission, scope string) {
	if err := a.Client.Answer(ctx, chatID, ap.ID, true, true, scope, "", ""); err != nil {
		a.setNotice(err.Error())
		return
	}
	a.setNotice("allowed always for this " + scope + ": " + sanitize(p.Always))
}
