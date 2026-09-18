package agent

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestClaudeStreamingTranslation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	done := make(chan Frame, 20)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "saved-claude-session"})
		for _, delta := range []string{"hello ", "world"} {
			_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": delta}}})
		}
		_ = e.Encode(map[string]any{"type": "result", "is_error": false, "result": "hello world", "usage": map[string]any{"input_tokens": 100.0, "cache_read_input_tokens": 900.0, "cache_creation_input_tokens": 50.0, "output_tokens": 40.0}, "total_cost_usd": 0.0125})
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { done <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "hello"}}}); err != nil {
		t.Fatal(err)
	}
	session, reply := "", ""
	var usage map[string]any
	for {
		select {
		case f := <-done:
			if f.Method == "thread/started" {
				session = String(Map(f.Params["thread"])["id"])
			}
			if f.Method == "item/completed" {
				reply = String(Map(f.Params["item"])["text"])
			}
			if f.Method == "thread/tokenUsage/updated" {
				if String(f.Params["threadId"]) != session || String(f.Params["turnId"]) == "" {
					t.Fatalf("usage names the wrong turn: %+v", f.Params)
				}
				usage = Map(Map(f.Params["tokenUsage"])["total"])
			}
			if f.Method == "turn/completed" {
				if session != "saved-claude-session" || reply != "hello world" {
					t.Fatal(session, reply)
				}
				// The turn's usage arrives before its completion, in Codex's
				// shape: input counts the cached tokens too.
				if usage == nil || usage["inputTokens"] != 1050.0 || usage["cachedInputTokens"] != 900.0 || usage["cacheWriteInputTokens"] != 50.0 || usage["outputTokens"] != 40.0 || usage["totalTokens"] != 1090.0 || usage["costUSD"] != 0.0125 {
					t.Fatalf("usage %+v", usage)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("translation timed out")
		}
	}
}

func TestClaudeBuiltinToolAskAndMCPResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	frames := make(chan Frame, 10)
	checked := make(chan bool, 1)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init"}})
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_request", "request_id": "approve", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "input": map[string]any{"file_path": "/workspace/check", "content": "ok"}}})
		if d.Decode(&v) != nil {
			return
		}
		allowed := Map(Map(v["response"])["response"])["behavior"] == "allow" && Map(Map(Map(v["response"])["response"])["updatedInput"])["content"] == "ok"
		_ = e.Encode(map[string]any{"type": "control_request", "request_id": "preview", "request": map[string]any{"subtype": "mcp_message", "server_name": "warden", "message": map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "preview_attach", "arguments": map[string]any{"port": 3000, "path": "/", "title": "Preview"}}}}})
		if d.Decode(&v) != nil {
			return
		}
		result := Map(Map(Map(v["response"])["response"])["mcp_response"])
		checked <- allowed && Map(result["result"])["isError"] == false
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { frames <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Call(ctx, "thread/start", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "write"}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-frames:
		// A built-in tool's ask reaches the controller as a permission
		// request carrying the call typed as its card; the controller's
		// mode answers it (here: accept).
		if f.Method != "item/tool/requestPermission" || f.Params["tool"] != "Write" || Map(f.Params["item"])["type"] != "fileChange" {
			t.Fatal(f)
		}
		_ = c.Reply(f.ID, map[string]any{"decision": "accept"})
	case <-ctx.Done():
		t.Fatal("permission timeout")
	}
	select {
	case f := <-frames:
		if f.Method != "item/tool/call" || f.Params["tool"] != "preview_attach" {
			t.Fatal(f)
		}
		_ = c.Reply(f.ID, map[string]any{"success": true, "contentItems": []any{map[string]any{"type": "inputText", "text": "preview URL"}}})
	case <-ctx.Done():
		t.Fatal("MCP timeout")
	}
	select {
	case ok := <-checked:
		if !ok {
			t.Fatal("incorrect Claude response")
		}
	case <-ctx.Done():
		t.Fatal("response timeout")
	}
}

