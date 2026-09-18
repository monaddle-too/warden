package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if !claudeNext(d, &v) {
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
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init"}})
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_request", "request_id": "approve", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "input": map[string]any{"file_path": "/workspace/check", "content": "ok"}}})
		if !claudeNext(d, &v) {
			return
		}
		allowed := Map(Map(v["response"])["response"])["behavior"] == "allow" && Map(Map(Map(v["response"])["response"])["updatedInput"])["content"] == "ok"
		_ = e.Encode(map[string]any{"type": "control_request", "request_id": "preview", "request": map[string]any{"subtype": "mcp_message", "server_name": "warden", "message": map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "preview_attach", "arguments": map[string]any{"port": 3000, "path": "/", "title": "Preview"}}}}})
		if !claudeNext(d, &v) {
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
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if !claudeNext(d, &v) {
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
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		for claudeNext(d, &v) {
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
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if !claudeNext(d, &v) {
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
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Working on"}}})
		if !claudeNext(d, &v) {
			return
		}
		interrupts <- v
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{}}})
		// What the CLI reports for an aborted query: an error result.
		_ = e.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "Request was aborted."})
		// The next turn runs on the same process.
		if !claudeNext(d, &v) {
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
	bash := claudeTool{name: "Bash", input: in("command", "ls -la", "description", "List files")}
	item := claudeToolItem("t1", bash, nil, nil)
	if item["type"] != "commandExecution" || item["command"] != "ls -la" || item["description"] != "List files" || item["status"] != "running" || item["aggregatedOutput"] != "" {
		t.Fatalf("bash started: %v", item)
	}
	item = claudeToolItem("t1", bash, ok("total 0\nfile"), map[string]any{"stdout": "total 0\nfile", "stderr": ""})
	if item["status"] != "completed" || item["aggregatedOutput"] != "total 0\nfile" {
		t.Fatalf("bash completed: %v", item)
	}
	item = claudeToolItem("t1", claudeTool{name: "Bash", input: in("command", "false")}, failed("Exit code 1"), "Error: Exit code 1")
	if item["status"] != "failed" || item["aggregatedOutput"] != "Exit code 1" || item["description"] != nil {
		t.Fatalf("bash failed: %v", item)
	}

	// Edit: at the start a headerless hunk of the old and new text (the
	// line is not known); the structured result brings the CLI's own
	// numbered hunks. Paths read relative to the workspace.
	edit := claudeTool{name: "Edit", input: in("file_path", ws+"notes.txt", "old_string", "gamma", "new_string", "GAMMA\nGAMMA2")}
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
	multi := claudeTool{name: "MultiEdit", input: in("file_path", ws+"a.go", "edits", []any{in("old_string", "x", "new_string", "y"), in("old_string", "p\nq", "new_string", "")})}
	item = claudeToolItem("t3", multi, nil, nil)
	if diff := String(Map(Array(item["changes"])[0])["diff"]); !strings.HasSuffix(diff, "+++ b/a.go\n-x\n+y\n-p\n-q\n") {
		t.Fatalf("multi-edit diff:\n%s", diff)
	}

	// Write: the content as an added file with numbered lines; the result
	// says whether the file was created (new file) or replaced (then the
	// CLI's patch against the old content).
	write := claudeTool{name: "Write", input: in("file_path", ws+"hello.txt", "content", "hello\nworld\n")}
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
	item = claudeToolItem("t5", claudeTool{name: "NotebookEdit", input: in("notebook_path", ws+"nb.ipynb", "cell_id", "c3", "new_source", "print(1)")}, nil, nil)
	if diff := String(Map(Array(item["changes"])[0])["diff"]); item["tool"] != "NotebookEdit" || !strings.HasSuffix(diff, "+++ b/nb.ipynb\n@@ cell c3\n+print(1)\n") {
		t.Fatalf("notebook diff:\n%s", diff)
	}

	// A Warden MCP tool: server and tool from the name, the arguments, the
	// result's text blocks.
	mcp := claudeTool{name: "mcp__warden__preview_attach", input: in("port", 3000.0, "title", "Preview")}
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
		{claudeTool{name: "Read", input: in("file_path", ws+"chat/main.go")}, "read", "Read chat/main.go", "chat/main.go", ""},
		{claudeTool{name: "Read", input: in("file_path", "/etc/hosts", "offset", 10.0, "limit", 20.0)}, "read", "Read /etc/hosts (lines 10–29)", "/etc/hosts", ""},
		{claudeTool{name: "Grep", input: in("pattern", "func main", "path", ws+"chat", "glob", "*.go")}, "search", "Grep 'func main' in chat *.go", "chat", "func main"},
		{claudeTool{name: "Grep", input: in("pattern", "x")}, "search", "Grep 'x' in .", ".", "x"},
		{claudeTool{name: "Glob", input: in("pattern", "**/*.ts")}, "search", "Glob '**/*.ts' in .", ".", "**/*.ts"},
		{claudeTool{name: "LS", input: in("path", ws)}, "search", "List .", ".", ""},
		{claudeTool{name: "WebFetch", input: in("url", "https://example.com/x", "prompt", "title?")}, "fetch", "Fetch https://example.com/x", "", "https://example.com/x"},
		{claudeTool{name: "Agent", input: in("description", "list files", "subagent_type", "Explore", "prompt", "…")}, "task", "Agent: list files (Explore)", "", ""},
		{claudeTool{name: "Task", input: in("description", "list files")}, "task", "Agent: list files", "", ""},
		{claudeTool{name: "TodoWrite", input: in("todos", []any{in("content", "a"), in("content", "b")})}, "other", "Update todos (2)", "", ""},
		{claudeTool{name: "Skill", input: in("skill", "deploy", "args", "prod")}, "other", "Skill /deploy prod", "", ""},
		{claudeTool{name: "ToolSearch", input: in("query", "select:Foo")}, "other", "ToolSearch 'select:Foo'", "", "select:Foo"},
		{claudeTool{name: "AskUserQuestion", input: in("questions", []any{in("question", "Which?")})}, "other", "Question: Which?", "", ""},
		{claudeTool{name: "Monitor", input: in("description", "wait for the build", "command", "sleep 1")}, "other", "Monitor: wait for the build", "", ""},
		{claudeTool{name: "Monitor", input: in("task_id", "b1"), taskTitle: "Run the tests"}, "other", "Monitor: Run the tests", "", ""},
		{claudeTool{name: "TaskOutput", input: in("task_id", "b1", "block", true), taskTitle: "Run the tests"}, "other", "Task output: Run the tests", "", ""},
		{claudeTool{name: "TaskOutput", input: in("task_id", "b1")}, "other", "Task output: b1", "", ""},
		{claudeTool{name: "TaskStop", input: in("task_id", "b1"), taskTitle: "Run the tests"}, "other", "Stop task: Run the tests", "", ""},
		{claudeTool{}, "other", "tool", "", ""},
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
	item = claudeToolItem("id", claudeTool{name: "Read", input: in("file_path", ws+"x")}, ok("1\talpha\n2\tbeta"), map[string]any{"type": "text"})
	if item["status"] != "completed" || item["output"] != "1\talpha\n2\tbeta" {
		t.Fatalf("read completed: %v", item)
	}
	item = claudeToolItem("id", claudeTool{name: "Read", input: in("file_path", ws+"x")}, failed("File does not exist."), nil)
	if item["status"] != "failed" || item["output"] != "File does not exist." {
		t.Fatalf("read failed: %v", item)
	}
	// A search's content blocks join; an image block is named; a long
	// input string is cut on the generic item.
	item = claudeToolItem("id", claudeTool{name: "Agent", input: in("prompt", strings.Repeat("p", 3000))}, ok([]any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "image"}, map[string]any{"type": "text", "text": "b"}}), nil)
	if item["output"] != "a\n[image]\nb" || len([]rune(String(Map(item["input"])["prompt"]))) != 2001 {
		t.Fatalf("content blocks: %v", item)
	}
	item = claudeToolItem("id", claudeTool{name: "WebSearch", input: in("query", "warden sandbox")}, ok("results…"), nil)
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
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if !claudeNext(d, &v) {
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
			if !claudeNext(d, &v) {
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

// claudeSession runs the adapter against a fake CLI: after the handshake
// and the first turn's message, `script` writes the CLI's frames. Every
// notification the adapter sends is collected until `until` frames of
// `turn/completed` have arrived (or the context ends).
func claudeSession(t *testing.T, script func(e *json.Encoder), until int) []Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	done := make(chan Frame, 200)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		script(e)
		// Keep the pipe open until the adapter is done reading.
		<-ctx.Done()
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
	var frames []Frame
	completed := 0
	for completed < until {
		select {
		case f := <-done:
			frames = append(frames, f)
			if f.Method == "turn/completed" {
				completed++
			}
		case <-ctx.Done():
			t.Fatalf("timed out after %d frames: %s", len(frames), claudeFrameLog(frames))
		}
	}
	return frames
}

func claudeFrameLog(frames []Frame) string {
	var b strings.Builder
	for _, f := range frames {
		item := Map(f.Params["item"])
		fmt.Fprintf(&b, "\n%s %s %s %s turn=%s parent=%s", f.Method, String(item["type"]), String(item["id"]), String(item["status"]), String(f.Params["turnId"]), String(item["parentId"]))
	}
	return b.String()
}

// CLI frames, as the pinned CLI writes them.
func claudeText(delta string) map[string]any {
	return map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": delta}}}
}
func claudeToolUse(parent, id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "parent_tool_use_id": claudeNilable(parent), "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}}
}
func claudeAssistantText(parent, text string) map[string]any {
	return map[string]any{"type": "assistant", "parent_tool_use_id": claudeNilable(parent), "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
}
func claudeToolResult(parent, id string, content any, structured any) map[string]any {
	return map[string]any{"type": "user", "parent_tool_use_id": claudeNilable(parent), "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}}}, "tool_use_result": structured}
}
func claudeResult(origin string) map[string]any {
	v := map[string]any{"type": "result", "is_error": false, "result": "", "usage": map[string]any{"input_tokens": 1.0, "output_tokens": 1.0}}
	if origin != "" {
		v["origin"] = map[string]any{"kind": origin}
	}
	return v
}
func claudeNilable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// A foreground subagent: its frames name the Agent call and nest under it,
// its final text comes back as the call's result, and the parent's own
// text stays its own.
func TestClaudeForegroundSubagentNests(t *testing.T) {
	frames := claudeSession(t, func(e *json.Encoder) {
		_ = e.Encode(claudeText("Looking."))
		_ = e.Encode(claudeToolUse("", "agent_1", "Agent", map[string]any{"description": "List files", "subagent_type": "Explore", "prompt": "List the files.", "run_in_background": false}))
		_ = e.Encode(map[string]any{"type": "system", "subtype": "task_started", "task_id": "a1", "tool_use_id": "agent_1", "description": "List files", "subagent_type": "Explore", "is_backgrounded": false, "task_type": "local_agent"})
		_ = e.Encode(map[string]any{"type": "user", "parent_tool_use_id": "agent_1", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "List the files."}}}})
		_ = e.Encode(claudeToolUse("agent_1", "bash_1", "Bash", map[string]any{"command": "ls"}))
		_ = e.Encode(claudeToolResult("agent_1", "bash_1", "a.txt\nb.txt", map[string]any{"stdout": "a.txt\nb.txt"}))
		_ = e.Encode(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "a1", "tool_use_id": "agent_1", "status": "completed", "summary": "a.txt and b.txt"})
		_ = e.Encode(claudeToolResult("", "agent_1", []any{map[string]any{"type": "text", "text": "a.txt and b.txt"}}, map[string]any{"status": "completed", "agentType": "Explore", "totalToolUseCount": 1.0, "totalDurationMs": 1200.0}))
		_ = e.Encode(claudeText("Done."))
		_ = e.Encode(claudeResult(""))
	}, 1)
	var seq []string
	turn := ""
	for _, f := range frames {
		item := Map(f.Params["item"])
		switch f.Method {
		case "item/started", "item/completed":
			seq = append(seq, f.Method[5:]+":"+String(item["type"])+":"+String(item["id"])+":"+String(item["parentId"]))
			if turn == "" {
				turn = String(f.Params["turnId"])
			} else if String(f.Params["turnId"]) != turn {
				t.Fatalf("every item belongs to the one turn: %s", claudeFrameLog(frames))
			}
			if String(item["id"]) == "agent_1" && f.Method == "item/completed" {
				if item["kind"] != "task" || item["output"] != "a.txt and b.txt" || item["status"] != "completed" || item["background"] == true {
					t.Fatalf("agent card: %v", item)
				}
			}
			if String(item["id"]) == "bash_1" && f.Method == "item/completed" && (item["aggregatedOutput"] != "a.txt\nb.txt" || item["parentId"] != "agent_1") {
				t.Fatalf("child command: %v", item)
			}
		}
	}
	want := []string{"started:agentMessage::", "completed:agentMessage::", "started:toolCall:agent_1:", "started:commandExecution:bash_1:agent_1", "completed:commandExecution:bash_1:agent_1", "completed:toolCall:agent_1:", "started:agentMessage::", "completed:agentMessage::"}
	// Text item ids are random: compare shape only.
	got := make([]string, len(seq))
	for i, s := range seq {
		if strings.HasPrefix(s, "started:agentMessage:") || strings.HasPrefix(s, "completed:agentMessage:") {
			s = s[:strings.Index(s, ":agentMessage:")+len(":agentMessage:")] + ":"
		}
		got[i] = s
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("sequence:\n got %v\nwant %v%s", got, want, claudeFrameLog(frames))
	}
}

