package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// claudeTool is a Claude Code tool call in flight: what its tool_use block
// said, kept until the tool_result completes the item, since the result
// names only the call's id. parent is the Agent call whose subagent made
// this call ("" for the conversation's own), turn the turn the item
// belongs to (a subagent's items keep their Agent call's turn, which may
// have ended). background marks a call the CLI runs as a background task
// (a command with run_in_background, an async subagent): task is its task
// id and the call's item stays running until the task reports back.
// taskTitle names the task a TaskOutput, TaskStop or Monitor call waits
// on, for its card.
type claudeTool struct {
	name       string
	input      map[string]any
	parent     string
	turn       string
	background bool
	task       string
	taskTitle  string
}

// claudeTodoTool says whether the tool writes the agent's todo list, whose
// current state is the card rather than each write.
func claudeTodoTool(name string) bool {
	switch name {
	case "TodoWrite", "TaskCreate", "TaskUpdate", "TaskList", "TaskGet":
		return true
	}
	return false
}

// claudeBackgroundTask is the task id when the call's result says the
// CLI took it to the background: a command's `backgroundTaskId`, an
// async subagent's `agentId`; from the result text (or the task the CLI
// announced with task_started) when the structured result did not come
// (several results in one frame).
func claudeBackgroundTask(t claudeTool, result map[string]any, structured any) string {
	s := Map(structured)
	switch t.name {
	case "Bash":
		if id := String(s["backgroundTaskId"]); id != "" {
			return id
		}
		if s == nil && strings.HasPrefix(claudeResultText(result["content"]), "Command running in background") {
			return claudeOr(t.task, "background")
		}
	case "Agent", "Task":
		if s["isAsync"] == true || String(s["status"]) == "async_launched" {
			return claudeOr(String(s["agentId"]), claudeOr(t.task, "background"))
		}
		if s == nil && strings.HasPrefix(claudeResultText(result["content"]), "Async agent launched") {
			return claudeOr(t.task, "background")
		}
	}
	return ""
}

// claudeTaskStatus reads a task notification's outcome: failed when the
// CLI says so or the summary names a non-zero exit code, else completed.
func claudeTaskStatus(status, summary string) string {
	switch status {
	case "", "completed", "success":
	default:
		return "failed"
	}
	if i := strings.LastIndex(summary, "exit code "); i >= 0 {
		code := strings.TrimRight(strings.Fields(summary[i+len("exit code "):] + " ")[0], ").,")
		if code != "" && code != "0" {
			return "failed"
		}
	}
	return "completed"
}

// claudeTaskOutput reads what a TaskOutput call retrieved: the task's id,
// its output and whether it failed, from the structured result (the text
// form wraps the same in tags the model reads).
func claudeTaskOutput(result map[string]any, structured any) (task, output, status string) {
	s := Map(structured)
	if s != nil {
		if String(s["retrieval_status"]) != "" && String(s["retrieval_status"]) != "success" {
			return "", "", ""
		}
		task := Map(s["task"])
		if task == nil {
			return "", "", ""
		}
		status = "completed"
		if st := String(task["status"]); st != "" && st != "completed" || claudeInt(task["exitCode"]) != 0 {
			status = "failed"
		}
		return String(task["task_id"]), strings.TrimSpace(String(task["output"])), status
	}
	text := claudeResultText(result["content"])
	tag := func(name string) string {
		open, close := "<"+name+">", "</"+name+">"
		i := strings.Index(text, open)
		if i < 0 {
			return ""
		}
		rest := text[i+len(open):]
		if j := strings.Index(rest, close); j >= 0 {
			rest = rest[:j]
		}
		return strings.TrimSpace(rest)
	}
	if tag("retrieval_status") != "success" {
		return "", "", ""
	}
	status = "completed"
	if st := tag("status"); st != "" && st != "completed" || tag("exit_code") != "" && tag("exit_code") != "0" {
		status = "failed"
	}
	return tag("task_id"), tag("output"), status
}