func TestClaudeTextBlocksAroundToolsAreSeparateItems(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	done := make(chan Frame, 40)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "Looking."}}})
		_ = e.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}}}}})
		_ = e.Encode(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}}})
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "Done."}}})
		_ = e.Encode(map[string]any{"type": "result", "is_error": false, "result": "Done."})
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { done <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "go"}}}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for {
		select {
		case f := <-done:
			item := Map(f.Params["item"])
			if f.Method == "item/completed" {
				order = append(order, String(item["type"])+":"+String(item["id"])+":"+String(item["text"]))
			}
			if f.Method == "turn/completed" {
				if len(order) != 3 || order[0][:len("agentMessage:")] != "agentMessage:" || order[1] != "commandExecution:toolu_1:" || order[2][:len("agentMessage:")] != "agentMessage:" {
					t.Fatalf("expected text, tool, text: %v", order)
				}
				first, second := order[0], order[2]
				if first[len(first)-len("Looking."):] != "Looking." || second[len(second)-len("Done."):] != "Done." || first[:len(first)-len("Looking.")] == second[:len(second)-len("Done.")] {
					t.Fatalf("text blocks must be separate items with their own text: %v", order)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("translation timed out")
		}
	}
}

// An image attachment reaches Claude as an image content block when its
// bytes came along; a path-only localImage adds nothing (the text names it).
func TestClaudeTurnInputImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	user := make(chan map[string]any, 1)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		for d.Decode(&v) == nil {
			if v["type"] == "user" {
				user <- v
				return
			}
		}
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(*Client, Frame) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	input := []any{map[string]any{"type": "text", "text": "see"}, map[string]any{"type": "localImage", "path": "/w/a.png", "data": "aGk="}, map[string]any{"type": "localImage", "path": "/w/b.png"}}
	if _, err = c.Call(ctx, "turn/start", map[string]any{"input": input}); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-user:
		content := Array(Map(v["message"])["content"])
		if len(content) != 2 || Map(content[0])["text"] != "see" || Map(content[1])["type"] != "image" || Map(Map(content[1])["source"])["data"] != "aGk=" || Map(Map(content[1])["source"])["media_type"] != "image/png" {
			t.Fatalf("%v", content)
		}
	case <-ctx.Done():
		t.Fatal("no user message reached the CLI")
	}
}

func TestClaudeTurnUsageIsGrowthOfTheRunningCost(t *testing.T) {
	sofar := claudeUsage{input: 10, cost: 0.01}
	u, ok := claudeTurnUsage(map[string]any{"usage": map[string]any{"input_tokens": 100.0, "cache_read_input_tokens": 900.0, "output_tokens": 40.0, "output_tokens_details": map[string]any{"thinking_tokens": 15.0}}, "total_cost_usd": 0.03}, sofar)
	if !ok || u.input != 1000 || u.cached != 900 || u.cacheWrite != 0 || u.output != 40 || u.reasoning != 15 || u.cost < 0.0199 || u.cost > 0.0201 {
		t.Fatalf("usage %+v", u)
	}
	// A crash result carries no usage; a cost that did not grow adds none.
	if _, ok = claudeTurnUsage(map[string]any{"is_error": true}, sofar); ok {
		t.Fatal("usage from a result without one")
	}
	u, _ = claudeTurnUsage(map[string]any{"usage": map[string]any{"output_tokens": 1.0}, "total_cost_usd": 0.01}, sofar)
	if u.cost != 0 {
		t.Fatalf("cost %v", u.cost)
	}
	if p := sofar.add(u).params(); p["totalTokens"] != int64(11) || p["costUSD"] != 0.01 {
		t.Fatalf("params %+v", p)
	}
}

