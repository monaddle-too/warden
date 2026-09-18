package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// claudeTool is a Claude Code tool call in flight: what its tool_use block
// said, kept until the tool_result completes the item, since the result
// names only the call's id.
type claudeTool struct {
	name  string
	input map[string]any
}

const (
	claudeWorkspace = "/home/agent/workspace" // the guest's working directory (sandbox/managed.go)
	claudeDiffCap   = 60000                   // runes of diff kept per change
)

// claudeToolItem is the Warden item for the tool call `id`: at its start
// (result nil) or with its tool_result block and the CLI's structured
// tool_use_result, which carries what the model's text does not (an edit's
// hunks with line numbers, whether a write created or replaced the file).
// The item's type follows the CLI's tool name: Bash is a command execution
// and the file tools are file changes, both in Codex's shape so the
// surfaces render them alike; a Warden MCP tool is an MCP call; a web
// search is Codex's webSearch; everything else is a generic toolCall with
// a kind the surfaces render by.
func claudeToolItem(id string, t claudeTool, result map[string]any, structured any) map[string]any {
	status := "running"
	output := ""
	if result != nil {
		status = "completed"
		if result["is_error"] == true {
			status = "failed"
		}
		output = claudeResultText(result["content"])
	}
	in := t.input
	switch {
	case t.name == "Bash":
		item := map[string]any{"id": id, "type": "commandExecution", "tool": "Bash", "command": String(in["command"]), "status": status, "aggregatedOutput": output}
		if d := String(in["description"]); d != "" {
			item["description"] = d
		}
		return item
	case t.name == "Edit" || t.name == "MultiEdit" || t.name == "Write" || t.name == "NotebookEdit":
		return map[string]any{"id": id, "type": "fileChange", "tool": t.name, "status": status, "changes": []any{claudeFileChange(t, structured)}, "output": output}
	case strings.HasPrefix(t.name, "mcp__"):
		server, tool := claudeMCPName(t.name)
		item := map[string]any{"id": id, "type": "mcpToolCall", "server": server, "tool": tool, "arguments": in, "status": status}
		if result != nil {
			item["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": output}}, "isError": status == "failed"}
		}
		return item
	case t.name == "WebSearch":
		return map[string]any{"id": id, "type": "webSearch", "tool": t.name, "query": String(in["query"]), "status": status, "output": output}
	}
	kind, title, paths, query := claudeToolTitle(t)
	item := map[string]any{"id": id, "type": "toolCall", "tool": t.name, "kind": kind, "title": title, "status": status, "output": output, "input": claudeToolInput(in)}
	if len(paths) > 0 {
		list := make([]any, 0, len(paths))
		for _, p := range paths {
			list = append(list, p)
		}
		item["paths"] = list // as it arrives after JSON, whichever side reads it
	}
	if query != "" {
		item["query"] = query
	}
	return item
}