// An async subagent: the Agent card stays running (background) past its
// boilerplate result, the parent's text streams on while the child works,
// the child's items keep the parent's turn after that turn ended, the
// notification completes the card with the child's text, and the turn the
// CLI then starts by itself is a turn of its own.
func TestClaudeAsyncSubagentAndCLIStartedTurn(t *testing.T) {
	frames := claudeSession(t, func(e *json.Encoder) {
		_ = e.Encode(claudeText("Starting."))
		_ = e.Encode(claudeToolUse("", "agent_1", "Agent", map[string]any{"description": "List files", "subagent_type": "Explore", "prompt": "List the files."}))
		_ = e.Encode(map[string]any{"type": "system", "subtype": "task_started", "task_id": "a1", "tool_use_id": "agent_1", "description": "List files", "is_backgrounded": true, "task_type": "local_agent"})
		_ = e.Encode(claudeToolResult("", "agent_1", []any{map[string]any{"type": "text", "text": "Async agent launched successfully. agentId: a1"}}, map[string]any{"isAsync": true, "status": "async_launched", "agentId": "a1"}))
		_ = e.Encode(claudeText("Meanwhile "))
		_ = e.Encode(claudeToolUse("agent_1", "bash_1", "Bash", map[string]any{"command": "ls"}))
		_ = e.Encode(claudeText("I wait."))
		_ = e.Encode(claudeToolResult("agent_1", "bash_1", "a.txt", nil))
		_ = e.Encode(claudeResult(""))
		// The child goes on after the parent's turn ended.
		_ = e.Encode(claudeAssistantText("agent_1", "The files: a.txt"))
		_ = e.Encode(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "a1", "tool_use_id": "agent_1", "status": "completed", "summary": "The files: a.txt"})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		_ = e.Encode(claudeText("Reported: a.txt"))
		_ = e.Encode(claudeResult("task-notification"))
	}, 2)
	turn1, turn2 := "", ""
	agentStarts := 0
	var agentDone, childText, reported map[string]any
	var childTextTurn, reportedTurn string
	for _, f := range frames {
		item := Map(f.Params["item"])
		switch f.Method {
		case "item/started":
			if turn1 == "" {
				turn1 = String(f.Params["turnId"])
			}
			if String(item["id"]) == "agent_1" {
				agentStarts++
				if agentStarts == 2 && (item["background"] != true || item["status"] != "running" || item["output"] != "") {
					t.Fatalf("async launch keeps the card running: %v", item)
				}
			}
		case "item/completed":
			switch {
			case String(item["id"]) == "agent_1":
				agentDone = item
			case String(item["id"]) == "bash_1":
				if String(f.Params["turnId"]) != turn1 || item["parentId"] != "agent_1" {
					t.Fatalf("child command in the parent's turn: %v %s", f.Params, claudeFrameLog(frames))
				}
			case item["parentId"] == "agent_1" && item["type"] == "agentMessage":
				childText, childTextTurn = item, String(f.Params["turnId"])
			case item["type"] == "agentMessage" && String(item["text"]) == "Reported: a.txt":
				reported, reportedTurn = item, String(f.Params["turnId"])
			case item["type"] == "agentMessage" && String(item["text"]) == "Meanwhile I wait.":
			case item["type"] == "agentMessage" && String(item["text"]) == "Starting.":
			default:
				t.Fatalf("unexpected item: %v", item)
			}
		case "turn/started":
			turn2 = String(Map(f.Params["turn"])["id"])
		case "turn/completed":
			if id := String(Map(f.Params["turn"])["id"]); id != turn1 && id != turn2 {
				t.Fatalf("turn/completed for an unknown turn %s: %s", id, claudeFrameLog(frames))
			}
		}
	}
	if agentStarts != 2 || agentDone == nil || agentDone["status"] != "completed" || agentDone["output"] != "The files: a.txt" || agentDone["background"] != true {
		t.Fatalf("agent card: starts=%d done=%v%s", agentStarts, agentDone, claudeFrameLog(frames))
	}
	if childText == nil || childTextTurn != turn1 {
		t.Fatalf("child text keeps the parent's turn: %v turn=%s (turn1 %s)%s", childText, childTextTurn, turn1, claudeFrameLog(frames))
	}
	if turn2 == "" || turn2 == turn1 || reported == nil || reportedTurn != turn2 || reported["parentId"] != nil {
		t.Fatalf("the CLI-started turn: turn2=%s reported=%v in %s%s", turn2, reported, reportedTurn, claudeFrameLog(frames))
	}
}