// A thinking block streams as a reasoning item in Codex's shape: started at
// the block, its text by delta, completed with the whole at the block's
// stop; a second block (before a tool, say) is a separate item, and the
// thinking never leaks into the message text.
func TestClaudeThinkingBlocksAreReasoningItems(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	done := make(chan Frame, 40)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}}})
		for _, delta := range []string{"Let me ", "look."} {
			_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": delta}}})
		}
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_stop", "index": 0}})
		_ = e.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "thinking", "thinking": "Let me look."}, map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}}}}})
		_ = e.Encode(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}}})
		// A block whose stop never arrives ends with the message.
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "Now answer."}}})
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}}})
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "Done."}}})
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_stop", "index": 1}})
		_ = e.Encode(map[string]any{"type": "result", "is_error": false, "result": "Done."})
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { done <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "go"}}}); err != nil {
		t.Fatal(err)
	}
	var started, completed, deltas []string
	for {
		select {
		case f := <-done:
			item := Map(f.Params["item"])
			switch f.Method {
			case "item/started":
				started = append(started, String(item["type"])+":"+String(item["id"]))
			case "item/reasoning/summaryTextDelta":
				deltas = append(deltas, String(f.Params["itemId"])+":"+String(f.Params["delta"]))
			case "item/completed":
				summary := ""
				for _, s := range Array(item["summary"]) {
					summary += String(s)
				}
				completed = append(completed, String(item["type"])+":"+String(item["id"])+":"+summary+String(item["text"]))
			case "turn/completed":
				if len(started) != 4 || started[0][:10] != "reasoning:" || started[1] != "commandExecution:toolu_1" || started[2][:10] != "reasoning:" || started[3][:13] != "agentMessage:" {
					t.Fatalf("started %v", started)
				}
				r1, r2 := started[0][10:], started[2][10:]
				if r1 == r2 {
					t.Fatal("each thinking block is its own item")
				}
				if len(deltas) != 3 || deltas[0] != r1+":Let me " || deltas[1] != r1+":look." || deltas[2] != r2+":Now answer." {
					t.Fatalf("deltas %v", deltas)
				}
				if len(completed) != 4 || completed[0] != "reasoning:"+r1+":Let me look." || completed[1] != "commandExecution:toolu_1:" || completed[2] != "reasoning:"+r2+":Now answer." || completed[3][len(completed[3])-5:] != "Done." {
					t.Fatalf("completed %v", completed)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("translation timed out")
		}
	}
}

// A turn/interrupt becomes the CLI's interrupt control request; the result
// that follows the abort ends the turn as interrupted, keeping whatever text
// streamed before it and raising no error, and the process stays up for the
// next turn.
func TestClaudeInterruptEndsTurnAsInterrupted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	done := make(chan Frame, 40)
	interrupts := make(chan map[string]any, 1)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Working on"}}})
		if d.Decode(&v) != nil {
			return
		}
		interrupts <- v
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{}}})
		// What the CLI reports for an aborted query: an error result.
		_ = e.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "Request was aborted."})
		// The next turn runs on the same process.
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "result", "is_error": false, "result": "Again."})
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { done <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	started, err := c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	turnID := String(Map(started["turn"])["id"])
	for {
		f := <-done
		if f.Method == "item/agentMessage/delta" {
			break
		}
	}
	if _, err = c.Call(ctx, "turn/interrupt", map[string]any{"turnId": turnID}); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-interrupts:
		if v["type"] != "control_request" || Map(v["request"])["subtype"] != "interrupt" {
			t.Fatalf("interrupt not forwarded as the SDK control request: %v", v)
		}
	case <-ctx.Done():
		t.Fatal("interrupt not forwarded")
	}
	var text string
	for {
		select {
		case f := <-done:
			switch f.Method {
			case "error":
				t.Fatal("an interrupted turn is not an error")
			case "item/completed":
				text = String(Map(f.Params["item"])["text"])
			case "turn/completed":
				turn := Map(f.Params["turn"])
				if turn["id"] != turnID || turn["status"] != "interrupted" {
					t.Fatalf("turn ended %v", turn)
				}
				if text != "Working on" {
					t.Fatalf("streamed text lost: %q", text)
				}
				goto next
			}
		case <-ctx.Done():
			t.Fatal("interrupted turn never completed")
		}
	}
next:
	if _, err = c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "again"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case f := <-done:
			if f.Method == "turn/completed" {
				if Map(f.Params["turn"])["status"] != "completed" {
					t.Fatalf("next turn ended %v", f.Params)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("next turn never completed")
		}
	}
}

