package tui

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Export writes the transcript the way the web's export does
// (chat/web/src/export.ts): markdown with the messages as the agent and the
// owner wrote them, or JSON with the chat's records as the service sent
// them under a header naming the format. Nothing is rendered; an agent's
// text goes into the file exactly as written.

// ProviderName is the provider as a person reads it.
func ProviderName(provider string) string {
	if provider == "claude" {
		return "Claude"
	}
	return "Codex"
}

// SenderLabel is who wrote a user entry: their name, else their email,
// else the owner ("You") or a collaborator.
func SenderLabel(e Entry) string {
	if e.Sender == nil {
		return "You"
	}
	switch {
	case e.Sender.Name != "":
		return e.Sender.Name
	case e.Sender.Email != "":
		return e.Sender.Email
	case e.Sender.PrincipalID == "owner":
		return "You"
	}
	return "Collaborator"
}

func authorLabel(e Entry, provider string) string {
	if e.Role == "user" {
		return SenderLabel(e)
	}
	return ProviderName(provider)
}

// LocalTime is the export's timestamp: local time to the minute.
func LocalTime(seconds float64) string {
	return time.Unix(0, int64(seconds*float64(time.Second))).Local().Format("2006-01-02 15:04")
}

var fenceRuns = regexp.MustCompile("`{3,}")

// fenceFor is a fence the text cannot close: one backtick longer than its
// longest run.
func fenceFor(text string) string {
	longest := 0
	for _, run := range fenceRuns.FindAllString(text, -1) {
		longest = max(longest, len(run))
	}
	return strings.Repeat("`", max(3, longest+1))
}

// exportable says whether an entry goes into the file: tool steps and the
// model's thinking are the agent's working, kept out unless asked for.
func exportable(role string, activity bool) bool {
	return activity || (role != "activity" && role != "thinking")
}

// FormatDuration is "0.8s", "12s", "1m 05s", "1h 02m".
func FormatDuration(seconds float64) string {
	s := math.Max(0, seconds)
	switch {
	case s < 10:
		return fmt.Sprintf("%.1fs", s)
	case s < 60:
		return fmt.Sprintf("%ds", int(math.Round(s)))
	}
	whole := int(math.Round(s))
	if whole < 3600 {
		return fmt.Sprintf("%dm %02ds", whole/60, whole%60)
	}
	return fmt.Sprintf("%dh %02dm", whole/3600, (whole%3600)/60)
}

// FormatTokens is "842", "9.5k", "13k", "1.2M".
func FormatTokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1e6:
		return fmt.Sprintf("%dk", int(math.Round(float64(n)/1000)))
	}
	return fmt.Sprintf("%.1fM", float64(n)/1e6)
}

// FormatCost is "$0.04", or "<$0.01" for an estimate that rounds to nothing.
func FormatCost(usd float64) string {
	if usd < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}

// UsageSummary is the tokens in one phrase: the total with the in/out split.
func UsageSummary(u Usage) string {
	return fmt.Sprintf("%s tokens (%s in, %s out)", FormatTokens(u.Total), FormatTokens(u.Input), FormatTokens(u.Output))
}

// FormatSize is an attachment's size as the web shows it.
func FormatSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%d KB", int(math.Round(float64(bytes)/1024)))
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}

// turnFooter is what a finished turn took, for the line under its last
// agent entry; see turns.ts.
type turnFooter struct {
	start, end float64
	usage      *Usage
}

func (f turnFooter) text() string {
	var parts []string
	if f.start > 0 && f.end > 0 {
		parts = append(parts, FormatDuration(f.end-f.start))
	}
	if f.usage != nil {
		parts = append(parts, UsageSummary(*f.usage))
		if f.usage.CostUSD != 0 {
			parts = append(parts, FormatCost(f.usage.CostUSD))
		}
	}
	return strings.Join(parts, " · ")
}

// turnFooters keys each finished turn's footer by the ID of its last agent
// entry. A turn without a record and without an end has no line.
func turnFooters(c *Chat) map[string]turnFooter {
	last := map[string]string{}
	sent := map[string]float64{}
	var order []string
	for _, e := range c.Conversation.Entries {
		if e.TurnID == nil {
			continue
		}
		id := *e.TurnID
		if e.Role == "user" {
			if _, ok := sent[id]; !ok {
				sent[id] = e.CreatedAt
			}
			continue
		}
		if _, ok := last[id]; !ok {
			order = append(order, id)
		}
		last[id] = e.ID
	}
	out := map[string]turnFooter{}
	for _, id := range order {
		record := c.Turn(id)
		f := turnFooter{}
		if record != nil {
			f.end = record.EndedAt
			f.usage = record.Usage
			f.start = record.StartedAt
		}
		if at, ok := sent[id]; ok {
			f.start = at
		}
		if f.end == 0 && f.usage == nil {
			continue
		}
		out[last[id]] = f
	}
	return out
}

func quote(text string) string {
	return "> " + strings.Join(strings.Split(text, "\n"), "\n> ")
}