// A background command's card stays running until the task reports; the
// output the model reads with TaskOutput lands on it; a task that fails
// after the turn ended completes the card as failed with the CLI's summary.
func TestClaudeBackgroundCommandCards(t *testing.T) {
	frames := claudeSession(t, func(e *json.Encoder) {
		_ = e.Encode(claudeToolUse("", "bash_1", "Bash", map[string]any{"command": "sleep 4; echo BG", "description": "Slow echo", "run_in_background": true}))
		_ = e.Encode(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "bash_1", "description": "Slow echo", "is_backgrounded": true, "task_type": "local_bash"})
		_ = e.Encode(claudeToolResult("", "bash_1", "Command running in background with ID: b1. Output is being written to: /tmp/b1.output.", map[string]any{"stdout": "", "stderr": "", "backgroundTaskId": "b1"}))
		_ = e.Encode(claudeToolUse("", "out_1", "TaskOutput", map[string]any{"task_id": "b1", "block": true}))
		_ = e.Encode(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "b1", "tool_use_id": "bash_1", "status": "completed", "summary": "Background command \"Slow echo\" completed (exit code 0)"})
		_ = e.Encode(claudeToolResult("", "out_1", "<retrieval_status>success</retrieval_status>\n<task_id>b1</task_id>\n<status>completed</status>\n<exit_code>0</exit_code>\n<output>\nBG\n\n[exited with code 0]\n</output>", map[string]any{"retrieval_status": "success", "task": map[string]any{"task_id": "b1", "task_type": "local_bash", "status": "completed", "description": "Slow echo", "output": "BG\n\n[exited with code 0]\n", "exitCode": 0.0}}))
		_ = e.Encode(claudeToolUse("", "bash_2", "Bash", map[string]any{"command": "sleep 2; exit 3", "description": "Fail later", "run_in_background": true}))
		_ = e.Encode(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b2", "tool_use_id": "bash_2", "description": "Fail later", "is_backgrounded": true, "task_type": "local_bash"})
		_ = e.Encode(claudeToolResult("", "bash_2", "Command running in background with ID: b2.", map[string]any{"backgroundTaskId": "b2"}))
		_ = e.Encode(claudeText("started"))
		_ = e.Encode(claudeResult(""))
		_ = e.Encode(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "b2", "tool_use_id": "bash_2", "status": "failed", "summary": "Background command \"Fail later\" failed with exit code 3"})
		_ = e.Encode(claudeText("It failed."))
		_ = e.Encode(claudeResult("task-notification"))
	}, 2)
	var bash1, bash2, out1 []map[string]any
	turn1 := ""
	for _, f := range frames {
		item := Map(f.Params["item"])
		if f.Method != "item/started" && f.Method != "item/completed" {
			continue
		}
		if turn1 == "" {
			turn1 = String(f.Params["turnId"])
		}
		item["_method"] = f.Method
		item["_turn"] = String(f.Params["turnId"])
		switch String(item["id"]) {
		case "bash_1":
			bash1 = append(bash1, item)
		case "bash_2":
			bash2 = append(bash2, item)
		case "out_1":
			out1 = append(out1, item)
		}
	}
	// bash_1: started, started again (background, running) at the
	// boilerplate result, completed with the summary at the notification,
	// completed again with the real output from TaskOutput.
	if len(bash1) != 4 || bash1[1]["_method"] != "item/started" || bash1[1]["background"] != true || bash1[1]["status"] != "running" || bash1[1]["aggregatedOutput"] != "" {
		t.Fatalf("background launch: %v%s", bash1, claudeFrameLog(frames))
	}
	if bash1[2]["_method"] != "item/completed" || bash1[2]["status"] != "completed" || !strings.HasPrefix(String(bash1[2]["aggregatedOutput"]), "Background command") {
		t.Fatalf("notification: %v", bash1[2])
	}
	if bash1[3]["_method"] != "item/completed" || bash1[3]["status"] != "completed" || bash1[3]["aggregatedOutput"] != "BG\n\n[exited with code 0]" || bash1[3]["background"] != true {
		t.Fatalf("output read: %v", bash1[3])
	}
	if len(out1) != 2 || out1[0]["title"] != "Task output: Slow echo" || out1[1]["status"] != "completed" || out1[1]["output"] != "BG\n\n[exited with code 0]" {
		t.Fatalf("TaskOutput card: %v", out1)
	}
	if len(bash2) != 3 || bash2[2]["status"] != "failed" || bash2[2]["_turn"] != turn1 || !strings.Contains(String(bash2[2]["aggregatedOutput"]), "exit code 3") {
		t.Fatalf("failed after the turn, on the turn's card: %v%s", bash2, claudeFrameLog(frames))
	}
}