// Each Claude tool call is a typed item: Bash a command execution with its
// real command and output, the file tools a file change with a unified
// diff, a Warden MCP tool an MCP call, WebSearch Codex's web search, and
// the rest a toolCall with a kind and a title in the tool's own terms.
func TestClaudeToolItems(t *testing.T) {
	in := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	ok := func(content any) map[string]any { return map[string]any{"type": "tool_result", "content": content} }
	failed := func(content any) map[string]any {
		return map[string]any{"type": "tool_result", "content": content, "is_error": true}
	}
	ws := "/home/agent/workspace/"

	// Bash: the command, its description, the output; a failure by is_error.
	bash := claudeTool{"Bash", in("command", "ls -la", "description", "List files")}
	item := claudeToolItem("t1", bash, nil, nil)
	if item["type"] != "commandExecution" || item["command"] != "ls -la" || item["description"] != "List files" || item["status"] != "running" || item["aggregatedOutput"] != "" {
		t.Fatalf("bash started: %v", item)
	}
	item = claudeToolItem("t1", bash, ok("total 0\nfile"), map[string]any{"stdout": "total 0\nfile", "stderr": ""})
	if item["status"] != "completed" || item["aggregatedOutput"] != "total 0\nfile" {
		t.Fatalf("bash completed: %v", item)
	}
	item = claudeToolItem("t1", claudeTool{"Bash", in("command", "false")}, failed("Exit code 1"), "Error: Exit code 1")
	if item["status"] != "failed" || item["aggregatedOutput"] != "Exit code 1" || item["description"] != nil {
		t.Fatalf("bash failed: %v", item)
	}

	// Edit: at the start a headerless hunk of the old and new text (the
	// line is not known); the structured result brings the CLI's own
	// numbered hunks. Paths read relative to the workspace.
	edit := claudeTool{"Edit", in("file_path", ws+"notes.txt", "old_string", "gamma", "new_string", "GAMMA\nGAMMA2")}
	item = claudeToolItem("t2", edit, nil, nil)
	change := Map(Array(item["changes"])[0])
	if item["type"] != "fileChange" || item["tool"] != "Edit" || change["path"] != "notes.txt" || change["kind"] != "update" {
		t.Fatalf("edit started: %v", item)
	}
	if diff := String(change["diff"]); diff != "diff --git a/notes.txt b/notes.txt\n--- a/notes.txt\n+++ b/notes.txt\n-gamma\n+GAMMA\n+GAMMA2\n" {
		t.Fatalf("edit diff:\n%s", diff)
	}
	structured := map[string]any{"filePath": ws + "notes.txt", "structuredPatch": []any{map[string]any{"oldStart": 1.0, "oldLines": 4.0, "newStart": 1.0, "newLines": 5.0, "lines": []any{" alpha", " beta", "-gamma", "+GAMMA", "+GAMMA2", " delta"}}}}
	item = claudeToolItem("t2", edit, ok("The file has been updated successfully."), structured)
	change = Map(Array(item["changes"])[0])
	if diff := String(change["diff"]); item["status"] != "completed" || diff != "diff --git a/notes.txt b/notes.txt\n--- a/notes.txt\n+++ b/notes.txt\n@@ -1,4 +1,5 @@\n alpha\n beta\n-gamma\n+GAMMA\n+GAMMA2\n delta\n" {
		t.Fatalf("edit completed:\n%s", diff)
	}
	// A failed edit keeps the asked-for diff and the tool's message,
	// unwrapped from the CLI's error tag.
	item = claudeToolItem("t2", edit, failed("<tool_use_error>File has not been read yet.</tool_use_error>"), "Error: File has not been read yet.")
	if item["status"] != "failed" || item["output"] != "File has not been read yet." || !strings.Contains(String(Map(Array(item["changes"])[0])["diff"]), "-gamma\n+GAMMA\n") {
		t.Fatalf("edit failed: %v", item)
	}

	// MultiEdit: one hunk per edit, headerless.
	multi := claudeTool{"MultiEdit", in("file_path", ws+"a.go", "edits", []any{in("old_string", "x", "new_string", "y"), in("old_string", "p\nq", "new_string", "")})}
	item = claudeToolItem("t3", multi, nil, nil)
	if diff := String(Map(Array(item["changes"])[0])["diff"]); !strings.HasSuffix(diff, "+++ b/a.go\n-x\n+y\n-p\n-q\n") {
		t.Fatalf("multi-edit diff:\n%s", diff)
	}

	// Write: the content as an added file with numbered lines; the result
	// says whether the file was created (new file) or replaced (then the
	// CLI's patch against the old content).
	write := claudeTool{"Write", in("file_path", ws+"hello.txt", "content", "hello\nworld\n")}
	item = claudeToolItem("t4", write, nil, nil)
	change = Map(Array(item["changes"])[0])
	if diff := String(change["diff"]); change["kind"] != "add" || diff != "diff --git a/hello.txt b/hello.txt\nnew file mode 100644\n--- /dev/null\n+++ b/hello.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\n" {
		t.Fatalf("write started:\n%s", diff)
	}
	item = claudeToolItem("t4", write, ok("File created successfully at: "+ws+"hello.txt"), map[string]any{"type": "create", "structuredPatch": []any{}})
	if change = Map(Array(item["changes"])[0]); change["kind"] != "add" || !strings.Contains(String(change["diff"]), "new file mode") {
		t.Fatalf("write created: %v", change)
	}
	item = claudeToolItem("t4", write, ok("The file has been updated successfully."), map[string]any{"type": "update", "structuredPatch": []any{map[string]any{"oldStart": 1.0, "oldLines": 1.0, "newStart": 1.0, "newLines": 2.0, "lines": []any{"-old", "+hello", "+world"}}}})
	change = Map(Array(item["changes"])[0])
	if diff := String(change["diff"]); change["kind"] != "update" || diff != "diff --git a/hello.txt b/hello.txt\n--- a/hello.txt\n+++ b/hello.txt\n@@ -1,1 +1,2 @@\n-old\n+hello\n+world\n" {
		t.Fatalf("write replaced:\n%s", diff)
	}

	// NotebookEdit: the new source as added lines under the cell.
	item = claudeToolItem("t5", claudeTool{"NotebookEdit", in("notebook_path", ws+"nb.ipynb", "cell_id", "c3", "new_source", "print(1)")}, nil, nil)
	if diff := String(Map(Array(item["changes"])[0])["diff"]); item["tool"] != "NotebookEdit" || !strings.HasSuffix(diff, "+++ b/nb.ipynb\n@@ cell c3\n+print(1)\n") {
		t.Fatalf("notebook diff:\n%s", diff)
	}

	// A Warden MCP tool: server and tool from the name, the arguments, the
	// result's text blocks.
	mcp := claudeTool{"mcp__warden__preview_attach", in("port", 3000.0, "title", "Preview")}
	item = claudeToolItem("t6", mcp, nil, nil)
	if item["type"] != "mcpToolCall" || item["server"] != "warden" || item["tool"] != "preview_attach" || Map(item["arguments"])["title"] != "Preview" || item["result"] != nil {
		t.Fatalf("mcp started: %v", item)
	}
	item = claudeToolItem("t6", mcp, ok([]any{map[string]any{"type": "text", "text": "Preview at https://p.example"}}), nil)
	if content := Array(Map(item["result"])["content"]); Map(content[0])["text"] != "Preview at https://p.example" || Map(item["result"])["isError"] != false {
		t.Fatalf("mcp completed: %v", item)
	}

	// The generic kinds: read, search, fetch, task, other.
	cases := []struct {
		tool                     claudeTool
		kind, title, path, query string
	}{
		{claudeTool{"Read", in("file_path", ws+"chat/main.go")}, "read", "Read chat/main.go", "chat/main.go", ""},
		{claudeTool{"Read", in("file_path", "/etc/hosts", "offset", 10.0, "limit", 20.0)}, "read", "Read /etc/hosts (lines 10–29)", "/etc/hosts", ""},
		{claudeTool{"Grep", in("pattern", "func main", "path", ws+"chat", "glob", "*.go")}, "search", "Grep 'func main' in chat *.go", "chat", "func main"},
		{claudeTool{"Grep", in("pattern", "x")}, "search", "Grep 'x' in .", ".", "x"},
		{claudeTool{"Glob", in("pattern", "**/*.ts")}, "search", "Glob '**/*.ts' in .", ".", "**/*.ts"},
		{claudeTool{"LS", in("path", ws)}, "search", "List .", ".", ""},
		{claudeTool{"WebFetch", in("url", "https://example.com/x", "prompt", "title?")}, "fetch", "Fetch https://example.com/x", "", "https://example.com/x"},
		{claudeTool{"Agent", in("description", "list files", "subagent_type", "Explore", "prompt", "…")}, "task", "Agent: list files (Explore)", "", ""},
		{claudeTool{"Task", in("description", "list files")}, "task", "Agent: list files", "", ""},
		{claudeTool{"TodoWrite", in("todos", []any{in("content", "a"), in("content", "b")})}, "other", "Update todos (2)", "", ""},
		{claudeTool{"Skill", in("skill", "deploy", "args", "prod")}, "other", "Skill /deploy prod", "", ""},
		{claudeTool{"ToolSearch", in("query", "select:Foo")}, "other", "ToolSearch 'select:Foo'", "", "select:Foo"},
		{claudeTool{"AskUserQuestion", in("questions", []any{in("question", "Which?")})}, "other", "Question: Which?", "", ""},
		{claudeTool{"Monitor", in("description", "wait for the build", "command", "sleep 1")}, "other", "Monitor wait for the build", "", ""},
		{claudeTool{"", nil}, "other", "tool", "", ""},
	}
	for _, c := range cases {
		item = claudeToolItem("id", c.tool, nil, nil)
		paths := Array(item["paths"])
		path := ""
		if len(paths) > 0 {
			path = String(paths[0])
		}
		if item["type"] != "toolCall" || item["kind"] != c.kind || item["title"] != c.title || path != c.path || String(item["query"]) != c.query || item["status"] != "running" {
			t.Fatalf("%s: %v", c.tool.name, item)
		}
	}
	item = claudeToolItem("id", claudeTool{"Read", in("file_path", ws+"x")}, ok("1\talpha\n2\tbeta"), map[string]any{"type": "text"})
	if item["status"] != "completed" || item["output"] != "1\talpha\n2\tbeta" {
		t.Fatalf("read completed: %v", item)
	}
	item = claudeToolItem("id", claudeTool{"Read", in("file_path", ws+"x")}, failed("File does not exist."), nil)
	if item["status"] != "failed" || item["output"] != "File does not exist." {
		t.Fatalf("read failed: %v", item)
	}
	// A search's content blocks join; an image block is named; a long
	// input string is cut on the generic item.
	item = claudeToolItem("id", claudeTool{"Agent", in("prompt", strings.Repeat("p", 3000))}, ok([]any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "image"}, map[string]any{"type": "text", "text": "b"}}), nil)
	if item["output"] != "a\n[image]\nb" || len([]rune(String(Map(item["input"])["prompt"]))) != 2001 {
		t.Fatalf("content blocks: %v", item)
	}
	item = claudeToolItem("id", claudeTool{"WebSearch", in("query", "warden sandbox")}, ok("results…"), nil)
	if item["type"] != "webSearch" || item["query"] != "warden sandbox" || item["output"] != "results…" {
		t.Fatalf("web search: %v", item)
	}
}

