package chats

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

// Permission rules (docs/claude-parity.md, round 2 B) answer a Claude
// chat's tool asks before its mode does. A rule is a kind and a pattern
// in Claude Code's own syntax, so what a person writes here reads like
// their settings.json:
//
//	Bash(git *)            commands starting with `git ` (and `git` alone)
//	Bash(npm run test:*)   the CLI's prefix form, the same
//	Bash(npm test)         exactly that command
//	Edit(src/**)           edits under src/ (any of the file tools)
//	Read                   every read (Read, Glob, Grep, LS)
//	WebFetch(domain:example.com)
//	mcp__warden__*         every tool of the warden MCP server
//
// Kinds: deny answers without asking in every mode (the model reads the
// rule as the tool's error), ask forces a card even in auto, allow answers
// in ask and plan mode. Chat rules ("Allow always" in this chat) and
// workspace rules (every chat of the environment, present and future)
// are consulted together: a deny wins over an ask wins over an allow.
//
// The CLI raises asks for what its own mode would ask about (file edits,
// commands that write or are not read-only); a rule on what it never asks
// about (Read, `ls`, `git status`) does not fire, since rules stay
// Warden's and are not pushed into the CLI's settings.
const (
	RuleAllow = "allow"
	RuleDeny  = "deny"
	RuleAsk   = "ask"
)

// RuleKinds lists the kinds in the order the editors offer them.
var RuleKinds = []string{RuleAllow, RuleDeny, RuleAsk}

// Rule is one permission rule on a chat or a workspace. Origin says where
// it came from: "editor" for one typed into the rules editor, "always" for
// an "Allow always" answer to a card (ChatID is that chat when the rule is
// the workspace's). By is who added it, At when.
type Rule struct {
	ID      string    `json:"id,omitempty"`
	Kind    string    `json:"kind"`
	Pattern string    `json:"pattern"`
	Origin  string    `json:"origin,omitempty"`
	ChatID  string    `json:"chatID,omitempty"`
	By      *cv.Actor `json:"by,omitempty"`
	At      float64   `json:"at,omitempty"`
}

