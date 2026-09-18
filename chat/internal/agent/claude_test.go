package agent

import (
	"context"
	"encoding/json"
	"net"
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

func TestClaudeBuiltinToolsAllowedAndMCPResponse(t *testing.T) {
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
		allowed := Map(Map(v["response"])["response"])["behavior"] == "allow"
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
		// Built-in tools are allowed without an owner prompt; the first
		// frame the controller sees is the Warden MCP tool call.
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
