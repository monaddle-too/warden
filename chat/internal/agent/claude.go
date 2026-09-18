package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
)

// ClaudeStream translates Claude Code's documented SDK control protocol into
// Warden's conversation protocol. The real CLI stays inside the sandbox.
func ClaudeStream(ctx context.Context, raw io.ReadWriteCloser) io.ReadWriteCloser {
	client, bridge := net.Pipe()
	go func() {
		defer bridge.Close()
		defer raw.Close()
		frames := make(chan map[string]any, 64)
		commands := make(chan Frame, 64)
		go func() {
			defer close(frames)
			scan := bufio.NewScanner(raw)
			scan.Buffer(make([]byte, 65536), 8<<20)
			for scan.Scan() {
				var v map[string]any
				if json.Unmarshal(scan.Bytes(), &v) != nil {
					return
				}
				select {
				case frames <- v:
				case <-ctx.Done():
					return
				}
			}
		}()
		go func() {
			defer close(commands)
			scan := bufio.NewScanner(bridge)
			// Commands come from the chat service; a turn's input may carry a
			// few images as base64.
			scan.Buffer(make([]byte, 65536), 32<<20)
			for scan.Scan() {
				var f Frame
				if json.Unmarshal(scan.Bytes(), &f) != nil {
					return
				}
				select {
				case commands <- f:
				case <-ctx.Done():
					return
				}
			}
		}()
		out := json.NewEncoder(bridge)
		cli := json.NewEncoder(raw)
		send := func(f Frame) { _ = out.Encode(f) }
		reply := func(id json.RawMessage, v any) { b, _ := json.Marshal(v); send(Frame{ID: id, Result: b}) }
		event := func(method string, p map[string]any) { send(Frame{Method: method, Params: p}) }
		control := func(id string, v any) {
			_ = cli.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": v}})
		}
		thread, turn := "", ""
		var initID json.RawMessage
		tools := []any{}
		pending := map[string]map[string]any{}
		// Tool calls in flight, by tool_use id, until their result.
		toolCalls := map[string]claudeTool{}
		textID := ""
		text := ""
		streamed := false
		// Token usage as the chat service expects it from Codex: a running
		// total for the process, per turn as the growth of that total.
		total := claudeUsage{}
		// A text block ends when Claude turns to a tool; complete it as its own
		// message so later text is a new bubble after the tool steps, not glued
		// onto this one.
		flushText := func() {
			if text == "" {
				return
			}
			event("item/completed", map[string]any{"turnId": turn, "item": map[string]any{"id": textID, "type": "agentMessage", "text": text}})
			text = ""
			textID = claudeID()
			streamed = true
		}
		// A thinking block is a reasoning item in Codex's shape, streamed
		// as it arrives so the transcript can show the model at work; each
		// block (one per model call, so one before every tool) is its own.
		thinkingID, thinking := "", ""
		// An interrupt asked of the CLI: its next result ends the turn as
		// interrupted, whatever the CLI calls the abort.
		interrupting := false
		// The context: what the last model call was given and the model's
		// window, reported as thread/context/updated when either changes
		// (claudeContext); a compaction in flight, as a transcript item.
		ctx2 := claudeContext{}
		compaction := ""
		var compactionMeta map[string]any
		reportContext := func() {
			if p, ok := ctx2.changed(); ok {
				event("thread/context/updated", map[string]any{"threadId": thread, "turnId": turn, "context": p})
			}
		}
		flushThinking := func() {
			if thinkingID == "" {
				return
			}
			event("item/completed", map[string]any{"turnId": turn, "item": map[string]any{"id": thinkingID, "type": "reasoning", "summary": []any{thinking}}})
			thinkingID, thinking = "", ""
		}
		for {
			select {
			case <-ctx.Done():
				return
			case f, ok := <-commands:
				if !ok {
					return
				}
				switch f.Method {
				case "initialize":
					initID = f.ID
					_ = cli.Encode(map[string]any{"type": "control_request", "request_id": "warden-init", "request": map[string]any{"subtype": "initialize", "sdkMcpServers": []string{"warden"}}})
				case "initialized":
				case "thread/start", "thread/resume":
					tools = Array(f.Params["dynamicTools"])
					thread = String(f.Params["threadId"])
					if thread == "" {
						thread = claudeID()
					}
					reply(f.ID, map[string]any{"thread": map[string]any{"id": thread}})
				case "turn/start", "turn/steer":
					if f.Method == "turn/start" {
						turn = claudeID()
						text = ""
						textID = claudeID()
						streamed = false
					}
					if f.Method == "turn/start" {
						reply(f.ID, map[string]any{"turn": map[string]any{"id": turn, "status": "inProgress"}})
					} else {
						reply(f.ID, map[string]any{"turnId": turn})
					}
					content := []any{}
					for _, v := range Array(f.Params["input"]) {
						item := Map(v)
						if String(item["type"]) != "localImage" {
							content = append(content, map[string]any{"type": "text", "text": String(item["text"])})
							continue
						}
						// The CLI runs in the sandbox but this adapter does not, so an
						// image comes with its bytes (a normalised PNG); without them the
						// path in the text is all Claude gets, and it can read the file.
						if data := String(item["data"]); data != "" {
							content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": data}})
						}
					}
					_ = cli.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}, "parent_tool_use_id": nil})
				case "turn/interrupt":
					// Claude Code's SDK interrupt: the query aborts where it is
					// (mid-thought, mid-tool) and reports a result; the process
					// stays up for the next message.
					if turn != "" && String(f.Params["turnId"]) == turn {
						interrupting = true
						_ = cli.Encode(map[string]any{"type": "control_request", "request_id": "warden-interrupt-" + claudeID(), "request": map[string]any{"subtype": "interrupt"}})
					}
					reply(f.ID, map[string]any{})
				case "":
					key := string(f.ID)
					p := pending[key]
					if p == nil {
						continue
					}
					delete(pending, key)
					var result map[string]any
					_ = json.Unmarshal(f.Result, &result)
					id := String(p["request_id"])
					req := Map(p["request"])
					if String(req["subtype"]) == "mcp_message" {
						msg := Map(req["message"])
						content := []any{}
						for _, v := range Array(result["contentItems"]) {
							content = append(content, map[string]any{"type": "text", "text": String(Map(v)["text"])})
						}
						control(id, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{"content": content, "isError": result["success"] != true}}})
					} else {
						answer := map[string]any{"behavior": "deny", "message": "Denied by Warden"}
						if result["decision"] == "accept" {
							answer = map[string]any{"behavior": "allow", "updatedInput": req["input"]}
						}
						if req["tool_name"] == "AskUserQuestion" {
							updated := Map(req["input"])
							answers := map[string]string{}
							provided := Map(result["answers"])
							for i, q := range Array(updated["questions"]) {
								values := Array(Map(provided[fmt.Sprintf("q%d", i)])["answers"])
								chosen := []string{}
								for _, v := range values {
									chosen = append(chosen, String(v))
								}
								if len(chosen) > 0 {
									answers[String(Map(q)["question"])] = strings.Join(chosen, ", ")
								}
							}
							if len(answers) > 0 {
								updated["answers"] = answers
								answer = map[string]any{"behavior": "allow", "updatedInput": updated}
							}
						}

						control(id, answer)
					}
				default:
					send(Frame{ID: f.ID, Error: &RPCError{Code: -32601, Message: "Unsupported Claude operation"}})
				}
			case v, ok := <-frames:
				if !ok {
					return
				}
				switch String(v["type"]) {
				case "control_response":
					r := Map(v["response"])
					if r["request_id"] == "warden-init" {
						if r["subtype"] == "error" {
							send(Frame{ID: initID, Error: &RPCError{Code: -32000, Message: String(r["error"])}})
						} else {
							reply(initID, map[string]any{})
						}
					}
				case "control_request":
					req := Map(v["request"])
					id := String(v["request_id"])
					if req["subtype"] == "mcp_message" {
						msg := Map(req["message"])
						var result any = map[string]any{}
						switch String(msg["method"]) {
						case "initialize":
							result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "warden", "version": "1"}}
						case "tools/list":
							list := []any{}
							for _, x := range tools {
								t := Map(x)
								list = append(list, map[string]any{"name": t["name"], "description": t["description"], "inputSchema": t["inputSchema"]})
							}
							result = map[string]any{"tools": list}
						case "tools/call":
							rid, _ := json.Marshal("claude-" + id)
							pending[string(rid)] = v
							p := Map(msg["params"])
							send(Frame{ID: rid, Method: "item/tool/call", Params: map[string]any{"tool": p["name"], "arguments": p["arguments"], "callId": id}})
							continue
						}
						control(id, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": result}})
					} else if req["subtype"] == "can_use_tool" {
						if req["tool_name"] != "AskUserQuestion" {
							// The SBX guest is the security boundary, as with
							// Codex's dangerFullAccess: built-in tools act only
							// inside the guest, network egress goes through the
							// Warden proxy, and every Warden MCP tool obtains
							// owner approval itself before granting access.
							// A second per-call prompt here adds no control.
							control(id, map[string]any{"behavior": "allow", "updatedInput": req["input"]})
							continue
						}
						rid, _ := json.Marshal("claude-" + id)
						pending[string(rid)] = v
						questions := []any{}
						for i, q := range Array(Map(req["input"])["questions"]) {
							question := Map(q)
							questions = append(questions, map[string]any{"id": fmt.Sprintf("q%d", i), "header": question["header"], "question": question["question"], "options": question["options"]})
						}
						send(Frame{ID: rid, Method: "item/tool/requestUserInput", Params: map[string]any{"questions": questions}})
					} else {
						control(id, map[string]any{"behavior": "deny", "message": "Unsupported Warden control request"})
					}
				case "system":
					switch v["subtype"] {
					case "init":
						thread = String(v["session_id"])
						ctx2.model(String(v["model"]))
						event("thread/started", map[string]any{"thread": map[string]any{"id": thread}})
					case "status":
						// The CLI compacting its context (/compact, or on its
						// own near the window): a compaction item runs from
						// the status to the boundary, or to the failure.
						if v["status"] == "compacting" {
							if compaction == "" {
								compaction = claudeID()
								event("item/started", map[string]any{"turnId": turn, "item": map[string]any{"id": compaction, "type": "compaction", "status": "running"}})
							}
						} else if v["compact_result"] == "failed" && compaction != "" {
							event("item/completed", map[string]any{"turnId": turn, "item": map[string]any{"id": compaction, "type": "compaction", "status": "failed", "error": String(v["compact_error"])}})
							compaction, compactionMeta = "", nil
						}
					case "compact_boundary":
						if compaction == "" {
							compaction = claudeID()
						}
						compactionMeta = Map(v["compact_metadata"])
						event("item/completed", map[string]any{"turnId": turn, "item": claudeCompactionItem(compaction, compactionMeta, "")})
						ctx2.compacted(compactionMeta)
						reportContext()
					}
				case "stream_event":
					e := Map(v["event"])
					switch e["type"] {
					case "content_block_start":
						if Map(e["content_block"])["type"] == "thinking" {
							flushThinking()
							thinkingID = claudeID()
							event("item/started", map[string]any{"turnId": turn, "item": map[string]any{"id": thinkingID, "type": "reasoning", "summary": []any{}}})
						}
					case "content_block_delta":
						d := Map(e["delta"])
						switch d["type"] {
						case "text_delta":
							if text == "" {
								event("item/started", map[string]any{"turnId": turn, "item": map[string]any{"id": textID, "type": "agentMessage", "text": ""}})
							}
							delta := String(d["text"])
							text += delta
							event("item/agentMessage/delta", map[string]any{"turnId": turn, "itemId": textID, "delta": delta})
						case "thinking_delta":
							if thinkingID == "" {
								thinkingID = claudeID()
								event("item/started", map[string]any{"turnId": turn, "item": map[string]any{"id": thinkingID, "type": "reasoning", "summary": []any{}}})
							}
							delta := String(d["thinking"])
							thinking += delta
							event("item/reasoning/summaryTextDelta", map[string]any{"turnId": turn, "itemId": thinkingID, "delta": delta})
						}
					case "content_block_stop":
						// Only a thinking block is tracked to its stop; text ends
						// with the message or the next tool.
						flushThinking()
					}
				case "assistant":
					flushThinking()
					if ctx2.call(Map(v["message"])) {
						reportContext()
					}
					for _, x := range Array(Map(v["message"])["content"]) {
						b := Map(x)
						if b["type"] == "tool_use" {
							flushText()
							// Each tool call is a typed item (claude_tools.go):
							// started here with what the call asks, completed by
							// its result below.
							id := String(b["id"])
							t := claudeTool{name: String(b["name"]), input: Map(b["input"])}
							toolCalls[id] = t
							event("item/started", map[string]any{"turnId": turn, "item": claudeToolItem(id, t, nil, nil)})
						}
					}
				case "user":
					if compaction != "" && v["isSynthetic"] == true {
						// The summary the CLI continues from, sent as a
						// synthetic user message right after the boundary.
						event("item/completed", map[string]any{"turnId": turn, "item": claudeCompactionItem(compaction, compactionMeta, claudeMessageText(Map(v["message"])))})
						compaction, compactionMeta = "", nil
						continue
					}
					results := []map[string]any{}
					for _, x := range Array(Map(v["message"])["content"]) {
						if b := Map(x); b["type"] == "tool_result" {
							results = append(results, b)
						}
					}
					for _, b := range results {
						id := String(b["tool_use_id"])
						t := toolCalls[id]
						delete(toolCalls, id)
						// The CLI's structured result rides on the frame, so it
						// belongs to a lone result only.
						var structured any
						if len(results) == 1 {
							structured = v["tool_use_result"]
						}
						event("item/completed", map[string]any{"turnId": turn, "item": claudeToolItem(id, t, b, structured)})
					}
				case "result":
					// Only a turn that streamed nothing falls back to the summary
					// result; otherwise it would duplicate the last text block.
					if text == "" && !streamed {
						text = String(v["result"])
					}
					flushThinking()
					flushText()
					status := "completed"
					if interrupting {
						status = "interrupted"
						interrupting = false
					} else if v["is_error"] == true {
						status = "failed"
						event("error", map[string]any{"error": map[string]any{"message": claudeResultError(v)}})
					}
					if last, ok := claudeTurnUsage(v, total); ok {
						total = total.add(last)
						event("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": turn, "tokenUsage": map[string]any{"last": last.params(), "total": total.params()}})
					}
					ctx2.result(v)
					reportContext()
					compaction, compactionMeta = "", nil
					event("turn/completed", map[string]any{"turn": map[string]any{"id": turn, "status": status}})
				}
			}
		}
	}()
	return client
}
func claudeID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func claudeResultError(v map[string]any) string {
	if errors := Array(v["errors"]); len(errors) > 0 {
		return fmt.Sprint(errors)
	}
	if result := String(v["result"]); result != "" {
		return result
	}
	return "Claude could not complete this turn"
}