// A result that carries the model's answer to a background task's
// notification while the turn Warden asked for still runs does not end
// that turn: the answer is a message in it, the turn ends with its own
// result.
func TestClaudeNotificationResultDuringTurnKeepsItOpen(t *testing.T) {
	frames := claudeSession(t, func(e *json.Encoder) {
		_ = e.Encode(claudeToolUse("", "bash_1", "Bash", map[string]any{"command": "sleep 30"}))
		_ = e.Encode(claudeText("The earlier task finished."))
		_ = e.Encode(claudeResult("task-notification"))
		_ = e.Encode(claudeToolResult("", "bash_1", "", map[string]any{"stdout": ""}))
		_ = e.Encode(claudeText("Done."))
		_ = e.Encode(claudeResult(""))
	}, 1)
	var texts []string
	completed := 0
	bashDone := false
	for _, f := range frames {
		item := Map(f.Params["item"])
		switch f.Method {
		case "item/completed":
			if item["type"] == "agentMessage" {
				texts = append(texts, String(item["text"]))
			}
			if String(item["id"]) == "bash_1" {
				bashDone = true
			}
		case "turn/completed":
			completed++
		case "turn/started":
			t.Fatalf("no turn of the CLI's own here: %s", claudeFrameLog(frames))
		}
	}
	if completed != 1 || !bashDone || strings.Join(texts, "|") != "The earlier task finished.|Done." {
		t.Fatalf("completed=%d bash=%v texts=%v%s", completed, bashDone, texts, claudeFrameLog(frames))
	}
}