// claudeToolTitle is how a generic tool call reads in the transcript: its
// kind for the surfaces, a title in the tool's own terms, the paths and
// query it names.
func claudeToolTitle(t claudeTool) (kind, title string, paths []string, query string) {
	in := t.input
	name := t.name
	if name == "" {
		name = "tool"
	}
	path := claudePath(String(in["file_path"]))
	if path == "" {
		path = claudePath(String(in["path"]))
	}
	switch name {
	case "Read":
		title = "Read " + path
		if offset, limit := in["offset"], in["limit"]; offset != nil || limit != nil {
			from := claudeInt(offset)
			if from < 1 {
				from = 1
			}
			if n := claudeInt(limit); n > 0 {
				title += fmt.Sprintf(" (lines %d–%d)", from, from+n-1)
			} else {
				title += fmt.Sprintf(" (from line %d)", from)
			}
		}
		return "read", title, []string{path}, ""
	case "Grep":
		query = String(in["pattern"])
		title = "Grep " + claudeQuote(query) + " in " + claudeOr(path, ".")
		if g := String(in["glob"]); g != "" {
			title += " " + g
		}
		return "search", title, []string{claudeOr(path, ".")}, query
	case "Glob":
		query = String(in["pattern"])
		return "search", "Glob " + claudeQuote(query) + " in " + claudeOr(path, "."), []string{claudeOr(path, ".")}, query
	case "LS":
		return "search", "List " + claudeOr(path, "."), []string{claudeOr(path, ".")}, ""
	case "WebFetch":
		query = String(in["url"])
		return "fetch", "Fetch " + query, nil, query
	case "Agent", "Task":
		title = "Agent"
		if d := String(in["description"]); d != "" {
			title += ": " + d
		}
		if sub := String(in["subagent_type"]); sub != "" {
			title += " (" + sub + ")"
		}
		return "task", title, nil, ""
	case "Skill":
		title = "Skill /" + String(in["skill"])
		if args := String(in["args"]); args != "" {
			title += " " + args
		}
		return "other", title, nil, ""
	case "ToolSearch":
		query = String(in["query"])
		return "other", "ToolSearch " + claudeQuote(query), nil, query
	case "TodoWrite":
		return "other", fmt.Sprintf("Update todos (%d)", len(Array(in["todos"]))), nil, ""
	case "AskUserQuestion":
		title = "Question"
		if qs := Array(in["questions"]); len(qs) > 0 {
			title += ": " + String(Map(qs[0])["question"])
		}
		return "other", title, nil, ""
	}
	title = name
	if path != "" {
		title += " " + path
		paths = []string{path}
	} else if brief := claudeBrief(in); brief != "" {
		title += " " + brief
	}
	return "other", title, paths, ""
}

// claudeBrief is the one input field worth a title: the first short string
// among the common names, so an unknown tool still reads as what it did.
func claudeBrief(in map[string]any) string {
	for _, k := range []string{"description", "query", "prompt", "url", "name", "pattern", "command", "text"} {
		if s := String(in[k]); s != "" {
			return claudeCut(strings.SplitN(s, "\n", 2)[0], 80)
		}
	}
	return ""
}

// claudeToolInput is the input kept on a generic item: every field, with
// long strings cut, so the transcript never stores a prompt-sized value
// twice.
func claudeToolInput(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = claudeCut(s, 2000)
		} else {
			out[k] = v
		}
	}
	return out
}

// claudeFileChange is the change a file tool makes, as Codex reports one:
// the path, whether the file is added or updated, and a unified diff. At
// the call's start the diff is what the input says (an edit's old and new
// text as a hunk without line numbers, a write's content as an added
// file); the structured result replaces it with the CLI's own hunks, which
// carry the line numbers, and says whether a write created the file.
func claudeFileChange(t claudeTool, structured any) map[string]any {
	in := t.input
	path := String(in["file_path"])
	if path == "" {
		path = String(in["notebook_path"])
	}
	path = claudePath(path)
	kind := "update"
	var hunks []claudeHunk
	switch t.name {
	case "Edit":
		hunks = []claudeHunk{claudeReplaceHunk(String(in["old_string"]), String(in["new_string"]))}
	case "MultiEdit":
		for _, e := range Array(in["edits"]) {
			m := Map(e)
			hunks = append(hunks, claudeReplaceHunk(String(m["old_string"]), String(m["new_string"])))
		}
	case "Write":
		kind = "add"
		h := claudeReplaceHunk("", String(in["content"]))
		h.oldStart, h.oldLines, h.newStart = 0, 0, 1
		h.numbered = true
		hunks = []claudeHunk{h}
	case "NotebookEdit":
		h := claudeReplaceHunk("", String(in["new_source"]))
		if String(in["edit_mode"]) == "delete" {
			h = claudeHunk{}
		}
		if cell := String(in["cell_id"]); cell != "" {
			h.section = "cell " + cell
		} else if mode := String(in["edit_mode"]); mode != "" {
			h.section = mode
		}
		hunks = []claudeHunk{h}
	}
	if s := Map(structured); s != nil {
		if patch := Array(s["structuredPatch"]); len(patch) > 0 {
			hunks = hunks[:0]
			for _, p := range patch {
				m := Map(p)
				h := claudeHunk{oldStart: claudeInt(m["oldStart"]), oldLines: claudeInt(m["oldLines"]), newStart: claudeInt(m["newStart"]), newLines: claudeInt(m["newLines"]), numbered: true}
				for _, l := range Array(m["lines"]) {
					h.lines = append(h.lines, String(l))
				}
				hunks = append(hunks, h)
			}
		}
		switch String(s["type"]) {
		case "create":
			kind = "add"
		case "update":
			kind = "update"
		}
	}
	// A failed call changed nothing; its diff stays as what was asked.
	return map[string]any{"path": path, "kind": kind, "diff": claudeUnifiedDiff(path, kind, hunks)}
}

