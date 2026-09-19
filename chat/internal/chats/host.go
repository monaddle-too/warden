package chats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
)

// The jailbreak (docs/host-dogfood-plan.md, Part B): with dogfood.jailbreak
// on (Engine.Jailbreak, a local owner install only), the owner may opt a
// workspace into host access (Chat.Jailbroken on every chat of it, set at
// creation or from the workspace panel). Its agent then holds the host_*
// tools on the warden MCP server, each a runner operation on this machine
// as the owner (sandbox/host.go), each a transcript card marked HOST, each
// an audit entry through the policy service, and each subject to the
// chat's permission mode and rules exactly like any other MCP tool
// (mcp__warden__host_run under auto runs unprompted, under ask is a card,
// a rule on it decides first). A call on a workspace that is not
// jailbroken is refused here, before the runner, which refuses the family
// itself unless it runs with --jailbreak.
//
// The tool list is what the agent's session took at its start
// (dynamicTools on thread/start and thread/resume): turning host access
// off removes the tools at the next session start, and refuses every
// call at once.

// hostTools lists the host tools of a jailbroken workspace.
func hostTools() []any {
	str := func(desc string, max int) map[string]any {
		return map[string]any{"type": "string", "description": desc, "maxLength": max}
	}
	return []any{
		map[string]any{"type": "function", "name": "host_run", "description": "Run a shell command on the owner's computer (the host this Warden runs on), as the owner, through their login shell. This workspace has host access: the command runs outside the sandbox with the owner's own files, tools and network. Output is stdout and stderr merged, the last 30000 characters. Use cwd for the directory (default: the home directory) and timeout in seconds (default 600, at most 3600); a Stop from the owner kills it. Every call is shown to the owner and recorded.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"command": str("the shell command line", sandbox.MaxHostCommand), "cwd": str("absolute directory on the host to run in (default: the home directory)", 1024), "timeout": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600, "description": "seconds before the command is killed (default 600)"}}, "required": []string{"command"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "host_put", "description": "Copy a file or directory from this sandbox to the owner's computer. from is an absolute path in the sandbox, to an absolute path on the host under the owner's home directory (Warden's own state directory is refused; an existing directory at to receives the copy inside it). Up to 512 MiB.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"from": str("absolute path in the sandbox", 4096), "to": str("absolute path on the host, under the home directory", 4096)}, "required": []string{"from", "to"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "host_get", "description": "Copy a file or directory from the owner's computer into this sandbox. from is an absolute path on the host under the owner's home directory (Warden's own state directory is refused), to an absolute path in the sandbox (its parent is created). Up to 512 MiB.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"from": str("absolute path on the host, under the home directory", 4096), "to": str("absolute path in the sandbox", 4096)}, "required": []string{"from", "to"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "host_expose", "description": "Publish a port that a server on the owner's computer listens on (127.0.0.1:<port> on the host, for example a second Warden you started there) as a preview of this chat, so the owner opens it from the preview list. Returns the preview URL. Only for servers on the host; a server inside this sandbox uses preview_attach.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535, "description": "the host loopback port"}, "name": str("what the preview is called (default: Host port N)", 160)}, "required": []string{"port"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "host_status", "description": "Describe the owner's computer as Warden sees it: OS and architecture, the home directory, this Warden's state directory, and the Warden instances installed there (~/.warden and ~/.warden-<name>: name, state directory, release, whether it is running).", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
	}
}

// hostToolNames are the tools hostTools lists, by name.
var hostToolNames = map[string]bool{"host_run": true, "host_put": true, "host_get": true, "host_expose": true, "host_status": true}

// IsHostTool reports whether name is one of the jailbreak's tools.
func IsHostTool(name string) bool { return hostToolNames[name] }

// hostRunTool is host_run as the CLI names it in a permission ask and a
// rule pattern; a rule on it takes a command spec like a Bash rule
// (mcp__warden__host_run(warden *)).
const hostRunTool = "mcp__warden__host_run"

// errHostAccessOff is the refusal for a host tool call on a workspace
// without host access.
var errHostAccessOff = errors.New("host access is off for this workspace")

// hostCall is a host tool call in flight, cancellable by Stop.
type hostCall struct{ cancel context.CancelFunc }

// trackHost registers a host call of the chat and returns a context Stop
// can end together with the untrack function.
func (e *Engine) trackHost(ctx context.Context, chatID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	call := &hostCall{cancel: cancel}
	e.hostMu.Lock()
	if e.hostCalls == nil {
		e.hostCalls = map[string]map[*hostCall]struct{}{}
	}
	if e.hostCalls[chatID] == nil {
		e.hostCalls[chatID] = map[*hostCall]struct{}{}
	}
	e.hostCalls[chatID][call] = struct{}{}
	e.hostMu.Unlock()
	return ctx, func() {
		cancel()
		e.hostMu.Lock()
		delete(e.hostCalls[chatID], call)
		if len(e.hostCalls[chatID]) == 0 {
			delete(e.hostCalls, chatID)
		}
		e.hostMu.Unlock()
	}
}

// cancelHostCalls ends the chat's host calls in flight (Stop: the turn is
// interrupted, the host command must die with it; the runner kills the
// process group when the request's connection closes).
func (e *Engine) cancelHostCalls(chatID string) int {
	e.hostMu.Lock()
	defer e.hostMu.Unlock()
	n := 0
	for call := range e.hostCalls[chatID] {
		call.cancel()
		n++
	}
	return n
}

// hostTool answers a host_* call frame with hostCall's result. A host
// command may run for an hour, so the call is answered off the run's
// frame loop (the reply is safe from any goroutine): the agent's other
// tools and its stream go on meanwhile, and Stop ends the call.
func (e *Engine) hostTool(ctx context.Context, c *Chat, client *agent.Client, f agent.Frame) error {
	name := agent.String(f.Params["tool"])
	raw, _ := json.Marshal(f.Params["arguments"])
	if s, ok := f.Params["arguments"].(string); ok {
		raw = []byte(s)
	}
	chat := *c
	e.background.Add(1)
	go func() {
		defer e.background.Done()
		defer e.Bugs.Recover("host tool " + name)
		value, err := e.hostCall(ctx, &chat, name, raw)
		_ = client.Reply(f.ID, toolResult(value, err))
	}()
	return nil
}

// hostCall performs a host_* call: refused unless this Warden and the
// workspace have host access, else the runner operation, its result as
// the tool's, and an audit entry.
func (e *Engine) hostCall(ctx context.Context, c *Chat, name string, raw []byte) (any, error) {
	reply := func(value any, err error) (any, error) { return value, err }
	if !e.Jailbreak || !c.Jailbroken {
		return reply(nil, errHostAccessOff)
	}
	var in struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Timeout int    `json:"timeout"`
		From    string `json:"from"`
		To      string `json:"to"`
		Port    int    `json:"port"`
		Name    string `json:"name"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil && !(name == "host_status" && strings.TrimSpace(string(raw)) == "") {
		return reply(nil, errors.New("invalid arguments: "+err.Error()))
	}
	ctx, done := e.trackHost(ctx, c.ID)
	defer done()
	who := "agent"
	switch name {
	case "host_run":
		in.Command = strings.TrimSpace(in.Command)
		if in.Command == "" || len(in.Command) > sandbox.MaxHostCommand {
			return reply(nil, errors.New("a command of at most 16384 characters is required"))
		}
		if in.Timeout < 0 || in.Timeout > 3600 {
			return reply(nil, errors.New("timeout is 1–3600 seconds"))
		}
		r := request(c, "host.exec")
		r.Command, r.Directory, r.Timeout = in.Command, in.Cwd, in.Timeout
		started := e.at()
		res, err := e.Worker.Call(ctx, r)
		if err != nil {
			e.hostAudit(c, "host.exec", map[string]any{"principal": who, "cwd": in.Cwd, "command": in.Command, "error": err.Error()})
			return reply(nil, err)
		}
		if res.Exec == nil {
			return reply(nil, errors.New("the runner gave no result"))
		}
		x := res.Exec
		e.hostAudit(c, "host.exec", map[string]any{"principal": who, "cwd": in.Cwd, "command": in.Command, "exit": x.ExitCode, "timed_out": x.TimedOut, "bytes": x.Bytes, "duration_ms": x.DurationMS, "started": started})
		out := map[string]any{"exitCode": x.ExitCode, "timedOut": x.TimedOut, "output": x.Output, "durationMS": x.DurationMS}
		if in.Cwd != "" {
			out["cwd"] = in.Cwd
		}
		return reply(out, nil)
	case "host_put", "host_get":
		in.From, in.To = strings.TrimSpace(in.From), strings.TrimSpace(in.To)
		if in.From == "" || in.To == "" {
			return reply(nil, errors.New("from and to are required"))
		}
		r := request(c, "host.put")
		r.Directory, r.Path = in.From, in.To
		direction := "sandbox → host"
		if name == "host_get" {
			r.Operation, r.Directory, r.Path = "host.get", in.To, in.From
			direction = "host → sandbox"
		}
		res, err := e.Worker.Call(ctx, r)
		fields := map[string]any{"principal": who, "direction": direction, "from": in.From, "to": in.To}
		if err != nil {
			fields["error"] = err.Error()
			e.hostAudit(c, "host.file", fields)
			return reply(nil, err)
		}
		fields["bytes"] = res.Size
		e.hostAudit(c, "host.file", fields)
		return reply(map[string]any{"from": in.From, "to": in.To, "bytes": res.Size, "direction": direction}, nil)
	case "host_expose":
		if in.Port < 1 || in.Port > 65535 {
			return reply(nil, errors.New("a host port from 1 to 65535 is required"))
		}
		if e.PublicPreviewSuffix == "" {
			return reply(nil, errors.New("previews are not configured on this Warden"))
		}
		title := strings.TrimSpace(in.Name)
		if title == "" {
			title = fmt.Sprintf("Host port %d", in.Port)
		}
		if len(title) > 160 {
			title = title[:160]
		}
		input := portInput{Port: in.Port, Path: "/", Title: title, Upstream: sandbox.UpstreamHost}
		// An approved binding of this host port is reused, as a sandbox
		// port's is (ports.go).
		id := ""
		for _, p := range e.Store.Snapshot().Ports {
			if p.ChatID == c.ID && p.SandboxID == c.SandboxID && p.Port == in.Port && p.Upstream == sandbox.UpstreamHost && p.State == "approved" {
				id = p.ID
			}
		}
		binding, err := e.bindPort(c, input, id)
		fields := map[string]any{"principal": who, "port": in.Port, "title": title}
		if err != nil {
			fields["error"] = err.Error()
			e.hostAudit(c, "host.expose", fields)
			return reply(nil, err)
		}
		fields["url"] = binding.URL
		e.hostAudit(c, "host.expose", fields)
		return reply(map[string]any{"port": in.Port, "url": binding.URL, "title": title, "note": "the owner opens it from the chat's preview list; the binding is signed-in Warden users only"}, nil)
	case "host_status":
		res, err := e.Worker.Call(ctx, request(c, "host.status"))
		if err != nil {
			return reply(nil, err)
		}
		if res.Host == nil {
			return reply(nil, errors.New("the runner gave no result"))
		}
		return reply(res.Host, nil)
	}
	return reply(nil, errors.New("unknown host tool"))
}

// hostAudit records a host event through the policy service, so it is in
// the audit hash chain (sharing/host_event); a failure is logged, never
// fatal to the call, since the runner's own record and the transcript
// stand.
func (e *Engine) hostAudit(c *Chat, event string, fields map[string]any) {
	data := map[string]any{"event": event, "chatID": c.ID, "sandboxID": c.SandboxID}
	for k, v := range fields {
		data[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := e.sharingCall(ctx, "host_event", data); err != nil {
		log.Printf("host audit %s for chat %s: %v", event, c.ID, err)
	}
}

// SetWorkspaceJailbreak turns host access on or off for workspace id (a
// sandbox ID) by actor: every chat of it records the choice, and the
// policy service audits it. Turning it on needs this Warden's
// dogfood.jailbreak; turning it off is always possible. The running
// session keeps the tools it started with until it next starts; every
// call is refused at once.
func (e *Engine) SetWorkspaceJailbreak(ctx context.Context, id string, on bool, actor cv.Actor) error {
	if on && !e.Jailbreak {
		return errors.New("host access is off on this Warden (dogfood.jailbreak in warden.json, a local owner install only)")
	}
	st := e.Store.Snapshot()
	chats := st.environmentChats(id)
	if len(chats) == 0 || st.deleted(id) {
		return errors.New("workspace not found")
	}
	if chats[0].Jailbroken == on {
		return nil
	}
	err := e.Store.update(func(st *State) error {
		for _, c := range st.Chats {
			if c.SandboxID == id {
				c.Jailbroken = on
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	e.hostAudit(chats[0], "workspace.jailbreak", map[string]any{"on": on, "actor": sharingActor(actor), "principal": actor.PrincipalID})
	e.touch()
	return nil
}

// createdJailbroken gives the fresh workspace of a chat CreateFrom just
// made host access. A shared workspace already has its setting; a refusal
// discards the never-used chat and is returned as the creation's error.
func (e *Engine) createdJailbroken(ctx context.Context, id string, actor cv.Actor) (string, error) {
	c := e.Store.Chat(id)
	if c == nil {
		return "", errors.New("chat not found")
	}
	if len(e.Store.Snapshot().environmentChats(c.SandboxID)) > 1 {
		e.discardUnused(id)
		return "", errors.New("a shared workspace already has its host access setting")
	}
	if err := e.SetWorkspaceJailbreak(ctx, c.SandboxID, true, actor); err != nil {
		e.discardUnused(id)
		return "", err
	}
	return id, nil
}