// claudeTodo is one item of the agent's todo list, in TodoWrite's terms:
// what to do, its status (pending, in_progress, completed) and the
// present-tense form shown while it is in progress. id is the task tools'
// id, "" for a TodoWrite item.
type claudeTodo struct {
	id, content, activeForm, status string
}

// claudeTodoList is the agent's todo list as its writes left it: TodoWrite
// replaces the whole list; TaskCreate, TaskUpdate, TaskList and TaskGet
// (the newer task tools) add, patch and refresh items by id.
type claudeTodoList struct {
	items []claudeTodo
}

// apply folds one successful write into the list; false when the call
// changed nothing the surfaces show.
func (l *claudeTodoList) apply(tool string, input map[string]any, result map[string]any, structured any) bool {
	s := Map(structured)
	switch tool {
	case "TodoWrite":
		l.items = l.items[:0]
		for _, v := range Array(input["todos"]) {
			m := Map(v)
			l.items = append(l.items, claudeTodo{content: String(m["content"]), activeForm: String(m["activeForm"]), status: claudeTodoStatus(String(m["status"]))})
		}
		return true
	case "TaskCreate":
		id := String(Map(s["task"])["id"])
		if id == "" {
			// "Task #3 created successfully: subject"
			if text := claudeResultText(result["content"]); strings.HasPrefix(text, "Task #") {
				id = strings.TrimRight(strings.Fields(text)[1], ":")[1:]
			}
		}
		l.items = append(l.items, claudeTodo{id: id, content: String(input["subject"]), activeForm: String(input["activeForm"]), status: "pending"})
		return true
	case "TaskUpdate":
		i := l.index(String(input["taskId"]))
		if i < 0 {
			return false
		}
		if status := String(input["status"]); status == "deleted" {
			l.items = append(l.items[:i], l.items[i+1:]...)
			return true
		} else if status != "" {
			l.items[i].status = claudeTodoStatus(status)
		}
		if subject := String(input["subject"]); subject != "" {
			l.items[i].content = subject
		}
		if form := String(input["activeForm"]); form != "" {
			l.items[i].activeForm = form
		}
		return true
	case "TaskList":
		var listed []claudeTodo
		if tasks := Array(s["tasks"]); s != nil {
			for _, v := range tasks {
				m := Map(v)
				listed = append(listed, claudeTodo{id: String(m["id"]), content: String(m["subject"]), activeForm: String(m["activeForm"]), status: claudeTodoStatus(String(m["status"]))})
			}
		} else {
			// "#1 [in_progress] Write the parser"
			for _, line := range strings.Split(claudeResultText(result["content"]), "\n") {
				f := strings.SplitN(line, " ", 3)
				if len(f) == 3 && strings.HasPrefix(f[0], "#") && strings.HasPrefix(f[1], "[") && strings.HasSuffix(f[1], "]") {
					listed = append(listed, claudeTodo{id: f[0][1:], content: f[2], status: claudeTodoStatus(strings.Trim(f[1], "[]"))})
				}
			}
		}
		if listed == nil && s == nil {
			return false
		}
		for i := range listed {
			if j := l.index(listed[i].id); j >= 0 && listed[i].activeForm == "" {
				listed[i].activeForm = l.items[j].activeForm
			}
		}
		l.items = listed
		return true
	case "TaskGet":
		m := Map(s["task"])
		i := l.index(String(m["id"]))
		if i < 0 {
			return false
		}
		if subject := String(m["subject"]); subject != "" {
			l.items[i].content = subject
		}
		if status := String(m["status"]); status != "" {
			l.items[i].status = claudeTodoStatus(status)
		}
		if form := String(m["activeForm"]); form != "" {
			l.items[i].activeForm = form
		}
		return true
	}
	return false
}

func (l *claudeTodoList) index(id string) int {
	if id == "" {
		return -1
	}
	for i, t := range l.items {
		if t.id == id {
			return i
		}
	}
	return -1
}

// list is the items as the todoList item carries them (TodoWrite's shape).
func (l *claudeTodoList) list() []any {
	out := make([]any, 0, len(l.items))
	for _, t := range l.items {
		m := map[string]any{"content": t.content, "status": t.status}
		if t.activeForm != "" {
			m["activeForm"] = t.activeForm
		}
		if t.id != "" {
			m["id"] = t.id
		}
		out = append(out, m)
	}
	return out
}

