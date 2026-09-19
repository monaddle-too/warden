package chats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

// Permission modes decide how a Claude chat's tool asks (the CLI's
// can_use_tool control requests, forwarded by the adapter as
// item/tool/requestPermission) are answered. The sandbox is the security
// boundary; the mode is the owner's degree of oversight.
//
//   - auto (the default): every ask is allowed at once, as before.
//   - ask: the CLI's own rules decide what needs asking (commands that
//     write or are not read-only, file edits); each ask is an approval card
//     unless an allow-always rule of the chat matches.
//   - plan: the CLI is in its plan mode and withholds edits; the model's
//     ExitPlanMode is an approval card showing the plan, answered into
//     auto or ask, or sent back with feedback.
const (
	ModeAuto = "auto"
	ModeAsk  = "ask"
	ModePlan = "plan"
)

// methodPermission is the adapter's ask for a tool call, an approval when
// the mode does not answer it.
const methodPermission = "item/tool/requestPermission"

// Modes lists the modes in the order the surfaces cycle through them.
var Modes = []string{ModeAuto, ModeAsk, ModePlan}

// ValidMode reports whether mode is one an owner can set.
func ValidMode(mode string) bool {
	for _, m := range Modes {
		if m == mode {
			return true
		}
	}
	return false
}

// multiWordPrograms are the programs whose first argument is the command
// the person means when allowing "these" always: `git commit`, not `git`.
var multiWordPrograms = map[string]bool{"git": true, "npm": true, "pnpm": true, "yarn": true, "go": true, "cargo": true, "docker": true, "kubectl": true, "gh": true, "pip": true, "pip3": true, "make": true, "bundle": true, "poetry": true, "uv": true, "brew": true, "apt": true, "apt-get": true, "systemctl": true, "helm": true, "gcloud": true, "aws": true, "warden": true}

// shellControl is what makes a command more than one program: chaining,
// piping, substitution. Such a command is remembered whole.
var shellControl = []string{"&&", "||", ";", "|", "\n", "$(", "`", ">", "<"}

// RuleFor is the allow rule an "Allow always" answer to the given tool
// call records, as a pattern in the rules' syntax (rules.go): a Bash
// command's program with its subcommand for the programs that have one
// (`Bash(git commit *)`), the whole command when it chains, any file tool
// as `Edit` (which covers them all), every other tool by name.
func RuleFor(tool string, input map[string]any) string {
	if fileTools[tool] {
		return "Edit"
	}
	if tool != "Bash" && tool != hostRunTool {
		return tool
	}
	command := strings.TrimSpace(agent.String(input["command"]))
	for _, c := range shellControl {
		if strings.Contains(command, c) {
			return tool + "(" + command + ")"
		}
	}
	words := strings.Fields(command)
	if len(words) == 0 {
		return tool
	}
	// A leading assignment (FOO=1 make) is not the program.
	for len(words) > 1 && strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "=") {
		words = words[1:]
	}
	prefix := words[0]
	if multiWordPrograms[words[0]] && len(words) > 1 && !strings.HasPrefix(words[1], "-") {
		prefix = words[0] + " " + words[1]
	}
	return tool + "(" + prefix + " *)"
}

// RuleLabel says what a rule pattern covers, for the card's button: "`git
// commit` commands", "file edits", "WebFetch calls".
func RuleLabel(pattern string) string {
	tool, spec, err := splitPattern(pattern)
	if err != nil {
		return pattern
	}
	switch {
	case tool == "Edit" && spec == "":
		return "file edits"
	case tool == "Bash" && spec == "":
		return "commands"
	case tool == hostRunTool && spec == "":
		return "host commands"
	case tool == "Bash":
		return "`" + strings.TrimSuffix(strings.TrimSuffix(spec, " *"), ":*") + "` commands"
	case tool == hostRunTool:
		return "`" + strings.TrimSuffix(strings.TrimSuffix(spec, " *"), ":*") + "` host commands"
	case spec != "":
		return tool + " " + spec
	}
	return tool + " calls"
}

// mode is the chat's permission mode, auto when unset (chats from before
// modes existed).
func (c *Chat) mode() string {
	if c.Mode == "" {
		return ModeAuto
	}
	return c.Mode
}

