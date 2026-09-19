package chats

import (
	"context"
	"errors"
	"warden/chat/internal/sandbox"
)

// A Claude chat's output style (docs/claude-parity.md, item 15): the
// CLI's `outputStyle` setting, which shapes its answers (Explanatory adds
// insights, Learning asks the person to write parts). The pinned CLI
// takes it only at launch (a `--settings` value; there is no control
// request for it), so the chat's idle session is released with the
// change and the next message starts a process with the new style,
// resuming the same session. Chat.Session.OutputStyle says what the
// running session actually has.

// OutputStyles lists the styles a chat may choose: the CLI's default
// ("") and its built-ins.
func OutputStyles() []string { return append([]string{""}, sandbox.OutputStyles...) }

// SetOutputStyle records style for chat id and ends its idle session so
// the next message launches with it. Claude chats only.
func (e *Engine) SetOutputStyle(ctx context.Context, id, style string) error {
	if !sandbox.ValidOutputStyle(style) {
		return errors.New("unknown output style; choose default, Explanatory or Learning")
	}
	err := e.Store.update(func(st *State) error {
		c := st.chat(id)
		if c == nil {
			return errors.New("chat not found")
		}
		if c.Provider != "claude" {
			return errors.New("output styles are a Claude chat's setting")
		}
		if c.Archived {
			return errors.New("chat is archived")
		}
		c.OutputStyle = style
		return nil
	})
	if err != nil {
		return err
	}
	e.releaseChat(ctx, id)
	return nil
}
