package chats

// The model a chat gets when none is chosen: a chat always has a concrete
// model, so the surfaces never show a "provider default" row. The
// operator may name another in warden.json (providers.<p>.defaultModel);
// a chat recorded before defaults existed (model "") resolves the same
// way when it launches and when clients read it.
const (
	DefaultClaudeModel = "opus"
	DefaultCodexModel  = "gpt-5.6-sol"
)

// DefaultModel is the model chats of `provider` start with: the operator's
// choice when set, else the built-in default.
func (e *Engine) DefaultModel(provider string) string {
	if provider == "" {
		provider = "codex"
	}
	if m := e.DefaultModels[provider]; m != "" {
		return m
	}
	switch provider {
	case "claude":
		return DefaultClaudeModel
	default:
		return DefaultCodexModel
	}
}

// modelOf is the model chat `c` runs: its own, or the provider's default
// for a chat recorded without one.
func (e *Engine) modelOf(c *Chat) string {
	if c.Model != "" {
		return c.Model
	}
	return e.DefaultModel(c.Provider)
}

// defaultModels is what clients are told, by provider.
func (e *Engine) defaultModels() map[string]string {
	return map[string]string{"claude": e.DefaultModel("claude"), "codex": e.DefaultModel("codex")}
}

// fillDefaultModels gives every chat recorded without a model the
// provider's default, once, so the model a chat runs is always the one
// its record says.
func (e *Engine) fillDefaultModels() {
	_ = e.Store.update(func(st *State) error {
		for _, c := range st.Chats {
			if c.Model == "" {
				c.Model = e.DefaultModel(c.Provider)
			}
		}
		return nil // an unchanged state is not saved
	})
}
