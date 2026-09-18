package chats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

// Session settings of a Claude chat, beside its model and permission mode:
// the thinking budget, the effort level and fast mode. Each is kept on the
// chat record, applied at once to a live session through the adapter's
// control requests (set_max_thinking_tokens, apply_flag_settings) and
// pushed again before every turn, so a fresh process starts with them
// too. Codex chats have none (its app-server takes reasoning settings per
// turn, which Warden does not set yet).

// Efforts are the CLI's effort levels, lowest first; "" is the model's
// own default (high on the current models).
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ValidEffort reports whether level is one of Efforts or the default.
func ValidEffort(level string) bool {
	if level == "" {
		return true
	}
	for _, l := range Efforts {
		if l == level {
			return true
		}
	}
	return false
}

// ThinkingOff turns thinking off (a budget of zero).
const ThinkingOff = "off"

// maxThinkingBudget bounds a budget to what any model can spend.
const maxThinkingBudget = 128000

// ValidThinking reports whether setting is "", "off" or a token budget.
func ValidThinking(setting string) bool {
	if setting == "" || setting == ThinkingOff {
		return true
	}
	n, err := strconv.Atoi(setting)
	return err == nil && n > 0 && n <= maxThinkingBudget
}

// ParseThinking reads a person's thinking argument: on/default/adaptive
// for the default, off, or a budget as digits with an optional k suffix
// (8k). It returns the setting as stored.
func ParseThinking(arg string) (string, error) {
	switch a := strings.ToLower(strings.TrimSpace(arg)); a {
	case "on", "default", "adaptive", "auto":
		return "", nil
	case ThinkingOff, "none", "0":
		return ThinkingOff, nil
	default:
		n, err := strconv.Atoi(strings.TrimSuffix(a, "k"))
		if err == nil && strings.HasSuffix(a, "k") {
			n *= 1000
		}
		if err != nil || n <= 0 || n > maxThinkingBudget {
			return "", fmt.Errorf("thinking: on, off or a budget in tokens up to %d (8000, 8k)", maxThinkingBudget)
		}
		return strconv.Itoa(n), nil
	}
}

// Settings is a change to a chat's session settings: each field applies
// when present.
type Settings struct {
	Thinking *string `json:"thinking,omitempty"`
	Effort   *string `json:"effort,omitempty"`
	Fast     *bool   `json:"fast,omitempty"`
}

// thinkingNotice is the transcript marker for a thinking setting.
func thinkingNotice(setting string) string {
	switch setting {
	case "":
		return "Thinking: default — the model decides when and how much to think"
	case ThinkingOff:
		return "Thinking off"
	}
	n, _ := strconv.Atoi(setting)
	return "Thinking budget: " + formatTokens(n) + " tokens"
}

// formatTokens writes a token count the way the usage line does (1.2k,
// 27k).
func formatTokens(n int) string {
	switch {
	case n >= 1e6:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1e4:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 0, 64) + "k"
	case n >= 1e3:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.Itoa(n)
}

// effortNotice is the transcript marker for an effort level.
func effortNotice(level string) string {
	if level == "" {
		return "Effort: model default"
	}
	return "Effort: " + level
}

// fastNotice is the transcript marker for fast mode.
func fastNotice(on bool) string {
	if on {
		return "Fast mode on — faster answers at a higher price, on the models that offer it"
	}
	return "Fast mode off"
}

