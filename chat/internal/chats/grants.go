package chats

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
)

// Owner-approved grants an agent can ask for. Each tool creates a pending
// approval on the chat (shown in the web UI, the terminal client and the
// popups) and the agent's tool call stays open until the owner answers;
// the answer performs the effect and becomes the tool result. Nothing is
// granted, and no credential moves, before the approval.
//
//   request_network_access    warden/network/allow    one public host, for a bounded time, this sandbox
//   request_repository_access warden/repository/access share or widen a repository's read categories
//   github_write              warden/github/write     one comment, issue or label set, posted by Warden
//   request_host_directory    warden/host/import      copy a host directory into the sandbox (local mode)
//   sync_host_directory       warden/host/export      copy it back over the host directory (local mode)
//   request_resources         warden/sandbox/resources more CPU or memory for the workspace (resources.go)

const (
	methodNetworkAllow     = "warden/network/allow"
	methodRepositoryAccess = "warden/repository/access"
	methodGitHubWrite      = "warden/github/write"
	methodHostImport       = "warden/host/import"
	methodHostExport       = "warden/host/export"
	methodResources        = "warden/sandbox/resources"
)

var grantMethods = map[string]string{
	"request_network_access":    methodNetworkAllow,
	"request_repository_access": methodRepositoryAccess,
	"github_write":              methodGitHubWrite,
	"request_host_directory":    methodHostImport,
	"sync_host_directory":       methodHostExport,
	"request_resources":         methodResources,
}

func isGrantMethod(method string) bool {
	for _, m := range grantMethods {
		if m == method {
			return true
		}
	}
	return false
}

// grantTools lists the request tools; the host directory ones exist only
// on a local install, where the owner's own files are the point.
func grantTools(local bool) []any {
	str := func(desc string, max int) map[string]any {
		return map[string]any{"type": "string", "description": desc, "maxLength": max}
	}
	tools := []any{
		map[string]any{"type": "function", "name": "request_network_access", "description": "Ask the owner to let this sandbox reach one public website host (HTTP or HTTPS on ports 80 and 443) for a limited time through Warden's gateway, when the network policy refused it. Give the exact hostname without scheme or path, why you need it, and for how long. Waits for the decision; on approval retry the request. No credential is ever attached to a host allowed this way.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"host": str("hostname, e.g. pypi.org", 253), "reason": str("why you need it", 500), "duration_minutes": map[string]any{"type": "integer", "minimum": 1, "maximum": 1440, "description": "how long, in minutes (default 60)"}}, "required": []string{"host", "reason"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "request_repository_access", "description": "Ask the owner to share a GitHub repository with this workspace, or to widen the read categories of one already shared: contents (code, branches, commits, clone), issues (issues, comments, labels, milestones), pull_requests (pull requests, their files and reviews). Read-only; writes need github_write or request_pull_request. Waits for the decision; a request the workspace already satisfies is answered at once without asking, and a failure says why (for example a GitHub sign-in the owner must refresh).", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"repository": str("owner/name", 200), "categories": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"contents", "issues", "pull_requests"}}, "minItems": 1, "maxItems": 3}, "reason": str("why you need it", 500)}, "required": []string{"repository", "categories", "reason"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "github_write", "description": "Perform one small GitHub write on a repository shared with this workspace, after the owner approves the exact payload: comment_issue or comment_pull_request (number, body), create_issue (title, body), add_labels (number, labels). Warden posts it with the owner's credential; you never hold a token. Waits for the decision and returns the created URL. Larger changes go through request_pull_request.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"repository": str("owner/name", 200), "action": map[string]any{"type": "string", "enum": []string{"comment_issue", "comment_pull_request", "create_issue", "add_labels"}}, "number": map[string]any{"type": "integer", "minimum": 1, "description": "issue or pull request number"}, "title": str("issue title (create_issue)", 256), "body": str("Markdown body", 65536), "labels": map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": 50}, "maxItems": 20}}, "required": []string{"repository", "action"}, "additionalProperties": false}},
		map[string]any{"type": "function", "name": "request_resources", "description": "Ask the owner for more CPU or memory for this workspace when a task needs it (a build that is killed for memory, a test suite that needs cores). Give the total you want (not the increase), at least one of cpus and memory_mb, and why; only more can be asked for. Waits for the decision. Where Warden runs on SBX the resize restarts the sandbox: your process ends, files and the conversation are kept, and Warden resumes the chat afterwards with a note; elsewhere the new limit applies at once.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"cpus": map[string]any{"type": "number", "minimum": 0.25, "maximum": 64, "description": "CPUs in total, e.g. 2 (whole numbers on SBX)"}, "memory_mb": map[string]any{"type": "integer", "minimum": 512, "maximum": 65536, "description": "memory in MiB in total, a multiple of 512, e.g. 4096"}, "reason": str("why you need it", 500)}, "required": []string{"reason"}, "additionalProperties": false}},
	}
	if local {
		tools = append(tools,
			map[string]any{"type": "function", "name": "request_host_directory", "description": "Ask the owner to copy a directory from their computer into this sandbox at /home/agent/host/<name> (a snapshot you can read and change; up to 1 GiB). Give the absolute path on their computer and why. Waits for the decision. Use sync_host_directory to copy your changes back.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": str("absolute directory path on the owner's computer", 1024), "reason": str("why you need it", 500)}, "required": []string{"path", "reason"}, "additionalProperties": false}},
			map[string]any{"type": "function", "name": "sync_host_directory", "description": "Ask the owner to copy /home/agent/host/<name> back over the original directory on their computer, merging file by file: existing files are overwritten, nothing is deleted. Give the same absolute path used with request_host_directory. Waits for the decision.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": str("absolute directory path on the owner's computer", 1024)}, "required": []string{"path"}, "additionalProperties": false}},
		)
	}
	return tools
}