// claudeUsage is a token count in the shape of Codex's TokenUsageBreakdown
// plus the cost, which Claude Code estimates and Codex does not.
type claudeUsage struct {
	input, cached, cacheWrite, output, reasoning int64
	cost                                         float64
}

func (u claudeUsage) add(v claudeUsage) claudeUsage {
	return claudeUsage{u.input + v.input, u.cached + v.cached, u.cacheWrite + v.cacheWrite, u.output + v.output, u.reasoning + v.reasoning, u.cost + v.cost}
}
func (u claudeUsage) params() map[string]any {
	return map[string]any{"inputTokens": u.input, "cachedInputTokens": u.cached, "cacheWriteInputTokens": u.cacheWrite, "outputTokens": u.output, "reasoningOutputTokens": u.reasoning, "totalTokens": u.input + u.output, "costUSD": u.cost}
}

// claudeTurnUsage reads what a turn cost from Claude Code's `result`: with
// streamed input, `usage` is the turn's own (main-loop) tokens, where
// `input_tokens` excludes the cached and cache-written ones, while
// `total_cost_usd` is the running estimate for the whole process, so the
// turn's cost is its growth over `sofar`. False when the result reports no
// usage (a crash result carries none).
func claudeTurnUsage(v map[string]any, sofar claudeUsage) (claudeUsage, bool) {
	usage := Map(v["usage"])
	if usage == nil {
		return claudeUsage{}, false
	}
	n := func(k string) int64 { f, _ := usage[k].(float64); return int64(f) }
	u := claudeUsage{cached: n("cache_read_input_tokens"), cacheWrite: n("cache_creation_input_tokens"), output: n("output_tokens")}
	u.input = n("input_tokens") + u.cached + u.cacheWrite
	// The thinking tokens are part of the output, as Codex counts them.
	if f, ok := Map(usage["output_tokens_details"])["thinking_tokens"].(float64); ok {
		u.reasoning = int64(f)
	}
	if cost, ok := v["total_cost_usd"].(float64); ok && cost > sofar.cost {
		u.cost = cost - sofar.cost
	}
	return u, true
}