// claudeTodoStatus is a todo status in TodoWrite's words.
func claudeTodoStatus(s string) string {
	switch s {
	case "completed", "done":
		return "completed"
	case "in_progress", "active":
		return "in_progress"
	}
	return "pending"
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
		if t.name == "TaskOutput" {
			// The task's output alone, not the tags the model reads it in.
			if _, out, status := claudeTaskOutput(result, structured); status != "" {
				output = out
			}
		}
	}
	in := t.input
	// A subagent's item names its Agent call; a background task's says so.
	mark := func(item map[string]any) map[string]any {
		if t.parent != "" {
			item["parentId"] = t.parent
		}
		if t.background {
			item["background"] = true
		}
		return item
	}
	switch {
	case t.name == "Bash":
		item := map[string]any{"id": id, "type": "commandExecution", "tool": "Bash", "command": String(in["command"]), "status": status, "aggregatedOutput": output}
		if d := String(in["description"]); d != "" {
			item["description"] = d
		}
		return mark(item)
	case t.name == "Edit" || t.name == "MultiEdit" || t.name == "Write" || t.name == "NotebookEdit":
		return mark(map[string]any{"id": id, "type": "fileChange", "tool": t.name, "status": status, "changes": []any{claudeFileChange(t, structured)}, "output": output})
	case strings.HasPrefix(t.name, "mcp__"):
		server, tool := claudeMCPName(t.name)
		item := map[string]any{"id": id, "type": "mcpToolCall", "server": server, "tool": tool, "arguments": in, "status": status}
		if result != nil {
			item["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": output}}, "isError": status == "failed"}
		}
		return mark(item)
	case t.name == "WebSearch":
		return mark(map[string]any{"id": id, "type": "webSearch", "tool": t.name, "query": String(in["query"]), "status": status, "output": output})
	}
	kind, title, paths, query := claudeToolTitle(t)
	var read map[string]any
	if t.name == "Read" && result != nil && status == "completed" {
		if read = claudeRead(result, structured); read != nil {
			if summary := String(read["summary"]); summary != "" {
				output = summary
			}
			delete(read, "summary")
		}
	}
	item := mark(map[string]any{"id": id, "type": "toolCall", "tool": t.name, "kind": kind, "title": title, "status": status, "output": output, "input": claudeToolInput(in)})
	if read != nil {
		item["read"] = read
	}
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
	case "TaskOutput", "TaskStop", "Monitor":
		// Waiting on, reading or stopping a background task: named by the
		// task's description when the CLI announced it, else by its id.
		task := claudeOr(t.taskTitle, String(in["task_id"]))
		verb := map[string]string{"TaskOutput": "Task output", "TaskStop": "Stop task", "Monitor": "Monitor"}[name]
		if name == "Monitor" && task == "" {
			task = claudeBrief(in)
		}
		return "other", strings.TrimSpace(verb + ": " + task), nil, ""
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