// The todo list is one item, replaced by every write; the writes get no
// card of their own.
func TestClaudeTodoListItem(t *testing.T) {
	frames := claudeSession(t, func(e *json.Encoder) {
		_ = e.Encode(claudeToolUse("", "c1", "TaskCreate", map[string]any{"subject": "Write the parser", "activeForm": "Writing the parser"}))
		_ = e.Encode(claudeToolResult("", "c1", "Task #1 created successfully: Write the parser", map[string]any{"task": map[string]any{"id": "1", "subject": "Write the parser"}}))
		_ = e.Encode(claudeToolUse("", "c2", "TaskCreate", map[string]any{"subject": "Add tests"}))
		_ = e.Encode(claudeToolResult("", "c2", "Task #2 created successfully: Add tests", map[string]any{"task": map[string]any{"id": "2", "subject": "Add tests"}}))
		_ = e.Encode(claudeToolUse("", "u1", "TaskUpdate", map[string]any{"taskId": "1", "status": "in_progress"}))
		_ = e.Encode(claudeToolResult("", "u1", "Updated task #1 status", map[string]any{"success": true}))
		_ = e.Encode(claudeToolUse("", "l1", "TaskList", map[string]any{}))
		_ = e.Encode(claudeToolResult("", "l1", "#1 [in_progress] Write the parser\n#2 [pending] Add tests", map[string]any{"tasks": []any{map[string]any{"id": "1", "subject": "Write the parser", "status": "in_progress"}, map[string]any{"id": "2", "subject": "Add tests", "status": "pending"}}}))
		_ = e.Encode(claudeToolUse("", "w1", "TodoWrite", map[string]any{"todos": []any{map[string]any{"content": "Ship", "status": "completed", "activeForm": "Shipping"}}}))
		_ = e.Encode(claudeToolResult("", "w1", "Todos have been modified successfully.", nil))
		_ = e.Encode(claudeText("ok"))
		_ = e.Encode(claudeResult(""))
	}, 1)
	var lists []map[string]any
	id := ""
	for _, f := range frames {
		item := Map(f.Params["item"])
		if f.Method == "item/started" && item["type"] != "agentMessage" {
			t.Fatalf("a todo write started a card: %v", item)
		}
		if f.Method != "item/completed" || item["type"] != "todoList" {
			continue
		}
		if id == "" {
			id = String(item["id"])
		} else if String(item["id"]) != id {
			t.Fatalf("the list is one item: %s vs %s", id, item["id"])
		}
		lists = append(lists, item)
	}
	if len(lists) != 5 {
		t.Fatalf("one list per write: %d%s", len(lists), claudeFrameLog(frames))
	}
	todo := func(i, j int) map[string]any { return Map(Array(lists[i]["todos"])[j]) }
	if len(Array(lists[1]["todos"])) != 2 || todo(1, 0)["content"] != "Write the parser" || todo(1, 0)["status"] != "pending" || todo(1, 0)["activeForm"] != "Writing the parser" {
		t.Fatalf("after creates: %v", lists[1]["todos"])
	}
	if todo(2, 0)["status"] != "in_progress" || todo(2, 1)["status"] != "pending" {
		t.Fatalf("after update: %v", lists[2]["todos"])
	}
	if todo(3, 0)["status"] != "in_progress" || todo(3, 0)["activeForm"] != "Writing the parser" || todo(3, 1)["content"] != "Add tests" {
		t.Fatalf("after list: %v", lists[3]["todos"])
	}
	if len(Array(lists[4]["todos"])) != 1 || todo(4, 0)["content"] != "Ship" || todo(4, 0)["status"] != "completed" || lists[4]["tool"] != "TodoWrite" {
		t.Fatalf("after TodoWrite: %v", lists[4])
	}
}

func TestClaudeTaskHelpers(t *testing.T) {
	if claudeTaskStatus("completed", "Background command \"x\" completed (exit code 0)") != "completed" || claudeTaskStatus("completed", "failed with exit code 3") != "failed" || claudeTaskStatus("failed", "") != "failed" || claudeTaskStatus("killed", "") != "failed" || claudeTaskStatus("", "done") != "completed" {
		t.Fatal("task status")
	}
	task, out, status := claudeTaskOutput(map[string]any{"content": "<retrieval_status>success</retrieval_status>\n\n<task_id>b1</task_id>\n\n<status>failed</status>\n\n<exit_code>2</exit_code>\n\n<output>\nboom\n</output>"}, nil)
	if task != "b1" || out != "boom" || status != "failed" {
		t.Fatalf("text fallback: %q %q %q", task, out, status)
	}
	if task, _, _ := claudeTaskOutput(map[string]any{"content": "<retrieval_status>not_found</retrieval_status>"}, nil); task != "" {
		t.Fatal("a failed retrieval changes nothing")
	}
	var l claudeTodoList
	if !l.apply("TaskList", nil, map[string]any{"content": "#1 [in_progress] Write\n#2 [pending] Test\nnot a task"}, nil) || len(l.items) != 2 || l.items[0].status != "in_progress" || l.items[1].content != "Test" || l.items[1].id != "2" {
		t.Fatalf("TaskList text: %+v", l.items)
	}
	if !l.apply("TaskUpdate", map[string]any{"taskId": "1", "status": "deleted"}, nil, nil) || len(l.items) != 1 || l.items[0].id != "2" {
		t.Fatalf("delete: %+v", l.items)
	}
	if l.apply("TaskUpdate", map[string]any{"taskId": "9", "status": "completed"}, nil, nil) {
		t.Fatal("an unknown task changes nothing")
	}
	if !l.apply("TaskGet", nil, nil, map[string]any{"task": map[string]any{"id": "2", "subject": "Test it", "status": "completed"}}) || l.items[0].content != "Test it" || l.items[0].status != "completed" {
		t.Fatalf("TaskGet: %+v", l.items)
	}
	if id := claudeBackgroundTask(claudeTool{name: "Bash", task: "b7"}, map[string]any{"content": "Command running in background with ID: b7."}, nil); id != "b7" {
		t.Fatalf("background from text: %q", id)
	}
	if id := claudeBackgroundTask(claudeTool{name: "Bash"}, map[string]any{"content": "done"}, map[string]any{"stdout": "done"}); id != "" {
		t.Fatalf("a foreground command: %q", id)
	}
}