// Through the stream: a tool call starts as its typed item when the
// assistant message names it and completes with its result, taking the
// frame's structured result only when the frame carries one result.
func TestClaudeToolItemsThroughTheStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	done := make(chan Frame, 40)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Edit", "input": map[string]any{"file_path": "/home/agent/workspace/n.txt", "old_string": "a", "new_string": "b"}}}}})
		_ = e.Encode(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}}, "tool_use_result": map[string]any{"structuredPatch": []any{map[string]any{"oldStart": 3.0, "oldLines": 1.0, "newStart": 3.0, "newLines": 1.0, "lines": []any{"-a", "+b"}}}}})
		// Two results in one frame: neither owns the frame's structured result.
		_ = e.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "toolu_2", "name": "Read", "input": map[string]any{"file_path": "/home/agent/workspace/x"}}, map[string]any{"type": "tool_use", "id": "toolu_3", "name": "Bash", "input": map[string]any{"command": "pwd"}}}}})
		_ = e.Encode(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": "1\tx"}, map[string]any{"type": "tool_result", "tool_use_id": "toolu_3", "content": "/home/agent/workspace"}}}, "tool_use_result": map[string]any{"structuredPatch": []any{}}})
		_ = e.Encode(map[string]any{"type": "result", "is_error": false, "result": "done"})
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { done <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "go"}}}); err != nil {
		t.Fatal(err)
	}
	started, completed := map[string]map[string]any{}, map[string]map[string]any{}
	for {
		select {
		case f := <-done:
			item := Map(f.Params["item"])
			switch f.Method {
			case "item/started":
				started[String(item["id"])] = item
			case "item/completed":
				completed[String(item["id"])] = item
			case "turn/completed":
				if s := started["toolu_1"]; s["type"] != "fileChange" || s["status"] != "running" || !strings.HasSuffix(String(Map(Array(s["changes"])[0])["diff"]), "+++ b/n.txt\n-a\n+b\n") {
					t.Fatalf("edit started: %v", s)
				}
				if d := completed["toolu_1"]; d["status"] != "completed" || !strings.Contains(String(Map(Array(d["changes"])[0])["diff"]), "@@ -3,1 +3,1 @@\n-a\n+b\n") {
					t.Fatalf("edit completed: %v", d)
				}
				if r := completed["toolu_2"]; r["type"] != "toolCall" || r["kind"] != "read" || r["title"] != "Read x" || r["output"] != "1\tx" {
					t.Fatalf("read: %v", r)
				}
				if b := completed["toolu_3"]; b["type"] != "commandExecution" || b["command"] != "pwd" || b["aggregatedOutput"] != "/home/agent/workspace" || b["status"] != "completed" {
					t.Fatalf("bash: %v", b)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("translation timed out")
		}
	}
}

// claudeFake is a scripted CLI on the far side of the adapter: it answers
// the initialize handshake, then hands every frame the adapter writes to
// `writes` and encodes whatever the test sends.
type claudeFake struct {
	enc    *json.Encoder
	writes chan map[string]any
}

func newClaudeFake(t *testing.T, ctx context.Context) (*Client, *claudeFake, chan Frame) {
	t.Helper()
	raw, fake := net.Pipe()
	t.Cleanup(func() { fake.Close() })
	cf := &claudeFake{enc: json.NewEncoder(fake), writes: make(chan map[string]any, 32)}
	go func() {
		d := json.NewDecoder(fake)
		for {
			var v map[string]any
			if d.Decode(&v) != nil {
				return
			}
			if v["type"] == "control_request" && Map(v["request"])["subtype"] == "initialize" {
				_ = cf.enc.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{}}})
				continue
			}
			select {
			case cf.writes <- v:
			case <-ctx.Done():
				return
			}
		}
	}()
	frames := make(chan Frame, 32)
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { frames <- f })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	return c, cf, frames
}