// SetSettings changes a Claude chat's session settings. Each present
// field is validated, recorded with a transcript marker when it changes,
// applied at once to a live session (a refusal is logged; the setting is
// re-applied at the next turn) and to every later session at its start.
func (e *Engine) SetSettings(ctx context.Context, id string, s Settings) error {
	if s.Effort != nil && !ValidEffort(*s.Effort) {
		return fmt.Errorf("unknown effort level %q: %s", *s.Effort, strings.Join(Efforts, ", "))
	}
	if s.Thinking != nil && !ValidThinking(*s.Thinking) {
		return errors.New("thinking: \"\" (default), \"off\" or a budget in tokens")
	}
	if s.Fast != nil && *s.Fast && !e.AllowFastMode {
		return errors.New("fast mode is not enabled on this Warden (providers.claude.allowFastMode)")
	}
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if c.Provider != "claude" {
			return errors.New("thinking, effort and fast mode apply to Claude chats")
		}
		if s.Thinking != nil && *s.Thinking != c.Thinking {
			c.Thinking = *s.Thinking
			c.Conversation.Entries = append(c.Conversation.Entries, cv.NewEntry("notice", thinkingNotice(c.Thinking)))
		}
		if s.Effort != nil && *s.Effort != c.Effort {
			c.Effort = *s.Effort
			c.Conversation.Entries = append(c.Conversation.Entries, cv.NewEntry("notice", effortNotice(c.Effort)))
		}
		if s.Fast != nil && *s.Fast != c.Fast {
			c.Fast = *s.Fast
			c.Conversation.Entries = append(c.Conversation.Entries, cv.NewEntry("notice", fastNotice(c.Fast)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if client := e.liveClient(id); client != nil {
		if c := e.Store.Snapshot().chat(id); c != nil {
			e.pushSettings(ctx, client, c)
		}
	}
	return nil
}

// liveClient is the agent client of the chat's session when one is up
// (its process answers control requests), nil otherwise.
func (e *Engine) liveClient(id string) *agent.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.active[id]; a != nil && a.id == id {
		return a.client
	}
	return nil
}

// pushSettings tells a live Claude session the chat's thinking, effort
// and fast-mode settings; the adapter sends each only when it changes
// what the CLI has.
func (e *Engine) pushSettings(ctx context.Context, client *agent.Client, c *Chat) {
	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"thinking/set", map[string]any{"thinking": c.Thinking}},
		{"effort/set", map[string]any{"effort": c.Effort}},
		{"fastMode/set", map[string]any{"fast": c.Fast}},
	} {
		callCtx, done := context.WithTimeout(ctx, 10*time.Second)
		_, err := client.Call(callCtx, call.method, call.params)
		done()
		if err != nil {
			log.Printf("%s not applied to the running session of chat %s: %v", call.method, c.ID, err)
		}
	}
}

// applySession gives a Claude session the chat's permission mode and
// session settings before a turn: a new process starts at the CLI's
// defaults whatever the chat says, and a setting changed while no session
// was up has not been pushed.
func (e *Engine) applySession(ctx context.Context, id string, c *Chat, client *agent.Client) {
	if c.Provider != "claude" {
		return
	}
	chat := e.Store.Snapshot().chat(id)
	if chat == nil {
		return
	}
	e.pushMode(ctx, client, chat.mode())
	e.pushSettings(ctx, client, chat)
}

// pushModel tells a live Claude session the chat's new model (set_model);
// the CLI's next system/init reports what it resolved. The error is the
// CLI's refusal.
func (e *Engine) pushModel(ctx context.Context, client *agent.Client, model string) error {
	callCtx, done := context.WithTimeout(ctx, 15*time.Second)
	defer done()
	_, err := client.Call(callCtx, "model/set", map[string]any{"model": model})
	return err
}

// modelRefused reports whether the CLI turned a model down as one it does
// not know ("Model 'x' not found"), which a relaunch with that model would
// not fix.
func modelRefused(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}

// modelNotice is the transcript marker for a model change.
func modelNotice(model string) string {
	if model == "" {
		return "Model → provider default"
	}
	return "Model → " + model
}

// longContextModel reports whether model is a 1M-context variant
// (`sonnet[1m]`, `opus[1m]`), offered only when the operator allows it.
func longContextModel(model string) bool {
	return strings.HasSuffix(strings.ToLower(model), "[1m]")
}