// UnmarshalJSON reads a rule as stored today or as item 3 stored an
// allow-always answer ({"tool":…,"command":…}), converting the latter to
// its pattern so a chat's rules survive the upgrade.
func (r *Rule) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID      string    `json:"id"`
		Kind    string    `json:"kind"`
		Pattern string    `json:"pattern"`
		Origin  string    `json:"origin"`
		ChatID  string    `json:"chatID"`
		By      *cv.Actor `json:"by"`
		At      float64   `json:"at"`
		Tool    string    `json:"tool"`
		Command string    `json:"command"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*r = Rule{ID: raw.ID, Kind: raw.Kind, Pattern: raw.Pattern, Origin: raw.Origin, ChatID: raw.ChatID, By: raw.By, At: raw.At}
	if r.Pattern == "" && raw.Tool != "" {
		r.Kind, r.Pattern, r.Origin = RuleAllow, legacyPattern(raw.Tool, raw.Command), "always"
	}
	if r.Kind == "" {
		r.Kind = RuleAllow
	}
	return nil
}

// legacyPattern is the pattern for an item-3 rule: a Bash program prefix
// becomes `Bash(prefix *)`, a whole chained command `Bash(the command)`,
// "edit" the CLI's `Edit` (which covers every file tool), any other tool
// its name.
func legacyPattern(tool, command string) string {
	switch {
	case tool == "edit":
		return "Edit"
	case tool != "Bash" || command == "":
		return tool
	}
	for _, c := range shellControl {
		if strings.Contains(command, c) {
			return "Bash(" + command + ")"
		}
	}
	return "Bash(" + command + " *)"
}

// ValidRuleKind reports whether kind is allow, deny or ask.
func ValidRuleKind(kind string) bool {
	for _, k := range RuleKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// fileTools are the CLI's tools that write files; a rule on Edit covers
// them all, as the CLI's own Edit rules do.
var fileTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// readTools are the CLI's tools that read files; a rule on Read covers
// them all (the CLI applies Read rules to them best-effort).
var readTools = map[string]bool{"Read": true, "Glob": true, "Grep": true, "LS": true}

// NewRule validates a kind and a pattern and returns the rule, without an
// id or origin (the caller stamps those).
func NewRule(kind, pattern string) (Rule, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !ValidRuleKind(kind) {
		return Rule{}, fmt.Errorf("rule kind %q: allow, deny or ask", kind)
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return Rule{}, errors.New("a rule needs a pattern, such as Bash(git *) or Edit(src/**)")
	}
	if len(pattern) > 512 || strings.ContainsAny(pattern, "\n\r\x00") {
		return Rule{}, errors.New("rule pattern too long or not one line")
	}
	tool, spec, err := splitPattern(pattern)
	if err != nil {
		return Rule{}, err
	}
	if spec != "" {
		switch {
		case tool == "Bash", fileTools[tool], readTools[tool]:
		case tool == "WebFetch":
			if !strings.HasPrefix(spec, "domain:") || len(spec) == len("domain:") {
				return Rule{}, errors.New("WebFetch rules take domain:HOST, such as WebFetch(domain:example.com)")
			}
		default:
			return Rule{}, fmt.Errorf("%s rules take no argument; write %s alone", tool, tool)
		}
	}
	return Rule{Kind: kind, Pattern: pattern}, nil
}

// splitPattern separates `Tool(spec)` into its parts; a bare tool has an
// empty spec. The tool is a name with `*` allowed (mcp__warden__*).
func splitPattern(pattern string) (tool, spec string, err error) {
	tool = pattern
	if i := strings.IndexByte(pattern, '('); i >= 0 {
		if !strings.HasSuffix(pattern, ")") {
			return "", "", errors.New("rule pattern: missing the closing parenthesis")
		}
		tool, spec = pattern[:i], strings.TrimSpace(pattern[i+1:len(pattern)-1])
		if spec == "" {
			return "", "", fmt.Errorf("rule pattern: empty parentheses; write %s alone", tool)
		}
	}
	if tool == "" {
		return "", "", errors.New("rule pattern: missing the tool name")
	}
	for _, c := range tool {
		if !(c == '_' || c == '*' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return "", "", fmt.Errorf("rule pattern: %q is not a tool name", tool)
		}
	}
	return tool, spec, nil
}

// Matches reports whether the rule covers a call of tool with input. For
// a deny or ask rule any part of a chained command counts; for an allow
// rule every part must be covered (and a substitution never is), so
// `Bash(git *)` does not allow `git status && rm -rf /`.
func (r Rule) Matches(tool string, input map[string]any) bool {
	name, spec, err := splitPattern(r.Pattern)
	if err != nil || !toolMatches(name, tool) {
		return false
	}
	if spec == "" {
		return true
	}
	switch {
	case tool == "Bash":
		return commandMatches(spec, agent.String(input["command"]), r.Kind == RuleAllow)
	case fileTools[tool] || readTools[tool]:
		return pathMatches(spec, inputPath(tool, input))
	case tool == "WebFetch":
		return domainMatches(strings.TrimPrefix(spec, "domain:"), agent.String(input["url"]))
	}
	return false
}

// toolMatches is the rule's tool name against the call's: a glob on the
// name, with Edit covering every file tool and Read every read tool.
func toolMatches(name, tool string) bool {
	if name == tool {
		return true
	}
	if name == "Edit" && fileTools[tool] || name == "Read" && readTools[tool] {
		return true
	}
	return strings.Contains(name, "*") && glob(name, tool, false)
}

// commandMatches is a Bash spec against a command. The spec is exact, a
// prefix (`git *`, or the CLI's `git:*`), or a glob with `*` anywhere. For
// an allow rule (every) each chained part must match; otherwise any part
// or the whole command matching is enough.
func commandMatches(spec, command string, every bool) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	if command == spec {
		return true
	}
	parts := commandParts(command)
	if every {
		for _, p := range parts {
			if strings.Contains(p, "$(") || strings.Contains(p, "`") || !commandGlob(spec, p) {
				return false
			}
		}
		return len(parts) > 0
	}
	if commandGlob(spec, command) {
		return true
	}
	for _, p := range parts {
		if commandGlob(spec, p) {
			return true
		}
	}
	return false
}