// claudeCompactionItem is the transcript item for a compaction: the
// boundary's trigger (manual for /compact, auto) and the context before
// and after it in tokens, and the summary the CLI continues from once it
// follows (the metadata is sent again with it, as the conversation
// replaces the item whole).
func claudeCompactionItem(id string, meta map[string]any, summary string) map[string]any {
	item := map[string]any{"id": id, "type": "compaction", "status": "completed"}
	if meta != nil {
		item["trigger"] = String(meta["trigger"])
		item["preTokens"] = meta["pre_tokens"]
		item["postTokens"] = meta["post_tokens"]
	}
	if summary != "" {
		item["summary"] = summary
	}
	return item
}

// claudeMessageText is the text of a CLI message whose content is either
// a string or a list of text blocks.
func claudeMessageText(m map[string]any) string {
	if s, ok := m["content"].(string); ok {
		return s
	}
	parts := []string{}
	for _, b := range Array(m["content"]) {
		if block := Map(b); block["type"] == "text" {
			parts = append(parts, String(block["text"]))
		}
	}
	return strings.Join(parts, "\n")
}

// claudeContext tracks how full the model's context is. Every assistant
// frame carries its API call's usage, whose input, cache-read and
// cache-written tokens together are the prompt that call was given: the
// context length. (The result's usage is the turn's calls summed, so it
// cannot say.) The window comes from the result's modelUsage once the
// CLI has reported one, and from claudeContextWindow before that. After a
// compaction the CLI reports only the summary's size (post_tokens); the
// context is that plus the fixed prefix — the system prompt and tools,
// estimated as the smallest context the process has seen — until the
// next call says.
type claudeContext struct {
	used, window, prefix int64
	name                 string
	// reported is what the last notification said, so one goes out only
	// on a change.
	reportedUsed, reportedWindow int64
}