// claudeRead is what a Read of something other than text carried, from
// the CLI's tool_use_result (probed on the pinned CLI, docs/claude-parity.md
// Round 2 E: `{type: image, file: {base64, type, originalSize,
// dimensions}}`, `{type: pdf, file: {filePath, base64, originalSize}}`,
// `{type: notebook, file: {filePath, cells: [{cellType, source, language,
// cell_id}]}}`) and the tool_result's blocks (an `image` block with the
// base64 and media type; a PDF's `document` block): an image's bytes and
// pixel size (the chat service stores the bytes and keeps the stored
// image's id), a PDF's size and page count (counted from its bytes — the
// CLI hands the model the document and reports nothing per page), a
// notebook's cells (type, language, first line) with a listing as the
// output. nil for a text read or anything unrecognised.
func claudeRead(result map[string]any, structured any) map[string]any {
	file := Map(Map(structured)["file"])
	switch String(Map(structured)["type"]) {
	case "image":
		data, media := "", ""
		for _, v := range Array(result["content"]) {
			if b := Map(v); String(b["type"]) == "image" {
				source := Map(b["source"])
				data, media = String(source["data"]), String(source["media_type"])
			}
		}
		if data == "" {
			data, media = String(file["base64"]), String(file["type"])
		}
		if data == "" {
			return nil
		}
		dims := Map(file["dimensions"])
		read := map[string]any{"kind": "image", "data": data, "mediaType": media, "width": claudeInt(dims["originalWidth"]), "height": claudeInt(dims["originalHeight"]), "bytes": claudeInt(file["originalSize"])}
		read["summary"] = claudeImageSummary(read)
		return read
	case "pdf":
		data := String(file["base64"])
		for _, v := range Array(result["content"]) {
			if b := Map(v); String(b["type"]) == "document" {
				if d := String(Map(b["source"])["data"]); d != "" {
					data = d
				}
			}
		}
		pages := claudePDFPages(data)
		size := claudeInt(file["originalSize"])
		if size == 0 {
			size = base64.StdEncoding.DecodedLen(len(data))
		}
		summary := fmt.Sprintf("PDF, %s", claudeSize(size))
		if pages > 0 {
			summary += fmt.Sprintf(", %d page%s", pages, claudePlural(pages))
		}
		summary += "; the model reads the document itself (the CLI returns no text per page)"
		return map[string]any{"kind": "pdf", "bytes": size, "pages": pages, "summary": summary}
	case "notebook":
		cells := []any{}
		var lines []string
		for i, v := range Array(file["cells"]) {
			c := Map(v)
			kind := String(c["cellType"])
			if kind == "" {
				kind = "code"
			}
			first := claudeCut(strings.TrimSpace(strings.SplitN(String(c["source"]), "\n", 2)[0]), 120)
			cell := map[string]any{"type": kind, "text": first}
			label := kind
			if lang := String(c["language"]); lang != "" {
				cell["language"] = lang
				if kind == "code" {
					label += " (" + lang + ")"
				}
			}
			cells = append(cells, cell)
			lines = append(lines, fmt.Sprintf("%d %s: %s", i+1, label, first))
		}
		if len(cells) == 0 {
			return nil
		}
		return map[string]any{"kind": "notebook", "cells": cells, "summary": strings.Join(lines, "\n")}
	}
	return nil
}

// claudeImageSummary is the text an image read shows in place of its
// bytes: the format and pixel size.
func claudeImageSummary(read map[string]any) string {
	media := strings.TrimPrefix(String(read["mediaType"]), "image/")
	if media == "" {
		media = "image"
	}
	summary := strings.ToUpper(media) + " image"
	if w, h := claudeInt(read["width"]), claudeInt(read["height"]); w > 0 && h > 0 {
		summary += fmt.Sprintf(", %d×%d", w, h)
	}
	if n := claudeInt(read["bytes"]); n > 0 {
		summary += ", " + claudeSize(n)
	}
	return summary
}

// claudePDFPagePattern finds page objects in an uncompressed PDF body;
// claudePDFCountPattern the page tree's count. Pages inside compressed
// object streams escape both, and the count is then 0 (not shown).
var (
	claudePDFPagePattern  = regexp.MustCompile(`/Type\s*/Page\b[^s]`)
	claudePDFCountPattern = regexp.MustCompile(`/Type\s*/Pages\b[^>]*?/Count\s+(\d+)`)
)

// claudePDFPages counts a PDF's pages from its base64 bytes, 0 when it
// cannot tell. At most 32 MiB is looked at.
func claudePDFPages(data string) int {
	if len(data) == 0 || len(data) > 32<<20*4/3 {
		return 0
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return 0
	}
	best := 0
	for _, m := range claudePDFCountPattern.FindAllSubmatch(raw, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > best {
			best = n
		}
	}
	if best > 0 {
		return best
	}
	return len(claudePDFPagePattern.FindAllIndex(raw, -1))
}

// claudeSize is a byte count in the transcript's units.
func claudeSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func claudePlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