// commandGlob matches one command against the spec: the CLI's `x:*` and
// the plain `x *` both mean x alone or x followed by more; any other `*`
// is a glob over any characters.
func commandGlob(spec, command string) bool {
	if prefix, ok := strings.CutSuffix(spec, ":*"); ok {
		return command == prefix || strings.HasPrefix(command, prefix)
	}
	if prefix, ok := strings.CutSuffix(spec, " *"); ok && !strings.Contains(prefix, "*") {
		return command == prefix || strings.HasPrefix(command, prefix+" ")
	}
	return glob(spec, command, false)
}

// commandParts splits a command at its chaining operators (&&, ||, ;, |,
// &, a newline; not the `>&` of a redirection), each part trimmed of
// leading assignments so `FOO=1 make` matches `make *`. Quotes are not
// parsed: a `;` inside one splits too, which only makes an allow harder
// and a deny easier to match.
func commandParts(command string) []string {
	command = strings.NewReplacer(">&", ">\x00", "<&", "<\x00").Replace(command)
	fields := strings.FieldsFunc(command, func(c rune) bool { return c == '\n' || c == ';' || c == '|' || c == '&' })
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(strings.ReplaceAll(f, "\x00", "&"))
		if f == "" {
			continue
		}
		words := strings.Fields(f)
		for len(words) > 1 && strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "=") {
			words = words[1:]
		}
		out = append(out, strings.Join(words, " "))
	}
	return out
}

// inputPath is the path a file tool call names.
func inputPath(tool string, input map[string]any) string {
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if p := agent.String(input[k]); p != "" {
			return p
		}
	}
	if readTools[tool] {
		return "."
	}
	return ""
}

// pathMatches is a path spec against the call's path: `**` spans
// directories, `*` and `?` stay within one. A spec starting with `/` (or
// the CLI's `//`) is the whole path, `~/` is under the guest's home; any
// other is matched against the path and against every tail of it
// (`src/**` covers /home/agent/workspace/src/a.go), since the engine does
// not know the guest's working directory.
func pathMatches(spec, p string) bool {
	if p == "" {
		return false
	}
	p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if strings.HasPrefix(spec, "//") {
		spec = spec[1:] // the CLI's absolute form
	}
	if strings.HasPrefix(spec, "/") {
		return glob(spec, p, true)
	}
	if rest, ok := strings.CutPrefix(spec, "~/"); ok {
		// The guest's home: /home/<user> or /root.
		return glob("/home/*/"+rest, p, true) || glob("/root/"+rest, p, true)
	}
	if glob(spec, p, true) {
		return true
	}
	rest := p
	for {
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
		if glob(spec, rest, true) {
			return true
		}
	}
}