func (c *claudeContext) model(name string) {
	if name != "" {
		c.name = name
	}
	if c.window == 0 && c.name != "" {
		c.window = claudeContextWindow(c.name)
	}
}

// call records an assistant frame's message; false when it carries no
// usage (the CLI's synthetic messages).
func (c *claudeContext) call(m map[string]any) bool {
	if String(m["model"]) == "<synthetic>" {
		return false
	}
	u := Map(m["usage"])
	if u == nil {
		return false
	}
	n := func(k string) int64 { f, _ := u[k].(float64); return int64(f) }
	used := n("input_tokens") + n("cache_creation_input_tokens") + n("cache_read_input_tokens")
	if used == 0 {
		return false
	}
	c.used = used
	if c.prefix == 0 || used < c.prefix {
		c.prefix = used
	}
	c.model(String(m["model"]))
	return true
}

// compacted re-estimates the context from a compact_boundary's metadata.
func (c *claudeContext) compacted(meta map[string]any) {
	post, _ := meta["post_tokens"].(float64)
	if post > 0 {
		c.used = int64(post) + c.prefix
	}
}

// result reads the model's window from a result's modelUsage: the entry
// for the session's model, else the only one.
func (c *claudeContext) result(v map[string]any) {
	models := Map(v["modelUsage"])
	if models == nil {
		return
	}
	pick := Map(models[c.name])
	if pick == nil && len(models) == 1 {
		for _, m := range models {
			pick = Map(m)
		}
	}
	if w, ok := pick["contextWindow"].(float64); ok && w > 0 {
		c.window = int64(w)
	}
}

// changed is the notification's params when the context differs from the
// last one sent.
func (c *claudeContext) changed() (map[string]any, bool) {
	if c.used == 0 || (c.used == c.reportedUsed && c.window == c.reportedWindow) {
		return nil, false
	}
	c.reportedUsed, c.reportedWindow = c.used, c.window
	return map[string]any{"used": c.used, "window": c.window, "model": c.name}, true
}

// claudeContextWindow is the context window of a model as the pinned CLI
// (2.1.272) knows it, for the calls before the first result reports it:
// 1M for the `[1m]` variants and the models that are natively 1M
// (sonnet 4.6, opus 4.6 and later, opus 5, fable 5), 200k otherwise.
func claudeContextWindow(model string) int64 {
	m := strings.ToLower(model)
	if strings.Contains(m, "[1m]") {
		return 1_000_000
	}
	for _, id := range []string{"sonnet-4-6", "opus-4-6", "opus-4-7", "opus-4-8", "opus-4-9", "opus-5", "fable-5"} {
		if strings.Contains(m, id) {
			return 1_000_000
		}
	}
	return 200_000
}
