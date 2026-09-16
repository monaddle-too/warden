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
			scan.Buffer(make([]byte, 65536), 8<<20)
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
		toolCommands := map[string]string{}
		textID := ""
		text := ""
		streamed := false
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
						content = append(content, map[string]any{"type": "text", "text": String(Map(v)["text"])})
					}
					_ = cli.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}, "parent_tool_use_id": nil})
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
					if v["subtype"] == "init" {
						thread = String(v["session_id"])
						event("thread/started", map[string]any{"thread": map[string]any{"id": thread}})
					}
				case "stream_event":
					e := Map(v["event"])
					if e["type"] == "content_block_delta" {
						d := Map(e["delta"])
						if d["type"] == "text_delta" {
							if text == "" {
								event("item/started", map[string]any{"turnId": turn, "item": map[string]any{"id": textID, "type": "agentMessage", "text": ""}})
							}
							delta := String(d["text"])
							text += delta
							event("item/agentMessage/delta", map[string]any{"turnId": turn, "itemId": textID, "delta": delta})
						}
					}
				case "assistant":
					for _, x := range Array(Map(v["message"])["content"]) {
						b := Map(x)
						if b["type"] == "tool_use" {
							flushText()
							command := fmt.Sprintf("%s: %v", b["name"], b["input"])
							toolCommands[String(b["id"])] = command
							event("item/started", map[string]any{"turnId": turn, "item": map[string]any{"id": b["id"], "type": "commandExecution", "command": command, "status": "running"}})
						}
					}
				case "user":
					for _, x := range Array(Map(v["message"])["content"]) {
						b := Map(x)
						if b["type"] == "tool_result" {
							id := String(b["tool_use_id"])
							status := "completed"
							if b["is_error"] == true {
								status = "failed"
							}
							content := String(b["content"])
							if content == "" {
								data, _ := json.Marshal(b["content"])
								content = string(data)
							}
							event("item/completed", map[string]any{"turnId": turn, "item": map[string]any{"id": id, "type": "commandExecution", "command": toolCommands[id], "status": status, "aggregatedOutput": content}})
						}
					}
				case "result":
					// Only a turn that streamed nothing falls back to the summary
					// result; otherwise it would duplicate the last text block.
					if text == "" && !streamed {
						text = String(v["result"])
					}
					flushText()
					status := "completed"
					if v["is_error"] == true {
						status = "failed"
						event("error", map[string]any{"error": map[string]any{"message": claudeResultError(v)}})
					}
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