// entryMarkdown is one entry as markdown; a message keeps its text
// verbatim (it is markdown already), everything else is described.
func entryMarkdown(e Entry, provider string, at func(float64) string) []string {
	var lines []string
	switch e.Role {
	case "user", "assistant":
		lines = append(lines, fmt.Sprintf("## %s — %s", authorLabel(e, provider), at(e.CreatedAt)), "")
		switch e.Delivery {
		case "failed":
			detail := ""
			if e.Detail != "" {
				detail = ": " + e.Detail
			}
			lines = append(lines, "_Not delivered"+detail+"_", "")
		case "queued":
			lines = append(lines, "_Queued_", "")
		}
		if e.Text != "" {
			lines = append(lines, e.Text, "")
		}
		if len(e.Attachments) > 0 {
			var parts []string
			for _, a := range e.Attachments {
				parts = append(parts, fmt.Sprintf("`%s` (%s, `%s`)", a.Name, FormatSize(a.Size), a.Path))
			}
			lines = append(lines, "Attachments: "+strings.Join(parts, ", "), "")
		}
	case "activity":
		title := e.Text
		if title == "" {
			title = "Agent activity"
		}
		lines = append(lines, "### Activity — "+title, "")
		if e.Detail != "" {
			fence := fenceFor(e.Detail)
			lines = append(lines, fence, strings.TrimSuffix(e.Detail, "\n"), fence, "")
		}
	case "thinking":
		lines = append(lines, "### Thinking", "")
		if e.Text != "" {
			lines = append(lines, quote(e.Text), "")
		}
	case "image":
		caption := ""
		if e.Text != "" {
			caption = ": " + e.Text
		}
		lines = append(lines, "_Image"+caption+"_", "")
	default:
		lines = append(lines, quote(e.Text), "")
	}
	return lines
}

// ExportMarkdown is the chat as a markdown transcript; activity includes
// tool steps and thinking. at formats a unix time (LocalTime normally).
func ExportMarkdown(c *Chat, activity bool, now time.Time, at func(float64) string) string {
	if at == nil {
		at = LocalTime
	}
	title := c.Title
	if title == "" {
		title = "Chat"
	}
	head := []string{"# " + title, ""}
	agent := "Agent: " + ProviderName(c.Provider)
	if c.Model != "" {
		agent += " (" + c.Model + ")"
	}
	facts := []string{agent}
	if c.Repository != "" {
		facts = append(facts, "Repository: "+c.Repository)
	}
	facts = append(facts, "Exported: "+at(float64(now.UnixMilli())/1000))
	for _, f := range facts {
		head = append(head, "- "+f)
	}
	head = append(head, "", "---", "")
	footers := turnFooters(c)
	body := head
	for _, e := range c.Conversation.Entries {
		if !exportable(e.Role, activity) {
			continue
		}
		body = append(body, entryMarkdown(e, c.Provider, at)...)
		if f, ok := footers[e.ID]; ok {
			if took := f.text(); took != "" {
				body = append(body, "_Turn: "+took+"_", "")
			}
		}
	}
	return strings.TrimRight(strings.Join(body, "\n"), "\n") + "\n"
}

// ExportJSON is the chat's own records, as the service sent them, under a
// header that names the format.
func ExportJSON(c *Chat, activity bool, now time.Time) ([]byte, error) {
	entries := []json.RawMessage{}
	for i, e := range c.Conversation.Entries {
		if !exportable(e.Role, activity) {
			continue
		}
		if i < len(c.Conversation.Raw) {
			entries = append(entries, c.Conversation.Raw[i])
			continue
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		entries = append(entries, raw)
	}
	turns := c.Conversation.Turns
	if turns == nil {
		turns = []Turn{}
	}
	doc := struct {
		Format     string `json:"format"`
		Version    int    `json:"version"`
		ExportedAt string `json:"exportedAt"`
		Chat       struct {
			ID         string  `json:"id"`
			Title      string  `json:"title"`
			Provider   string  `json:"provider"`
			Model      string  `json:"model"`
			Repository string  `json:"repository"`
			Status     string  `json:"status"`
			Archived   bool    `json:"archived"`
			ThreadID   *string `json:"threadID,omitempty"`
		} `json:"chat"`
		Entries []json.RawMessage `json:"entries"`
		Turns   []Turn            `json:"turns"`
	}{Format: "warden-chat", Version: 1, ExportedAt: now.UTC().Format("2006-01-02T15:04:05.000Z07:00"), Entries: entries, Turns: turns}
	doc.Chat.ID, doc.Chat.Title, doc.Chat.Provider, doc.Chat.Model = c.ID, c.Title, c.Provider, c.Model
	doc.Chat.Repository, doc.Chat.Status, doc.Chat.Archived, doc.Chat.ThreadID = c.Repository, c.Status, c.Archived, c.Conversation.ThreadID
	return json.MarshalIndent(doc, "", "  ")
}

var slugJunk = regexp.MustCompile(`[^a-z0-9.]+`)

// ExportName is a file name from the title: ASCII letters, digits, dots
// and dashes, with the time so two exports of one chat do not collide.
func ExportName(title, format string, now time.Time) string {
	slug := slugJunk.ReplaceAllString(strings.ToLower(title), "-")
	slug = strings.Trim(slug, "-.")
	if len(slug) > 60 {
		slug = slug[:60]
	}
	slug = strings.TrimRight(slug, "-.")
	if slug == "" {
		slug = "chat"
	}
	ext := "md"
	if format == "json" {
		ext = "json"
	}
	return fmt.Sprintf("%s-%s.%s", slug, now.Format("20060102-1504"), ext)
}