// The CLI's system/init lists the session's slash commands (built-ins
// plus whatever the workspace defines); thread/started carries them, less
// the terminal-only and internal ones, with the resolved settings. A
// /compact turn then brings a compact_boundary and an empty result: the
// boundary is its own notification and the turn still completes. Frames
// as observed on CLI 2.1.272.
func TestClaudeInitCommandsAndCompaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	done := make(chan Frame, 40)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s",
			"slash_commands": []any{"code-review", "compact", "init", "doctor", "__remote-workflow", "probe-cmd"}, "terminal_slash_commands": []any{"doctor"},
			"model": "claude-opus-5[1m]", "permissionMode": "default", "output_style": "default", "mcp_servers": []any{map[string]any{"name": "warden", "status": "connected"}},
			"memory_paths": map[string]any{"auto": "/home/agent/.claude/projects/-home-agent-workspace/memory"}})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "status", "status": "compacting", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s", "slash_commands": []any{"compact"}})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "compact_boundary", "session_id": "s", "compact_metadata": map[string]any{"trigger": "manual", "pre_tokens": 27230.0, "post_tokens": 1850.0, "duration_ms": 28060.0}})
		_ = e.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "This session is being continued from a previous conversation…"}, "isReplay": true, "isSynthetic": true})
		_ = e.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "<local-command-stdout>Compacted </local-command-stdout>"}, "isReplay": true})
		_ = e.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "", "num_turns": 0.0, "usage": map[string]any{"input_tokens": 0.0, "output_tokens": 0.0}, "total_cost_usd": 0.1})
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { done <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	turn, err := c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "/compact"}}})
	if err != nil {
		t.Fatal(err)
	}
	turnID := String(Map(turn["turn"])["id"])
	var threads []map[string]any
	var compacted map[string]any
	items := 0
	for {
		select {
		case f := <-done:
			switch f.Method {
			case "thread/started":
				threads = append(threads, Map(f.Params["thread"]))
			case "item/started", "item/completed":
				// The boundary is a compaction item (item 8); nothing else
				// streams.
				if item := Map(f.Params["item"]); item["type"] == "compaction" {
					if f.Method == "item/completed" {
						compacted = item
					}
					if String(f.Params["turnId"]) != turnID {
						t.Fatalf("compaction outside the turn: %+v", f.Params)
					}
					continue
				}
				items++
			case "turn/completed":
				if len(threads) != 2 {
					t.Fatalf("thread/started %d times", len(threads))
				}
				first := threads[0]
				names := []string{}
				for _, v := range Array(first["commands"]) {
					names = append(names, String(Map(v)["name"]))
				}
				if first["id"] != "s" || fmt.Sprint(names) != "[code-review compact init probe-cmd]" || first["model"] != "claude-opus-5[1m]" || first["permissionMode"] != "default" || first["outputStyle"] != "default" || first["autoMemory"] != "/home/agent/.claude/projects/-home-agent-workspace/memory" {
					t.Fatalf("thread %+v", first)
				}
				if compacted == nil || compacted["trigger"] != "manual" || compacted["preTokens"] != 27230.0 || compacted["postTokens"] != 1850.0 || compacted["status"] != "completed" {
					t.Fatalf("compacted %+v", compacted)
				}
				// The summary and the local command's stdout are the CLI's
				// own user frames, not transcript items; nothing streamed.
				if items != 0 || String(Map(f.Params["turn"])["status"]) != "completed" {
					t.Fatalf("items %d, turn %+v", items, f.Params)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("translation timed out")
		}
	}
}

// The model, thinking budget, effort level and fast mode of a live
// session are control requests: set_model (the CLI's refusal fails the
// call), set_max_thinking_tokens (null for the default, 0 for off, else
// the budget) and apply_flag_settings (effortLevel, fastMode), each sent
// only when it changes what the CLI has; the fast-mode state the CLI
// reports rides on thread/started.
func TestClaudeModelThinkingEffortAndFastMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, cf, frames := newClaudeFake(t, ctx)
	// answer accepts the next control request, checks it and answers it.
	answer := func(want map[string]any, response map[string]any) {
		go func() {
			v := cf.next(t, ctx)
			req := Map(v["request"])
			for k, expected := range want {
				got := req[k]
				if m, ok := expected.(map[string]any); ok {
					got = Map(req[k])
					for kk, vv := range m {
						if got.(map[string]any)[kk] != vv {
							t.Errorf("%s.%s = %v, want %v in %+v", k, kk, got.(map[string]any)[kk], vv, req)
						}
					}
					continue
				}
				if got != expected {
					t.Errorf("%s = %v, want %v in %+v", k, got, expected, req)
				}
			}
			cf.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": response}})
		}()
	}
	refuse := func(message string) {
		go func() {
			v := cf.next(t, ctx)
			cf.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": v["request_id"], "error": message}})
		}()
	}
	quiet := func(what string) {
		t.Helper()
		select {
		case v := <-cf.writes:
			t.Fatalf("%s: unexpected write %+v", what, v)
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Model: the alias goes as it is, "" as "default".
	answer(map[string]any{"subtype": "set_model", "model": "opus"}, nil)
	if r, err := c.Call(ctx, "model/set", map[string]any{"model": "opus"}); err != nil || r["model"] != "opus" {
		t.Fatal(r, err)
	}
	answer(map[string]any{"subtype": "set_model", "model": "default"}, nil)
	if r, err := c.Call(ctx, "model/set", map[string]any{"model": ""}); err != nil || r["model"] != "" {
		t.Fatal(r, err)
	}
	refuse("Model 'bogus' not found")
	if _, err := c.Call(ctx, "model/set", map[string]any{"model": "bogus"}); err == nil || !strings.Contains(err.Error(), "Model 'bogus' not found") {
		t.Fatal(err)
	}
	// Thinking: the default needs no request on a fresh process; off is a
	// budget of 0; a number is the budget; back to the default is null.
	if r, err := c.Call(ctx, "thinking/set", map[string]any{"thinking": ""}); err != nil || r["thinking"] != "" {
		t.Fatal(r, err)
	}
	quiet("thinking default")
	answer(map[string]any{"subtype": "set_max_thinking_tokens", "max_thinking_tokens": 0.0}, nil)
	if r, err := c.Call(ctx, "thinking/set", map[string]any{"thinking": "off"}); err != nil || r["thinking"] != "off" {
		t.Fatal(r, err)
	}
	if r, err := c.Call(ctx, "thinking/set", map[string]any{"thinking": "off"}); err != nil || r["thinking"] != "off" {
		t.Fatal(r, err)
	}
	quiet("thinking unchanged")
	answer(map[string]any{"subtype": "set_max_thinking_tokens", "max_thinking_tokens": 8000.0}, nil)
	if r, err := c.Call(ctx, "thinking/set", map[string]any{"thinking": "8000"}); err != nil || r["thinking"] != "8000" {
		t.Fatal(r, err)
	}
	answer(map[string]any{"subtype": "set_max_thinking_tokens", "max_thinking_tokens": nil}, nil)
	if r, err := c.Call(ctx, "thinking/set", map[string]any{"thinking": ""}); err != nil || r["thinking"] != "" {
		t.Fatal(r, err)
	}
	// A refusal leaves the CLI's value as it was, so the same ask is sent again.
	refuse("set_max_thinking_tokens: max_thinking_tokens must be an integer or null")
	if _, err := c.Call(ctx, "thinking/set", map[string]any{"thinking": "off"}); err == nil || !strings.Contains(err.Error(), "must be an integer") {
		t.Fatal(err)
	}
	answer(map[string]any{"subtype": "set_max_thinking_tokens", "max_thinking_tokens": 0.0}, nil)
	if _, err := c.Call(ctx, "thinking/set", map[string]any{"thinking": "off"}); err != nil {
		t.Fatal(err)
	}
	// Effort through the flag settings; the default is null.
	answer(map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": "low"}}, nil)
	if r, err := c.Call(ctx, "effort/set", map[string]any{"effort": "low"}); err != nil || r["effort"] != "low" {
		t.Fatal(r, err)
	}
	if _, err := c.Call(ctx, "effort/set", map[string]any{"effort": "low"}); err != nil {
		t.Fatal(err)
	}
	quiet("effort unchanged")
	answer(map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": nil}}, nil)
	if r, err := c.Call(ctx, "effort/set", map[string]any{"effort": ""}); err != nil || r["effort"] != "" {
		t.Fatal(r, err)
	}
	// Fast mode likewise.
	if _, err := c.Call(ctx, "fastMode/set", map[string]any{"fast": false}); err != nil {
		t.Fatal(err)
	}
	quiet("fast mode off already")
	answer(map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"fastMode": true}}, nil)
	if r, err := c.Call(ctx, "fastMode/set", map[string]any{"fast": true}); err != nil || r["fast"] != true {
		t.Fatal(r, err)
	}
	// The next init reports the model and the fast-mode state.
	cf.send(map[string]any{"type": "system", "subtype": "init", "session_id": "s", "model": "claude-opus-5", "permissionMode": "default", "fast_mode_state": "on"})
	f := nextFrame(t, ctx, frames, "thread/started")
	if th := Map(f.Params["thread"]); th["model"] != "claude-opus-5" || th["fastMode"] != "on" {
		t.Fatalf("%+v", th)
	}
	if claudeThinkingBudget("") != nil || claudeThinkingBudget("off") != 0 || claudeThinkingBudget("4000") != 4000 || claudeThinkingBudget("x") != nil {
		t.Fatal("claudeThinkingBudget")
	}
}

// A /compact turn, as CLI 2.1.272 emits it: a compaction item runs from
// the "compacting" status to the compact_boundary, which completes it
// with the trigger and the token counts; the synthetic user frame that
// follows completes it again with the summary. The context is reported
// per model call from the assistant frame's usage (input plus cache read
// and written), re-estimated at the boundary from post_tokens and the
// fixed prefix, with the window from the result's modelUsage. A failed
// auto-compaction completes the item as failed.
func TestClaudeCompactionAndContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	done := make(chan Frame, 60)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		if !claudeNext(d, &v) {
			return
		}
		usage := map[string]any{"input_tokens": 2.0, "cache_creation_input_tokens": 11788.0, "cache_read_input_tokens": 28803.0, "output_tokens": 2.0}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s", "model": "claude-sonnet-5"})
		_ = e.Encode(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-sonnet-5", "usage": usage, "content": []any{map[string]any{"type": "thinking", "thinking": ""}}}})
		_ = e.Encode(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-sonnet-5", "usage": usage, "content": []any{map[string]any{"type": "text", "text": "ok"}}}})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "status", "status": "compacting", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "status", "status": nil, "compact_result": "success", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s", "model": "claude-sonnet-5"})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "compact_boundary", "session_id": "s", "compact_metadata": map[string]any{"trigger": "manual", "pre_tokens": 171238.0, "post_tokens": 2194.0, "cumulative_dropped_tokens": 169044.0, "duration_ms": 22526.0}})
		_ = e.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\nSummary:\n1. Files read: a.txt (lima)"}, "isSynthetic": true, "isReplay": false})
		_ = e.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "<local-command-stdout>Compacted </local-command-stdout>"}, "isReplay": true})
		_ = e.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "", "num_turns": 0.0, "usage": map[string]any{"input_tokens": 0.0, "cache_creation_input_tokens": 0.0, "cache_read_input_tokens": 0.0, "output_tokens": 0.0}, "total_cost_usd": 0.78, "modelUsage": map[string]any{"claude-sonnet-5": map[string]any{"contextWindow": 200000.0, "maxOutputTokens": 64000.0}}})
		// After the turn the adapter asks the CLI for its own account of
		// the context (the boundary's request and the result's both arrive;
		// the first is answered, as CLI 2.1.272 answers get_context_usage).
		for i := 0; i < 2; i++ {
			if d.Decode(&v) != nil || v["type"] != "control_request" || Map(v["request"])["subtype"] != "get_context_usage" {
				return
			}
			if i == 0 {
				_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": map[string]any{"totalTokens": 41113.0, "maxTokens": 200000.0, "rawMaxTokens": 200000.0, "autocompactSource": "model-default", "percentage": 21.0, "categories": []any{map[string]any{"name": "System prompt", "tokens": 8516.0, "kind": "used"}, map[string]any{"name": "Autocompact buffer", "tokens": 33000.0, "kind": "buffer"}, map[string]any{"name": "Free space", "tokens": 125887.0, "kind": "free"}}}}})
			}
		}
		// The next turn: an automatic compaction that fails.
		if !claudeNext(d, &v) {
			return
		}
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s", "model": "claude-sonnet-5"})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "status", "status": "compacting", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "system", "subtype": "status", "status": nil, "compact_result": "failed", "compact_error": "API Error: refused", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "assistant", "message": map[string]any{"model": "<synthetic>", "usage": map[string]any{"input_tokens": 0.0, "output_tokens": 0.0}, "content": []any{map[string]any{"type": "text", "text": "Prompt is too long · automatic compaction failed: API Error: refused"}}}})
		_ = e.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "Prompt is too long · automatic compaction failed: API Error: refused", "usage": map[string]any{"input_tokens": 0.0, "output_tokens": 0.0}, "total_cost_usd": 0.78})
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) { done <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "/compact keep the file list"}}}); err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	var contexts []map[string]any
	collect := func() {
		for {
			select {
			case f := <-done:
				switch f.Method {
				case "item/started", "item/completed":
					if item := Map(f.Params["item"]); item["type"] == "compaction" {
						items = append(items, item)
					}
				case "thread/context/updated":
					contexts = append(contexts, Map(f.Params["context"]))
				case "turn/completed":
					return
				}
			case <-ctx.Done():
				t.Fatal("translation timed out")
			}
		}
	}
	collect()
	if len(items) != 3 || items[0]["status"] != "running" || items[1]["status"] != "completed" || items[2]["status"] != "completed" || items[0]["id"] != items[2]["id"] {
		t.Fatalf("compaction items %+v", items)
	}
	if items[1]["trigger"] != "manual" || items[1]["preTokens"] != 171238.0 || items[1]["postTokens"] != 2194.0 || items[1]["summary"] != nil {
		t.Fatalf("boundary item %+v", items[1])
	}
	if items[2]["trigger"] != "manual" || items[2]["preTokens"] != 171238.0 || !strings.HasPrefix(String(items[2]["summary"]), "This session is being continued") {
		t.Fatalf("summary item %+v", items[2])
	}
	// One report per change: the call (the table's window before any
	// result), the boundary's estimate (the summary plus the prefix, the
	// smallest context seen); the result's window is the same 200k.
	if len(contexts) != 2 || contexts[0]["used"] != 40593.0 || contexts[0]["window"] != 200000.0 || contexts[0]["model"] != "claude-sonnet-5" || contexts[0]["threshold"] != nil || contexts[1]["used"] != 2194.0+40593 || contexts[1]["window"] != 200000.0 {
		t.Fatalf("contexts %+v", contexts)
	}
	// The CLI's own account then corrects the estimate and adds where it
	// compacts on its own: the window less its buffer.
	select {
	case f := <-done:
		if f.Method != "thread/context/updated" {
			t.Fatalf("after the turn: %s %+v", f.Method, f.Params)
		}
		if c := Map(f.Params["context"]); c["used"] != 41113.0 || c["window"] != 200000.0 || c["threshold"] != 167000.0 {
			t.Fatalf("accounted context %+v", c)
		}
	case <-ctx.Done():
		t.Fatal("no context account")
	}
	items, contexts = nil, nil
	if _, err = c.Call(ctx, "turn/start", map[string]any{"input": []any{map[string]any{"text": "go on"}}}); err != nil {
		t.Fatal(err)
	}
	collect()
	if len(items) != 2 || items[0]["status"] != "running" || items[1]["status"] != "failed" || items[1]["error"] != "API Error: refused" || len(contexts) != 0 {
		t.Fatalf("failed compaction %+v, contexts %+v", items, contexts)
	}
}