// requestGrant validates a grant tool call and parks it as a pending
// approval; Resolve answers it.
func (e *Engine) requestGrant(c *Chat, client *agent.Client, f agent.Frame) error {
	name := agent.String(f.Params["tool"])
	method := grantMethods[name]
	// fail answers the agent with the validation error (tests pass no client
	// and get the error back).
	fail := func(msg string) error {
		err := errors.New(msg)
		if client == nil {
			return err
		}
		return client.Reply(f.ID, toolResult(nil, err))
	}
	if (method == methodHostImport || method == methodHostExport) && !e.LocalMode {
		return fail("host directories are only available on a local Warden install")
	}
	raw, _ := json.Marshal(f.Params["arguments"])
	if s, ok := f.Params["arguments"].(string); ok {
		raw = []byte(s)
	}
	var in struct {
		Host       string   `json:"host"`
		Reason     string   `json:"reason"`
		Duration   int      `json:"duration_minutes"`
		Repository string   `json:"repository"`
		Categories []string `json:"categories"`
		Action     string   `json:"action"`
		Number     int      `json:"number"`
		Title      string   `json:"title"`
		Body       string   `json:"body"`
		Labels     []string `json:"labels"`
		Path       string   `json:"path"`
		CPUs       float64  `json:"cpus"`
		MemoryMB   int      `json:"memory_mb"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fail("invalid arguments: " + err.Error())
	}
	in.Reason = strings.TrimSpace(in.Reason)
	params := map[string]any{}
	switch method {
	case methodNetworkAllow:
		in.Host = strings.TrimRight(strings.ToLower(strings.TrimSpace(in.Host)), ".")
		if in.Duration == 0 {
			in.Duration = 60
		}
		if in.Host == "" || strings.ContainsAny(in.Host, "/: ") || in.Reason == "" || in.Duration < 1 || in.Duration > 1440 {
			return fail("host (no scheme or path), reason and 1–1440 minutes are required")
		}
		params = map[string]any{"host": in.Host, "reason": in.Reason, "duration_minutes": in.Duration}
	case methodRepositoryAccess:
		in.Repository = strings.ToLower(strings.TrimSpace(in.Repository))
		valid := map[string]bool{"contents": true, "issues": true, "pull_requests": true}
		seen := map[string]bool{}
		var categories []any
		for _, cat := range in.Categories {
			if !valid[cat] || seen[cat] {
				return fail("categories are contents, issues and pull_requests")
			}
			seen[cat] = true
			categories = append(categories, cat)
		}
		if !strings.Contains(in.Repository, "/") || len(categories) == 0 || in.Reason == "" {
			return fail("repository (owner/name), at least one category and a reason are required")
		}
		// A request the workspace already satisfies changes nothing, so it
		// is answered at once instead of asking the owner; a missing
		// connection is reported now rather than after their approval.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		current, err := e.repositoryAccess(ctx, c)
		cancel()
		if err != nil {
			return fail("cannot check the repositories shared with this workspace: " + githubRemedy(err, e.LocalMode))
		}
		if have, shared := current[in.Repository]; shared && coversCategories(have, in.Categories) {
			if client == nil {
				return nil
			}
			return client.Reply(f.ID, toolResult(map[string]any{"repository": in.Repository, "access": have, "already_shared": true, "note": "this workspace already has read access to " + strings.Join(in.Categories, ", ") + " of " + in.Repository + "; no approval was needed. Use list_shared_repositories to see every shared repository and its categories."}, nil))
		}
		params = map[string]any{"repository": in.Repository, "categories": categories, "reason": in.Reason}
	case methodGitHubWrite:
		in.Repository = strings.ToLower(strings.TrimSpace(in.Repository))
		if !strings.Contains(in.Repository, "/") {
			return fail("repository (owner/name) is required")
		}
		params = map[string]any{"repository": in.Repository, "action": in.Action}
		switch in.Action {
		case "comment_issue", "comment_pull_request":
			if in.Number < 1 || strings.TrimSpace(in.Body) == "" {
				return fail("number and body are required")
			}
			params["number"], params["body"] = in.Number, in.Body
		case "create_issue":
			if strings.TrimSpace(in.Title) == "" {
				return fail("title is required")
			}
			params["title"], params["body"] = strings.TrimSpace(in.Title), in.Body
		case "add_labels":
			if in.Number < 1 || len(in.Labels) == 0 {
				return fail("number and labels are required")
			}
			labels := []any{}
			for _, l := range in.Labels {
				labels = append(labels, l)
			}
			params["number"], params["labels"] = in.Number, labels
		default:
			return fail("action must be comment_issue, comment_pull_request, create_issue or add_labels")
		}
	case methodHostImport, methodHostExport:
		in.Path = strings.TrimSpace(in.Path)
		if !strings.HasPrefix(in.Path, "/") || (method == methodHostImport && in.Reason == "") {
			return fail("an absolute path is required")
		}
		params = map[string]any{"path": in.Path}
		if method == methodHostImport {
			params["reason"] = in.Reason
		}
	case methodResources:
		p, immediate, err := e.resourceRequest(c, in.CPUs, in.MemoryMB, in.Reason)
		if err != nil {
			return fail(err.Error())
		}
		if immediate != nil {
			if client == nil {
				return nil
			}
			return client.Reply(f.ID, toolResult(immediate, nil))
		}
		params = p
	default:
		return fail("unsupported tool")
	}
	return e.Store.update(func(st *State) error {
		chat := st.chat(c.ID)
		if chat.Status != "running" || chat.RunID != c.RunID {
			return errors.New("run expired")
		}
		chat.Approvals = append(chat.Approvals, Approval{ID: cv.ID(), RunID: c.RunID, RPCID: append(json.RawMessage(nil), f.ID...), Method: method, Params: params, State: "pending"})
		return nil
	})
}

// resolveGrant performs an approved grant (or reports the decline) and
// returns the agent's tool result. actor is the person who answered.
func (e *Engine) resolveGrant(c *Chat, a Approval, allow bool, actor cv.Actor) any {
	if !allow {
		return toolResult(nil, errors.New("the owner declined this request"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	who := actor.Name
	if who == "" {
		who = actor.Email
	}
	if who == "" {
		who = actor.PrincipalID
	}
	switch a.Method {
	case methodNetworkAllow:
		minutes := 60
		switch v := a.Params["duration_minutes"].(type) {
		case int:
			minutes = v
		case float64:
			minutes = int(v)
		}
		result, err := e.sharingCall(ctx, "network_allow", map[string]any{"sandboxID": c.SandboxID, "host": a.Params["host"], "duration": minutes * 60, "reason": a.Params["reason"], "actor": who})
		if err == nil {
			result["note"] = "retry the request now; the host stays reachable until expires_at"
		}
		return toolResult(result, err)
	case methodRepositoryAccess:
		repo := agent.String(a.Params["repository"])
		var requested []string
		for _, cat := range agent.Array(a.Params["categories"]) {
			requested = append(requested, agent.String(cat))
		}
		current, err := e.repositoryAccess(ctx, c)
		if err != nil {
			return toolResult(nil, errors.New("could not share "+repo+": "+githubRemedy(err, e.LocalMode)))
		}
		before, shared := current[repo]
		merged := map[string]bool{}
		for _, cat := range append(append([]string{}, before...), requested...) {
			merged[cat] = true
		}
		list := []string{}
		for _, cat := range []string{"contents", "issues", "pull_requests"} {
			if merged[cat] {
				list = append(list, cat)
			}
		}
		// The selection is replaced whole: every repository already shared
		// keeps its categories and the requested one gets the union.
		names := []any{}
		access := map[string]any{}
		for name, cats := range current {
			names = append(names, name)
			access[name] = anyList(cats)
		}
		if !shared {
			names = append(names, repo)
		}
		access[repo] = anyList(list)
		result, err := e.sharingCall(ctx, "github_select", map[string]any{"chatID": c.ID, "sandboxID": c.SandboxID, "repositories": names, "access": access, "actor": who})
		if err != nil {
			what := "share " + repo
			if shared {
				what = "widen " + repo + " from " + strings.Join(before, ", ") + " to " + strings.Join(list, ", ")
			}
			return toolResult(nil, errors.New("the owner approved, but Warden could not "+what+": "+githubRemedy(err, e.LocalMode)))
		}
		out := map[string]any{"repository": repo, "access": anyList(list), "shared": result["repositories"]}
		if shared {
			out["widened_from"] = anyList(before)
		}
		return toolResult(out, nil)
	case methodGitHubWrite:
		data := map[string]any{"sandboxID": c.SandboxID, "actor": who}
		for k, v := range a.Params {
			data[k] = v
		}
		result, err := e.sharingCall(ctx, "github_write", data)
		return toolResult(result, err)
	case methodHostImport, methodHostExport:
		op := "host.import"
		if a.Method == methodHostExport {
			op = "host.export"
		}
		r := request(c, op)
		r.Path = agent.String(a.Params["path"])
		res, err := e.Worker.Call(ctx, r)
		if err != nil {
			return toolResult(nil, err)
		}
		return toolResult(map[string]any{"sandbox_path": res.Directory, "host_path": res.Output, "note": map[string]string{"host.import": "the directory is a copy; use sync_host_directory to write changes back", "host.export": "the host directory now has the sandbox's files; nothing was deleted"}[op]}, nil)
	case methodResources:
		return e.resolveResources(c, a)
	}
	return toolResult(nil, errors.New("unknown grant"))
}

// repositoryAccess is the workspace's current GitHub selection: each shared
// repository (lower case owner/name) with its read categories in canonical
// order.
func (e *Engine) repositoryAccess(ctx context.Context, c *Chat) (map[string][]string, error) {
	current, err := e.sharingCall(ctx, "github_list", map[string]any{"chatID": c.ID, "sandboxID": c.SandboxID})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, item := range agent.Array(current["repositories"]) {
		r := agent.Map(item)
		name := strings.ToLower(agent.String(r["full_name"]))
		cats := []string{}
		for _, cat := range agent.Array(r["access"]) {
			cats = append(cats, agent.String(cat))
		}
		out[name] = cats
	}
	return out, nil
}

// coversCategories reports whether every wanted category is already held.
func coversCategories(have, wanted []string) bool {
	held := map[string]bool{}
	for _, cat := range have {
		held[cat] = true
	}
	for _, cat := range wanted {
		if !held[cat] {
			return false
		}
	}
	return true
}

func anyList(items []string) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

// githubRemedy words a sharing error for the agent together with what the
// owner can do about it, since the agent cannot fix a connection itself.
func githubRemedy(err error, local bool) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "Refresh the GitHub sign-in"):
		if local {
			return msg + " (the GitHub token Warden holds was rejected; the owner runs `warden login github` on the host, then retry)"
		}
		return msg + " (the GitHub token Warden holds was rejected; the owner reconnects GitHub in the Admin console, then retry)"
	case msg == "GitHub is not connected":
		if local {
			return msg + " (the owner runs `warden login github` on the host, then retry)"
		}
		return msg + " (the owner connects GitHub in the Admin console, then retry)"
	case strings.HasPrefix(msg, "repository is not owned by the connected account"):
		return msg + " (the signed-in GitHub account cannot see this repository; check the owner/name or ask the owner to sign in with an account that can)"
	}
	return msg
}