// modeNotice is the transcript marker for a mode.
func modeNotice(mode string) string {
	switch mode {
	case ModeAsk:
		return "Permission mode: ask — Claude asks before commands that write and before file edits"
	case ModePlan:
		return "Permission mode: plan — Claude explores and proposes a plan; edits wait for its approval"
	}
	return "Permission mode: auto — every tool call is allowed"
}

// SetMode sets a chat's permission mode. It applies at once to a live
// Claude session (set_permission_mode through the adapter; a refusal is
// logged and the mode is re-applied at the next turn) and to the next
// session otherwise.
func (e *Engine) SetMode(ctx context.Context, id, mode string) error {
	if !ValidMode(mode) {
		return fmt.Errorf("unknown permission mode %q: auto, ask or plan", mode)
	}
	var provider string
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		provider = c.Provider
		if provider != "claude" {
			return errors.New("permission modes apply to Claude chats")
		}
		if c.mode() == mode {
			return nil
		}
		c.Mode = mode
		c.Conversation.Entries = append(c.Conversation.Entries, cv.NewEntry("notice", modeNotice(mode)))
		return nil
	})
	if err != nil {
		return err
	}
	if client := e.liveClient(id); client != nil {
		e.pushMode(ctx, client, mode)
	}
	return nil
}

// pushMode tells a live Claude session the chat's mode; the adapter sends
// set_permission_mode only when that changes the CLI's mode.
func (e *Engine) pushMode(ctx context.Context, client *agent.Client, mode string) {
	callCtx, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	if _, err := client.Call(callCtx, "permissions/set", map[string]any{"mode": mode}); err != nil {
		log.Printf("permission mode %s not applied to the running session: %v", mode, err)
	}
}

// applyMode records the mode the CLI reports (permissions/modeChanged):
// the model entering plan mode by itself puts the chat in plan; the CLI
// leaving plan mode without the chat having moved puts it in ask.
func (c *Chat) applyMode(cliMode string) {
	switch {
	case cliMode == "plan" && c.mode() != ModePlan:
		c.Mode = ModePlan
		c.Conversation.Entries = append(c.Conversation.Entries, cv.NewEntry("notice", "Claude entered plan mode — edits wait for the plan's approval"))
	case cliMode != "plan" && c.mode() == ModePlan:
		c.Mode = ModeAsk
		c.Conversation.Entries = append(c.Conversation.Entries, cv.NewEntry("notice", modeNotice(ModeAsk)))
	}
}

// Verdict is how a permission ask was answered without a card: the
// decision ("accept" or "decline"), the message a decline carries to the
// model, and the rule behind it (nil for the mode). Nil means a card.
type Verdict struct {
	Decision string
	Message  string
	Rule     *Decision
}

// decide answers a permission ask from the chat's rules, its workspace's
// and its mode: a deny rule declines in every mode, an ask rule makes it
// a card in every mode, an allow rule accepts, else auto accepts and ask
// and plan make it a card. A plan (ExitPlanMode) is always the owner's.
func (st *State) decide(c *Chat, tool string, input map[string]any) *Verdict {
	if tool == "ExitPlanMode" {
		return nil
	}
	if d := st.decideByRule(c, tool, input); d != nil {
		switch d.Rule.Kind {
		case RuleDeny:
			return &Verdict{Decision: "decline", Message: denialFor(d), Rule: d}
		case RuleAsk:
			return nil
		}
		return &Verdict{Decision: "accept", Rule: d}
	}
	if c.mode() == ModeAuto {
		return &Verdict{Decision: "accept"}
	}
	return nil
}

// denialFor is what the model reads when a deny rule answers: the rule
// and where it lives, inside the CLI's own rejection wording (the adapter
// adds that).
func denialFor(d *Decision) string {
	return "a permission rule of this " + d.Scope + " denies it (deny " + d.Rule.Pattern + "); do not retry it, find another way or ask"
}

// permissionEntry is the asked call as a transcript entry (the item-1
// typed card: a command, a diff, a read's path), for the approval card.
func permissionEntry(params map[string]any) *cv.Entry {
	item := agent.Map(params["item"])
	if item == nil {
		return nil
	}
	var scratch cv.Conversation
	scratch.Upsert(item, "", false)
	if len(scratch.Entries) == 0 {
		return nil
	}
	e := scratch.Entries[0]
	e.IsStreaming = false
	e.TurnID = nil
	return &e
}