func TestClaudeContextWindowTable(t *testing.T) {
	for model, want := range map[string]int64{"claude-sonnet-5": 200000, "claude-sonnet-5[1m]": 1000000, "claude-opus-4-5": 200000, "claude-opus-4-6": 1000000, "claude-opus-5": 1000000, "claude-haiku-4-5-20251001": 200000, "claude-sonnet-4-6": 1000000, "claude-fable-5": 1000000, "": 200000} {
		if got := claudeContextWindow(model); got != want {
			t.Errorf("%s: %d, want %d", model, got, want)
		}
	}
	// The result's modelUsage overrides the table (a [1m] session on a
	// model the table calls 200k, or the other way round).
	c := claudeContext{}
	c.model("claude-sonnet-5")
	c.result(map[string]any{"modelUsage": map[string]any{"claude-sonnet-5": map[string]any{"contextWindow": 1000000.0}}})
	if c.window != 1000000 {
		t.Fatalf("window %d", c.window)
	}
	c.result(map[string]any{"modelUsage": map[string]any{"other": map[string]any{"contextWindow": 500000.0}}})
	if c.window != 500000 {
		t.Fatalf("lone entry: window %d", c.window)
	}
}

// claudeNext reads the adapter's next frame for a fake CLI, skipping the
// context requests the adapter sends on its own after a turn (a real CLI
// answers them; the fakes here need not).
func claudeNext(d *json.Decoder, v *map[string]any) bool {
	for {
		if d.Decode(v) != nil {
			return false
		}
		if (*v)["type"] == "control_request" && Map((*v)["request"])["subtype"] == "get_context_usage" {
			continue
		}
		return true
	}
}