// domainMatches is a WebFetch domain spec against the fetched URL's host:
// the host itself or a subdomain of it.
func domainMatches(domain, raw string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || domain == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// glob matches s against a pattern where `*` spans any characters (any
// but `/` when paths is set, where `**` spans directories) and `?` one.
func glob(pattern, s string, paths bool) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			if paths && strings.HasPrefix(pattern, "**") {
				rest := strings.TrimLeft(pattern, "*")
				rest = strings.TrimPrefix(rest, "/")
				if rest == "" {
					return true
				}
				for i := 0; i <= len(s); i++ {
					if (i == 0 || s[i-1] == '/') && glob(rest, s[i:], paths) {
						return true
					}
				}
				return false
			}
			rest := pattern[1:]
			for i := 0; i <= len(s); i++ {
				if glob(rest, s[i:], paths) {
					return true
				}
				if i < len(s) && paths && s[i] == '/' {
					return false
				}
			}
			return false
		case '?':
			if len(s) == 0 || paths && s[0] == '/' {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		default:
			if len(s) == 0 || s[0] != pattern[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return len(s) == 0
}

// Decision is what the rules say about a call: the rule that decides it
// and the scope it lives in ("chat" or "workspace"), or nil when none
// matches. A deny wins over an ask, an ask over an allow, whichever scope
// they are in; within a kind the chat's rules come first, in order.
type Decision struct {
	Rule  Rule
	Scope string
}

// decide finds the rule that decides a call among the chat's rules and
// its workspace's.
func (st *State) decideByRule(c *Chat, tool string, input map[string]any) *Decision {
	var best *Decision
	rank := map[string]int{RuleDeny: 3, RuleAsk: 2, RuleAllow: 1}
	consider := func(rules []Rule, scope string) {
		for _, r := range rules {
			if !r.Matches(tool, input) {
				continue
			}
			if best == nil || rank[r.Kind] > rank[best.Rule.Kind] {
				best = &Decision{Rule: r, Scope: scope}
			}
		}
	}
	consider(c.Rules, "chat")
	if env := st.Environments[c.SandboxID]; env != nil {
		consider(env.Rules, "workspace")
	}
	return best
}

// EnvironmentRecord is what the store keeps per workspace beyond its
// chats: its permission rules (rules.go). Keyed by sandbox id in
// State.Environments.
type EnvironmentRecord struct {
	Rules []Rule `json:"rules,omitempty"`
}

// stamp fills a new rule's id, origin, author and time.
func (r Rule) stamp(origin string, by cv.Actor, at float64) Rule {
	r.ID, r.Origin, r.At = cv.ID(), origin, at
	if by.PrincipalID != "" {
		actor := by
		r.By = &actor
	}
	return r
}

// addRule appends a rule to a list unless an equal one (kind and pattern)
// is there already; it reports whether it was added.
func addRule(rules []Rule, r Rule) ([]Rule, bool) {
	for _, have := range rules {
		if have.Kind == r.Kind && have.Pattern == r.Pattern {
			return rules, false
		}
	}
	return append(rules, r), true
}

// removeRule drops the rule with the id; it reports whether one was.
func removeRule(rules []Rule, id string) ([]Rule, bool) {
	for i, r := range rules {
		if r.ID == id && id != "" {
			return append(rules[:i:i], rules[i+1:]...), true
		}
	}
	return rules, false
}

// RulesView is what environments/{id}/rules and chats/{id}/rules answer:
// the workspace's rules and, per chat of it, that chat's own.
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

// Rules lists a workspace's rules and its chats'. The id may be a
// workspace (sandbox) id or a chat id.
func (e *Engine) Rules(id string) (RulesView, error) {
	st := e.Store.Snapshot()
	sandboxID := id
	if c := st.chat(id); c != nil {
		sandboxID = c.SandboxID
	}
	chats := st.environmentChats(sandboxID)
	if len(chats) == 0 {
		return RulesView{}, errors.New("workspace not found")
	}
	view := RulesView{Workspace: sandboxID, Rules: []Rule{}, Chats: []ChatRules{}}
	if env := st.Environments[sandboxID]; env != nil && len(env.Rules) > 0 {
		view.Rules = env.Rules
	}
	for _, c := range chats {
		rules := c.Rules
		if rules == nil {
			rules = []Rule{}
		}
		view.Chats = append(view.Chats, ChatRules{ID: c.ID, Title: c.Title, Rules: rules})
	}
	return view, nil
}

// AddRule adds a rule typed into the editor to a workspace (id a sandbox
// id) or a chat (id a chat id), by actor.
func (e *Engine) AddRule(id, kind, pattern string, actor cv.Actor) (Rule, error) {
	rule, err := NewRule(kind, pattern)
	if err != nil {
		return Rule{}, err
	}
	rule = rule.stamp("editor", actor, e.at())
	err = e.Store.update(func(st *State) error {
		if c := st.chat(id); c != nil {
			if c.Provider != "claude" {
				return errors.New("permission rules apply to Claude chats")
			}
			var added bool
			if c.Rules, added = addRule(c.Rules, rule); !added {
				return errors.New("that rule is already there")
			}
			return nil
		}
		if len(st.environmentChats(id)) == 0 {
			return errors.New("workspace not found")
		}
		if st.Environments == nil {
			st.Environments = map[string]*EnvironmentRecord{}
		}
		env := st.Environments[id]
		if env == nil {
			env = &EnvironmentRecord{}
			st.Environments[id] = env
		}
		var added bool
		if env.Rules, added = addRule(env.Rules, rule); !added {
			return errors.New("that rule is already there")
		}
		return nil
	})
	return rule, err
}

// RemoveRule removes a rule by id from a workspace or a chat.
func (e *Engine) RemoveRule(id, ruleID string) error {
	return e.Store.update(func(st *State) error {
		var removed bool
		if c := st.chat(id); c != nil {
			c.Rules, removed = removeRule(c.Rules, ruleID)
		} else if env := st.Environments[id]; env != nil {
			env.Rules, removed = removeRule(env.Rules, ruleID)
			if len(env.Rules) == 0 {
				delete(st.Environments, id)
			}
		} else if len(st.environmentChats(id)) == 0 {
			return errors.New("workspace not found")
		}
		if !removed {
			return errors.New("no such rule")
		}
		return nil
	})
}

// PermissionEvent is one decision on a tool ask, kept on the chat
// (permission history, the last historyCap): what was asked, how it was
// decided and by whom — "auto" for the mode, "rule" with the rule and its
// scope, or the person who answered the card (By).
type PermissionEvent struct {
	ID       string    `json:"id"`
	At       float64   `json:"at"`
	Tool     string    `json:"tool"`
	Summary  string    `json:"summary"`
	Decision string    `json:"decision"` // allow or deny
	How      string    `json:"how"`      // auto, rule or card
	Rule     *Rule     `json:"rule,omitempty"`
	Scope    string    `json:"scope,omitempty"` // the deciding or remembered rule's: chat or workspace
	By       *cv.Actor `json:"by,omitempty"`
	Message  string    `json:"message,omitempty"`
}

const historyCap = 200

// record appends a permission event to the chat's history, keeping the
// last historyCap.
func (c *Chat) record(ev PermissionEvent) {
	if ev.ID == "" {
		ev.ID = cv.ID()
	}
	if ev.At == 0 {
		ev.At = float64(time.Now().UnixMilli()) / 1000
	}
	c.Permissions = append(c.Permissions, ev)
	if n := len(c.Permissions); n > historyCap {
		c.Permissions = append([]PermissionEvent(nil), c.Permissions[n-historyCap:]...)
	}
}

// Permissions is a chat's permission history, oldest first.
func (e *Engine) Permissions(id string) ([]PermissionEvent, error) {
	c := e.Store.Chat(id)
	if c == nil {
		return nil, errors.New("chat not found")
	}
	if c.Permissions == nil {
		return []PermissionEvent{}, nil
	}
	return c.Permissions, nil
}

// askSummary is the one line the history keeps of a call: a command, a
// path, a URL, else the call's title from its typed entry, else the
// tool's name.
func askSummary(tool string, input map[string]any, entry *cv.Entry) string {
	switch {
	case tool == "Bash":
		if c := strings.TrimSpace(agent.String(input["command"])); c != "" {
			return firstLine(c)
		}
	case fileTools[tool] || readTools[tool]:
		if p := inputPath(tool, input); p != "" && p != "." {
			return p
		}
	case tool == "WebFetch":
		if u := agent.String(input["url"]); u != "" {
			return u
		}
	case tool == "ExitPlanMode":
		return "the plan"
	}
	if entry != nil && entry.Text != "" {
		return firstLine(entry.Text)
	}
	return tool
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