// claudeHunk is one hunk of a change. Without numbers (numbered false) the
// hunk has no `@@` header: the line it applies at is not known before the
// CLI reports it, and a wrong number would be worse than none.
type claudeHunk struct {
	oldStart, oldLines, newStart, newLines int
	lines                                  []string
	numbered                               bool
	section                                string
}

// claudeReplaceHunk is old text replaced by new text as removed and added
// lines, counted so a header can be written once the start is known.
func claudeReplaceHunk(old, new string) claudeHunk {
	h := claudeHunk{}
	for _, l := range claudeLines(old) {
		h.lines = append(h.lines, "-"+l)
		h.oldLines++
	}
	for _, l := range claudeLines(new) {
		h.lines = append(h.lines, "+"+l)
		h.newLines++
	}
	return h
}

func claudeLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// claudeUnifiedDiff writes the hunks as a unified diff with a git header,
// which is what DiffView.tsx and the TUI already read for Codex's changes.
// A hunk without line numbers is written headerless after the file
// header; a surface colours its lines by prefix.
func claudeUnifiedDiff(path, kind string, hunks []claudeHunk) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
	if kind == "add" {
		b.WriteString("new file mode 100644\n--- /dev/null\n")
	} else {
		fmt.Fprintf(&b, "--- a/%s\n", path)
	}
	fmt.Fprintf(&b, "+++ b/%s\n", path)
	for _, h := range hunks {
		if h.numbered {
			fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@", h.oldStart, h.oldLines, h.newStart, h.newLines)
			if h.section != "" {
				b.WriteString(" " + h.section)
			}
			b.WriteString("\n")
		} else if h.section != "" {
			b.WriteString("@@ " + h.section + "\n")
		}
		for _, l := range h.lines {
			b.WriteString(l + "\n")
		}
	}
	return claudeCut(b.String(), claudeDiffCap)
}

// claudeResultText is a tool_result's content as text: the string, or the
// text blocks joined (an image block is named, not carried). The CLI wraps
// a tool's own error in a tag the model reads; the transcript shows the
// message alone.
func claudeResultText(content any) string {
	text := ""
	switch c := content.(type) {
	case string:
		text = c
	case []any:
		parts := []string{}
		for _, v := range c {
			m := Map(v)
			switch String(m["type"]) {
			case "text":
				parts = append(parts, String(m["text"]))
			case "image":
				parts = append(parts, "[image]")
			default:
				if m != nil {
					data, _ := json.Marshal(m)
					parts = append(parts, string(data))
				}
			}
		}
		text = strings.Join(parts, "\n")
	case nil:
	default:
		data, _ := json.Marshal(c)
		text = string(data)
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<tool_use_error>") && strings.HasSuffix(text, "</tool_use_error>") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "<tool_use_error>"), "</tool_use_error>"))
	}
	return text
}

// claudeMCPName splits `mcp__server__tool` into its server and tool.
func claudeMCPName(name string) (server, tool string) {
	parts := strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "mcp", parts[0]
}

// claudePath is a path as the transcript shows it: relative to the
// workspace when inside it, as given otherwise.
func claudePath(p string) string {
	if p == claudeWorkspace {
		return "."
	}
	return strings.TrimPrefix(p, claudeWorkspace+"/")
}

func claudeQuote(s string) string { return "'" + claudeCut(strings.ReplaceAll(s, "\n", " "), 80) + "'" }
func claudeOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
func claudeCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
func claudeInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