// A user message carries Warden's message ID as its uuid, and
// conversation/rewind is the CLI's rewind_conversation control request,
// answered with what the CLI said: rewound, or not with the reason.
func TestClaudeConversationRewind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, fake := net.Pipe()
	defer fake.Close()
	seen := make(chan map[string]any, 8)
	go func() {
		d := json.NewDecoder(fake)
		e := json.NewEncoder(fake)
		var v map[string]any
		if d.Decode(&v) != nil {
			return
		}
		_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "warden-init", "response": map[string]any{}}})
		// The user message, answered at once (a fresh map per frame: a
		// decode into a used map merges into it).
		v = nil
		if d.Decode(&v) != nil {
			return
		}
		seen <- v
		_ = e.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		_ = e.Encode(map[string]any{"type": "result", "is_error": false, "result": "ok"})
		// Two rewind requests: one the CLI applies, one it refuses (the
		// context requests the adapter sends after the result are skipped).
		for i := 0; i < 2; i++ {
			v = nil
			if !claudeNext(d, &v) {
				return
			}
			seen <- v
			req := Map(v["request"])
			answer := map[string]any{"rewound": true, "targetMessageUuid": req["target_message_uuid"], "prefillText": "first"}
			if req["target_message_uuid"] == "unknown" {
				answer = map[string]any{"rewound": false, "error": "target not found", "reason": "target_not_found"}
			}
			_ = e.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": v["request_id"], "response": answer}})
		}
	}()
	c, err := StartStream(ctx, ClaudeStream(ctx, raw), func(_ *Client, f Frame) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Call(ctx, "thread/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Call(ctx, "turn/start", map[string]any{"clientUserMessageId": "m1", "input": []any{map[string]any{"text": "first"}}}); err != nil {
		t.Fatal(err)
	}
	if v := <-seen; v["type"] != "user" || v["uuid"] != "m1" {
		t.Fatalf("user frame without the message's uuid: %v", v)
	}
	result, err := c.Call(ctx, "conversation/rewind", map[string]any{"threadId": "s", "targetMessageId": "m1", "lastSeenMessageId": "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if v := <-seen; v["type"] != "control_request" || Map(v["request"])["subtype"] != "rewind_conversation" || Map(v["request"])["target_message_uuid"] != "m1" || Map(v["request"])["last_seen_user_message_uuid"] != "m1" {
		t.Fatalf("rewind not forwarded as rewind_conversation: %v", v)
	}
	if result["rewound"] != true || result["prefillText"] != "first" {
		t.Fatalf("rewind answer: %v", result)
	}
	result, err = c.Call(ctx, "conversation/rewind", map[string]any{"threadId": "s", "targetMessageId": "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if v := <-seen; Map(v["request"])["last_seen_user_message_uuid"] != nil {
		t.Fatalf("an empty last-seen id must be left out: %v", v)
	}
	if result["rewound"] != false || result["reason"] != "target_not_found" {
		t.Fatalf("refused rewind answer: %v", result)
	}
}
