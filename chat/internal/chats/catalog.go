package chats

import (
	"context"
	"log"
	"time"
	"warden/chat/internal/agent"
)

// The model catalog (docs/claude-parity.md, R2.3): what a provider's CLI
// offers as models — Claude Code's `list_models`, asked through the
// adapter (`models/list`) each time a session starts, since the account's
// catalog can change between processes — kept per provider in the state
// and handed to clients as agentOptions.models, so the pickers' rows,
// each model's effort levels and which models have fast mode come from
// the CLI instead of a static list. Codex's app-server offers no such
// call; its rows stay the clients' own.

// ModelInfo is one row of a provider's catalog: the value a chat's model
// is set to, what it resolves to, its name and blurb, the effort levels
// it takes (none: the model has no effort setting), and whether it has
// adaptive thinking and fast mode.
type ModelInfo struct {
	Value            string   `json:"value"`
	Resolved         string   `json:"resolved,omitempty"`
	Label            string   `json:"label"`
	Description      string   `json:"description,omitempty"`
	Efforts          []string `json:"efforts,omitempty"`
	AdaptiveThinking bool     `json:"adaptiveThinking,omitempty"`
	FastMode         bool     `json:"fastMode,omitempty"`
}

// Catalog is a provider's rows and when they were reported (unix
// seconds).
type Catalog struct {
	Models []ModelInfo `json:"models"`
	At     float64     `json:"at"`
}

// catalogTimeout bounds the wait for the CLI's answer at a session
// start: it answers at once from its own tables, so a slow answer means
// something else is wrong and the turn should not wait.
const catalogTimeout = 10 * time.Second

// maxCatalog bounds the rows kept from one answer.
const maxCatalog = 64

// loadCatalog asks the session just started for the provider's catalog
// and records it when it answers with any rows; Codex is not asked.
func (e *Engine) loadCatalog(ctx context.Context, provider string, client *agent.Client) {
	if provider != "claude" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	response, err := client.Call(ctx, "models/list", map[string]any{})
	if err != nil {
		log.Printf("%s: model catalog not reported: %v", provider, err)
		return
	}
	models := ParseCatalog(response)
	if len(models) == 0 {
		return
	}
	at := e.at()
	_ = e.Store.update(func(st *State) error {
		if st.Catalog == nil {
			st.Catalog = map[string]*Catalog{}
		}
		if old := st.Catalog[provider]; old != nil && sameModels(old.Models, models) {
			return nil // unchanged: the record, and its time, stand
		}
		st.Catalog[provider] = &Catalog{Models: models, At: at}
		return nil
	})
}

// ParseCatalog reads Claude Code's list_models answer ({models: [{value,
// resolvedModel, displayName, description, supportedEffortLevels,
// supportsAdaptiveThinking, supportsFastMode}]}) into rows; a row
// without a value is dropped, strings are bounded.
func ParseCatalog(response map[string]any) []ModelInfo {
	var out []ModelInfo
	for _, v := range agent.Array(response["models"]) {
		m := agent.Map(v)
		value := bounded(agent.String(m["value"]), 100)
		if value == "" {
			continue
		}
		row := ModelInfo{Value: value, Resolved: bounded(agent.String(m["resolvedModel"]), 100), Label: bounded(agent.String(m["displayName"]), 100), Description: bounded(agent.String(m["description"]), 300), AdaptiveThinking: m["supportsAdaptiveThinking"] == true, FastMode: m["supportsFastMode"] == true}
		if row.Label == "" {
			row.Label = value
		}
		for _, l := range agent.Array(m["supportedEffortLevels"]) {
			if level := bounded(agent.String(l), 20); level != "" {
				row.Efforts = append(row.Efforts, level)
			}
		}
		out = append(out, row)
		if len(out) == maxCatalog {
			break
		}
	}
	return out
}

// bounded cuts s to n runes and drops control characters.
func bounded(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	out := r[:0]
	for _, c := range r {
		if c >= 0x20 && c != 0x7f {
			out = append(out, c)
		}
	}
	return string(out)
}

// sameModels reports whether two catalogs list the same rows.
func sameModels(a, b []ModelInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Value != b[i].Value || a[i].Resolved != b[i].Resolved || a[i].Label != b[i].Label || a[i].Description != b[i].Description || a[i].AdaptiveThinking != b[i].AdaptiveThinking || a[i].FastMode != b[i].FastMode || len(a[i].Efforts) != len(b[i].Efforts) {
			return false
		}
		for j := range a[i].Efforts {
			if a[i].Efforts[j] != b[i].Efforts[j] {
				return false
			}
		}
	}
	return true
}

// catalogRows is every provider's rows, for agentOptions.models; nil
// when none was reported yet.
func catalogRows(catalog map[string]*Catalog) map[string][]ModelInfo {
	if len(catalog) == 0 {
		return nil
	}
	out := map[string][]ModelInfo{}
	for provider, c := range catalog {
		if c != nil && len(c.Models) > 0 {
			out[provider] = c.Models
		}
	}
	return out
}