func (cf *claudeFake) send(v map[string]any) { _ = cf.enc.Encode(v) }

func (cf *claudeFake) next(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	select {
	case v := <-cf.writes:
		return v
	case <-ctx.Done():
		t.Fatal("the adapter wrote nothing")
	}
	return nil
}

func nextFrame(t *testing.T, ctx context.Context, frames chan Frame, method string) Frame {
	t.Helper()
	for {
		select {
		case f := <-frames:
			if f.Method == method {
				return f
			}
		case <-ctx.Done():
			t.Fatalf("no %s frame", method)
		}
	}
}

// A tool ask is the controller's to answer: it arrives typed with the
// call's card, and a decline carries the controller's message to the CLI
// as the tool's error text.
func TestClaudePermissionAskDeniedWithMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, cf, frames := newClaudeFake(t, ctx)
	if _, err := c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "go"}}}); err != nil {
		t.Fatal(err)
	}
	cf.next(t, ctx) // the user message
	cf.send(map[string]any{"type": "control_request", "request_id": "r1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "tool_use_id": "toolu_9", "description": "Create a file", "input": map[string]any{"command": "touch x", "description": "Create a file"}, "permission_suggestions": []any{}}})
	f := nextFrame(t, ctx, frames, "item/tool/requestPermission")
	item := Map(f.Params["item"])
	if f.Params["tool"] != "Bash" || Map(f.Params["input"])["command"] != "touch x" || f.Params["description"] != "Create a file" || item["type"] != "commandExecution" || item["command"] != "touch x" || item["id"] != "toolu_9" {
		t.Fatalf("%+v", f.Params)
	}
	if err := c.Reply(f.ID, map[string]any{"decision": "decline", "message": "use printf"}); err != nil {
		t.Fatal(err)
	}
	v := cf.next(t, ctx)
	r := Map(v["response"])
	answer := Map(r["response"])
	if v["type"] != "control_response" || r["request_id"] != "r1" || answer["behavior"] != "deny" || answer["updatedPermissions"] != nil {
		t.Fatalf("%+v", v)
	}
	// The message rides in the CLI's own rejection wording, so the model
	// takes it as the user's decision rather than the tool's output.
	if msg := String(answer["message"]); !strings.HasPrefix(msg, "The user doesn't want to proceed with this tool use.") || !strings.HasSuffix(msg, "the user said: use printf") {
		t.Fatal(msg)
	}
	if msg := claudeDenial("Bash", ""); !strings.HasSuffix(msg, "wait for the user to tell you how to proceed.") {
		t.Fatal(msg)
	}
	if msg := claudeDenial("ExitPlanMode", "add tests"); !strings.Contains(msg, "Stay in plan mode") || !strings.HasSuffix(msg, "the user said: add tests") {
		t.Fatal(msg)
	}
}

// ExitPlanMode is an ask with the plan; approving it into a mode moves
// the CLI's mode with the answer.
func TestClaudeExitPlanModeApprovalSetsMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, cf, frames := newClaudeFake(t, ctx)
	if _, err := c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "go"}}}); err != nil {
		t.Fatal(err)
	}
	cf.next(t, ctx)
	cf.send(map[string]any{"type": "control_request", "request_id": "r2", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "ExitPlanMode", "tool_use_id": "toolu_p", "input": map[string]any{"plan": "# Plan\n1. edit", "planFilePath": "/home/agent/.claude/plans/p.md"}}})
	f := nextFrame(t, ctx, frames, "item/tool/requestPermission")
	if f.Params["tool"] != "ExitPlanMode" || f.Params["plan"] != "# Plan\n1. edit" {
		t.Fatalf("%+v", f.Params)
	}
	if err := c.Reply(f.ID, map[string]any{"decision": "accept", "mode": "ask"}); err != nil {
		t.Fatal(err)
	}
	answer := Map(Map(cf.next(t, ctx)["response"])["response"])
	updates := Array(answer["updatedPermissions"])
	if answer["behavior"] != "allow" || Map(answer["updatedInput"])["plan"] != "# Plan\n1. edit" || len(updates) != 1 || Map(updates[0])["type"] != "setMode" || Map(updates[0])["mode"] != "default" || Map(updates[0])["destination"] != "session" {
		t.Fatalf("%+v", answer)
	}
	// The CLI now reports default; asking for ask again sends nothing,
	// plan sends set_permission_mode.
	if _, err := c.Call(ctx, "permissions/set", map[string]any{"mode": "ask"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		v := cf.next(t, ctx)
		if Map(v["request"])["subtype"] != "set_permission_mode" || Map(v["request"])["mode"] != "plan" {
			t.Errorf("%+v", v)
		}
		cf.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{"mode": "plan"}}})
	}()
	result, err := c.Call(ctx, "permissions/set", map[string]any{"mode": "plan"})
	if err != nil || result["mode"] != "plan" {
		t.Fatal(result, err)
	}
	select {
	case v := <-cf.writes:
		t.Fatalf("unexpected write %+v", v)
	case <-time.After(50 * time.Millisecond):
	}
}

// A mode the CLI refuses fails the call; the CLI's own status frames
// reach the controller as mode changes.
func TestClaudeModeRefusedAndStatusReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, cf, frames := newClaudeFake(t, ctx)
	go func() {
		v := cf.next(t, ctx)
		cf.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": v["request_id"], "error": "Cannot set permission mode"}})
	}()
	if _, err := c.Call(ctx, "permissions/set", map[string]any{"mode": "plan"}); err == nil || !strings.Contains(err.Error(), "Cannot set permission mode") {
		t.Fatal(err)
	}
	cf.send(map[string]any{"type": "system", "subtype": "status", "status": nil, "permissionMode": "plan"})
	f := nextFrame(t, ctx, frames, "permissions/modeChanged")
	if f.Params["mode"] != "plan" {
		t.Fatal(f)
	}
	// The CLI is in plan mode now: asking for plan sends nothing more.
	if r, err := c.Call(ctx, "permissions/set", map[string]any{"mode": "plan"}); err != nil || r["mode"] != "plan" {
		t.Fatal(r, err)
	}
	select {
	case v := <-cf.writes:
		t.Fatalf("unexpected write %+v", v)
	case <-time.After(50 * time.Millisecond):
	}
}
