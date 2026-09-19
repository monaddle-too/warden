package policy

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// proposalLimit caps the submitted contents of one proposal. A proposal
// carries every touched file whole, so a two-line edit to a 90 KiB
// document costs 90 KiB; 256 KiB refused an ordinary ~170-line change over
// a dozen files (docs/dogfood-loop-plan.md). GitHub's contents API takes
// files far larger than this.
const proposalLimit = 1024 * 1024

var branchShape = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// proposalText validates reviewable text: bounded UTF-8 without hidden
// control or bidirectional characters.
func proposalText(value any, limit int, allowEmpty bool) (string, error) {
	s, ok := value.(string)
	if !ok || len(s) > limit || !utf8.ValidString(s) || (!allowEmpty && strings.TrimSpace(s) == "") {
		return "", valueErr("invalid or oversized proposal text")
	}
	for _, r := range s {
		if (r < 32 && r != '\n' && r != '\r' && r != '\t') || strings.ContainsRune("‪‫‬‭‮⁦⁧⁨⁩", r) {
			return "", valueErr("binary or hidden control characters are not supported")
		}
	}
	return s, nil
}

func objectSHA(value any) (string, error) {
	s, ok := value.(string)
	if !ok || !isHex40(s) {
		return "", valueErr("invalid Git object")
	}
	return s, nil
}

// reviewedBody fails closed if a publisher would send a body other than the
// owner's approval.
func reviewedBody(proposal map[string]any, outgoing string) error {
	expected, _ := proposal["approved_body_sha256"].(string)
	if outgoing != proposal["body"] || sha256Hex([]byte(outgoing)) != expected {
		return valueErr("Pull request body differs from the approved review. Submit it for review again.")
	}
	return nil
}

// treeCall performs one repository API call with the token for operation.
type treeCall func(method, suffix, operation string, body map[string]any) (map[string]any, error)

// relevantEntries reads only affected directories, pinned to the base tree's
// immutable SHAs.
func relevantEntries(call treeCall, root string, paths map[string]bool) (map[string]map[string]any, error) {
	entries := map[string]map[string]any{}
	loaded := map[string]bool{}
	directory := func(prefix, tree string) error {
		if loaded[prefix] {
			return nil
		}
		sha, err := objectSHA(tree)
		if err != nil {
			return err
		}
		listing, err := call("GET", "/git/trees/"+sha, "git/get-tree", nil)
		if err != nil {
			return err
		}
		if truncated, _ := listing["truncated"].(bool); truncated {
			return valueErr("Repository directory is too large to review safely")
		}
		items, _ := listing["tree"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			name, ok := item["path"].(string)
			if !ok || name == "" || strings.Contains(name, "/") || name == "." || name == ".." {
				return valueErr("Invalid repository tree entry")
			}
			path := prefix + name
			if _, dup := entries[path]; dup {
				return valueErr("Duplicate repository tree entry")
			}
			entries[path] = item
		}
		loaded[prefix] = true
		return nil
	}
	if err := directory("", root); err != nil {
		return nil, err
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, path := range sorted {
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			parent := strings.Join(parts[:i], "/")
			entry := entries[parent]
			if entry == nil || entry["type"] != "tree" {
				break
			}
			sha, _ := entry["sha"].(string)
			if err := directory(parent+"/", sha); err != nil {
				return nil, err
			}
		}
	}
	return entries, nil
}

// PullRequests manages immutable, owner-reviewed pull request proposals.
type PullRequests struct {
	s         *Sharing
	Transport GitHubTransport
	// Logs fetches a GitHub Actions job log for Checks; nil skips logs.
	Logs LogTransport
}

type pullRow struct {
	id, chat, sandbox, status, proposal, outcome string
	delivered                                    int64
}

func newPullRequests(s *Sharing) (*PullRequests, error) {
	if _, err := s.DB.Exec("CREATE TABLE IF NOT EXISTS pull_requests (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, status TEXT, proposal TEXT, outcome TEXT, delivered INTEGER DEFAULT 0)"); err != nil {
		return nil, err
	}
	if err := ensureTextColumns(s.DB, "pull_requests", [][2]string{{"principal", OwnerPrincipal}}); err != nil {
		return nil, err
	}
	interrupted := mustJSON(map[string]any{"error": "Publication interrupted. Check the proposal branch on GitHub before submitting again; Warden will not retry automatically."})
	if _, err := s.DB.Exec("UPDATE pull_requests SET status='failed', outcome=? WHERE status='publishing'", string(interrupted)); err != nil {
		return nil, err
	}
	return &PullRequests{s: s, Transport: GitHubRequest, Logs: githubLogRequest}, nil
}

func (p *PullRequests) rowLocked(id string) (*pullRow, error) {
	var r pullRow
	err := p.s.DB.QueryRow("SELECT id,chat,sandbox,status,proposal,outcome,delivered FROM pull_requests WHERE id=?", id).Scan(&r.id, &r.chat, &r.sandbox, &r.status, &r.proposal, &r.outcome, &r.delivered)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Row returns a proposal row for tests and operators.
func (p *PullRequests) Row(id string) (status string, proposal map[string]any, ok bool) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	r, err := p.rowLocked(id)
	if err != nil {
		return "", nil, false
	}
	_ = json.Unmarshal([]byte(r.proposal), &proposal)
	return r.status, proposal, true
}

// SetProposal replaces a stored proposal (tests simulate tampering).
func (p *PullRequests) SetProposal(id string, proposal map[string]any) error {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	_, err := p.s.DB.Exec("UPDATE pull_requests SET proposal=? WHERE id=?", string(mustJSON(proposal)), id)
	return err
}

func (p *PullRequests) result(r *pullRow, preview bool) map[string]any {
	var proposal map[string]any
	_ = json.Unmarshal([]byte(r.proposal), &proposal)
	out := map[string]any{"request_id": r.id, "kind": "pull_request", "chatID": r.chat, "sandboxID": r.sandbox,
		"status": r.status, "repository": proposal["repository"], "title": proposal["title"], "head": proposal["head"]}
	if update, ok := proposal["pull_request"].(map[string]any); ok {
		out["pull_request"], out["update"] = update["number"], true
	}
	var outcome map[string]any
	_ = json.Unmarshal([]byte(r.outcome), &outcome)
	for k, v := range outcome {
		out[k] = v
	}
	if preview {
		out["proposal"] = proposal
	}
	return out
}

func (p *PullRequests) undeliveredLocked() ([]any, error) {
	rows, err := p.s.DB.Query("SELECT id,chat,sandbox,status,proposal,outcome,delivered FROM pull_requests WHERE status NOT IN ('pending','publishing') AND delivered=0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []any
	for rows.Next() {
		var r pullRow
		if err = rows.Scan(&r.id, &r.chat, &r.sandbox, &r.status, &r.proposal, &r.outcome, &r.delivered); err != nil {
			return nil, err
		}
		out = append(out, p.result(&r, false))
	}
	return out, rows.Err()
}

// active confirms the repository is selected for the conversation and
// returns its pinned identity.
func (p *PullRequests) active(data map[string]any, repo string, repositoryID *int64) (int64, error) {
	listing, err := p.s.githubDispatch("github_list", data)
	if err != nil {
		return 0, err
	}
	repos, _ := listing["repositories"].([]any)
	for _, item := range repos {
		r, _ := item.(map[string]any)
		if lowerString(r["full_name"]) != strings.ToLower(repo) {
			continue
		}
		id, _ := asInt(r["id"])
		if repositoryID != nil && id != *repositoryID {
			break
		}
		return id, nil
	}
	return 0, valueErr("Select this repository for the conversation before proposing a pull request")
}

func (p *PullRequests) api(repo string, repositoryID int64) treeCall {
	tokens := map[string]string{}
	return func(method, suffix, operation string, body map[string]any) (map[string]any, error) {
		perms, err := GitHubPermissions(operation)
		if err != nil {
			return nil, err
		}
		key := Dumps(perms)
		token, ok := tokens[key]
		if !ok {
			authorization, err := p.s.GitHub.Authorization(repo, operation, &repositoryID)
			if err != nil {
				return nil, err
			}
			token = strings.TrimPrefix(authorization, "Bearer ")
			tokens[key] = token
		}
		return p.Transport(method, "/repos/"+repo+suffix, token, body)
	}
}

func quotePath(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-' || c == '~' || c == '/' {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)|0x100, 16)[1:]))
		}
	}
	return b.String()
}

func githubBranchRef(call treeCall, branch string) (string, error) {
	ref, err := call("GET", "/git/ref/heads/"+quotePath(branch), "git/get-ref", nil)
	if err != nil {
		return "", err
	}
	object, _ := ref["object"].(map[string]any)
	return objectSHA(object["sha"])
}

// Submit validates a proposal, computes the review from GitHub and stores it.
func (p *PullRequests) Submit(data map[string]any) (map[string]any, error) {
	if p.s.GitHub == nil {
		return nil, valueErr("GitHub is not connected")
	}
	owner, appID, err := p.s.GitHub.Identity()
	if err != nil {
		return nil, valueErr(err.Error())
	}
	// base is the branch a new pull request targets; with pull_request
	// (an update to a pull request Warden published) the proposal is
	// against that pull request's head branch and base may be omitted.
	required := stringSet("chatID", "sandboxID", "callID", "repository", "title", "body", "files")
	optional := stringSet("images", "base", "pull_request")
	for key := range data {
		if !required[key] && !optional[key] {
			return nil, valueErr("Provide repository, base branch, title, body, and changed files")
		}
	}
	for key := range required {
		if _, ok := data[key]; !ok {
			return nil, valueErr("Provide repository, base branch, title, body, and changed files")
		}
	}
	if _, ok := data["base"]; !ok {
		if _, ok := data["pull_request"]; !ok {
			return nil, valueErr("Provide repository, base branch, title, body, and changed files")
		}
	}
	repoText, err := proposalText(data["repository"], 201, false)
	if err != nil {
		return nil, err
	}
	repo := strings.ToLower(repoText)
	rid, err := p.active(data, repo, nil)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"chatID", "sandboxID", "callID"} {
		if _, err = proposalText(data[key], 512, false); err != nil {
			return nil, err
		}
	}
	chat, sandbox, callID := stringField(data, "chatID"), stringField(data, "sandboxID"), stringField(data, "callID")
	id := sha256Hex([]byte("pr\x00" + chat + "\x00" + sandbox + "\x00" + callID))
	p.s.mu.Lock()
	if old, err := p.rowLocked(id); err == nil {
		result := p.result(old, false)
		p.s.mu.Unlock()
		return result, nil
	}
	p.s.mu.Unlock()
	title, err := proposalText(data["title"], 256, false)
	if err != nil {
		return nil, err
	}
	body, err := proposalText(data["body"], 32768, true)
	if err != nil {
		return nil, err
	}
	var branch string
	var update map[string]any
	if v, present := data["pull_request"]; present {
		// An update: the change is reviewed against the pull request's
		// current head and, approved, committed onto that branch. Only a
		// branch Warden itself published (warden/pr-…) of an open pull
		// request in this repository qualifies, so the write stays within
		// what Warden created.
		number, ok := asInt(v)
		if !ok || number <= 0 {
			return nil, valueErr("pull_request must be the number of an open pull request Warden published")
		}
		pr, err := p.api(repo, rid)("GET", "/pulls/"+strconv.FormatInt(number, 10), "pulls/get", nil)
		if err != nil {
			return nil, valueErr("Pull request #" + strconv.FormatInt(number, 10) + " could not be read in " + repo)
		}
		headRef, prBase, err := wardenPullHead(pr, repo)
		if err != nil {
			return nil, err
		}
		if given, _ := data["base"].(string); given != "" && given != prBase {
			return nil, valueErr("base must be the pull request's base branch " + prBase + ", or omitted")
		}
		branch = headRef
		update = map[string]any{"number": number, "base": prBase, "url": "https://github.com/" + repo + "/pull/" + strconv.FormatInt(number, 10)}
	} else {
		branch, err = proposalText(data["base"], 200, false)
		if err != nil {
			return nil, err
		}
	}
	if !branchShape.MatchString(branch) || strings.Contains(branch, "..") {
		return nil, valueErr("invalid base branch")
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".lock") {
			return nil, valueErr("invalid base branch")
		}
	}
	files, ok := data["files"].([]any)
	images, _ := data["images"].([]any)
	if !ok || len(files) > 20 || (len(files) == 0 && len(images) == 0) {
		return nil, valueErr("Submit up to 20 text files and/or image attachments")
	}
	paths := map[string]bool{}
	size := 0
	type fileInput struct {
		path    string
		content *string
	}
	var inputs []fileInput
	for _, raw := range files {
		f, ok := raw.(map[string]any)
		if !ok || !sameKeys(f, "path", "content") {
			return nil, valueErr("Each file requires path and complete content (null to delete)")
		}
		path, err := proposalText(f["path"], 512, false)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(path, "\\\n\r\t") {
			return nil, valueErr("invalid repository path")
		}
		for _, part := range strings.Split(path, "/") {
			if part == "" || part == "." || part == ".." || strings.ToLower(part) == ".git" {
				return nil, valueErr("invalid repository path")
			}
		}
		if strings.HasPrefix(path, ".github/workflows/") {
			return nil, valueErr("Workflow changes require separate GitHub App permissions and are not supported here")
		}
		if paths[path] {
			return nil, valueErr("duplicate path")
		}
		paths[path] = true
		input := fileInput{path: path}
		if f["content"] != nil {
			content, err := proposalText(f["content"], proposalLimit, true)
			if err != nil {
				return nil, err
			}
			size += len(content)
			input.content = &content
		}
		inputs = append(inputs, input)
	}
	if size > proposalLimit {
		return nil, valueErr("Proposal exceeds 1 MiB of file contents; split it into smaller pull requests")
	}
	for path := range paths {
		for other := range paths {
			if other != path && strings.HasPrefix(path, other+"/") {
				return nil, valueErr("overlapping file paths")
			}
		}
	}
	call := p.api(repo, rid)
	base, err := githubBranchRef(call, branch)
	if err != nil {
		return nil, err
	}
	commit, err := call("GET", "/git/commits/"+base, "git/get-commit", nil)
	if err != nil {
		return nil, err
	}
	treeInfo, _ := commit["tree"].(map[string]any)
	tree, err := objectSHA(treeInfo["sha"])
	if err != nil {
		return nil, err
	}
	lookup := map[string]bool{}
	for path := range paths {
		lookup[path] = true
	}
	if len(images) > 0 {
		lookup[".warden/images/attachment.png"] = true
	}
	entries, err := relevantEntries(call, tree, lookup)
	if err != nil {
		return nil, err
	}
	changes := []any{}
	total := size
	for _, input := range inputs {
		path := input.path
		entry := entries[path]
		var before *string
		mode := "100644"
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			ancestor := strings.Join(parts[:i], "/")
			if e, ok := entries[ancestor]; ok && e["type"] != "tree" {
				return nil, valueErr("path crosses a non-directory")
			}
		}
		if entry != nil {
			entryMode, _ := entry["mode"].(string)
			if entry["type"] != "blob" || (entryMode != "100644" && entryMode != "100755") {
				return nil, valueErr("Only regular text files can be reviewed")
			}
			if entrySize, _ := asInt(entry["size"]); entrySize > proposalLimit {
				return nil, valueErr("Base file exceeds review limit")
			}
			sha, err := objectSHA(entry["sha"])
			if err != nil {
				return nil, err
			}
			blob, err := call("GET", "/git/blobs/"+sha, "git/get-blob", nil)
			if err != nil {
				return nil, err
			}
			if blob["encoding"] != "base64" {
				return nil, valueErr("unsupported file encoding")
			}
			content, _ := blob["content"].(string)
			decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(content), ""))
			if err != nil || !utf8.Valid(decoded) {
				return nil, valueErr("invalid or oversized proposal text")
			}
			text, err := proposalText(string(decoded), proposalLimit, true)
			if err != nil {
				return nil, err
			}
			before = &text
			mode = entryMode
			total += len(text)
		}
		after := input.content
		if (before == nil && after == nil) || (before != nil && after != nil && *before == *after) {
			return nil, valueErr("Every submitted file must contain a change")
		}
		if total > proposalLimit*2 {
			return nil, valueErr("Combined before/after text exceeds review limit")
		}
		fromFile, toFile := "/dev/null", "/dev/null"
		var beforeText, afterText string
		if before != nil {
			fromFile = "a/" + path
			beforeText = *before
		}
		if after != nil {
			toFile = "b/" + path
			afterText = *after
		}
		diff := UnifiedDiff(splitKeepEnds(beforeText), splitKeepEnds(afterText), fromFile, toFile)
		// Preserve missing-newline information instead of visually joining +/- lines.
		lines := []any{}
		additions, deletions := 0, 0
		for i, line := range diff {
			lines = append(lines, strings.TrimSuffix(line, "\n"))
			if !strings.HasSuffix(line, "\n") {
				lines = append(lines, "\\ No newline at end of file")
			}
			if i >= 2 {
				if strings.HasPrefix(line, "+") {
					additions++
				}
				if strings.HasPrefix(line, "-") {
					deletions++
				}
			}
		}
		change := "modified"
		if before == nil {
			change = "added"
		} else if after == nil {
			change = "deleted"
		}
		var content any
		if after != nil {
			content = *after
		}
		changes = append(changes, map[string]any{"path": path, "content": content, "mode": mode, "change": change, "diff": lines, "additions": additions, "deletions": deletions})
	}
	if raw, present := data["images"]; present {
		if _, ok := raw.([]any); !ok {
			return nil, valueErr("Select up to four distinct images")
		}
	}
	seen := map[string]bool{}
	for _, item := range images {
		s, _ := item.(string)
		seen[s] = true
	}
	if len(images) > 4 || len(seen) != len(images) {
		return nil, valueErr("Select up to four distinct images")
	}
	attachments := []any{}
	for _, item := range images {
		imageID, _ := item.(string)
		image, err := p.s.Images.Get(imageID, chat, sandbox)
		if err != nil {
			return nil, err
		}
		path := ".warden/images/" + image.Digest + ".png"
		_, exists := entries[path]
		if paths[path] || exists {
			return nil, valueErr("Image path conflicts with proposed or existing files")
		}
		for other := range paths {
			if strings.HasPrefix(path, other+"/") {
				return nil, valueErr("Image path conflicts with proposed or existing files")
			}
		}
		for _, ancestor := range []string{".warden", ".warden/images"} {
			if e, ok := entries[ancestor]; ok && e["type"] != "tree" {
				return nil, valueErr("Image directory is not a regular directory")
			}
		}
		link := "../blob/warden/pr-" + id[:24] + "/" + path + "?raw=true"
		attachments = append(attachments, map[string]any{"image_id": imageID, "sha256": image.Digest, "path": path, "url": link, "caption": image.Caption})
		body += "\n\n![Screenshot " + strconv.Itoa(len(attachments)) + "](" + link + ")\n"
	}
	if _, err = proposalText(body, 32768, true); err != nil {
		return nil, err
	}
	proposal := map[string]any{"images": attachments, "repository": repo, "repository_id": rid, "owner": owner, "app_id": appID, "title": title, "body": body, "base": branch, "base_sha": base, "base_tree": tree, "head": "warden/pr-" + id[:24], "files": changes}
	if update != nil {
		// The reviewed commit lands on the pull request's own branch.
		proposal["head"], proposal["pull_request"] = branch, update
	}
	encoded := mustJSON(proposal)
	if len(encoded) > 4*proposalLimit {
		return nil, valueErr("Rendered proposal exceeds review limit")
	}
	if _, err = p.active(data, repo, &rid); err != nil {
		return nil, err
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if _, err = p.s.DB.Exec("INSERT OR IGNORE INTO pull_requests (id,chat,sandbox,status,proposal,outcome,delivered,principal) VALUES (?,?,?,?,?,?,0,?)", id, chat, sandbox, "pending", string(encoded), "{}", principalOf(data)); err != nil {
		return nil, err
	}
	r, err := p.rowLocked(id)
	if err != nil {
		return nil, err
	}
	return p.result(r, false), nil
}

// Dispatch handles pr_state, pr_preview, pr_get, pr_resolve and pr_checks.
func (p *PullRequests) Dispatch(op string, data map[string]any) (map[string]any, error) {
	if op == "pr_checks" {
		return p.Checks(data)
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if op == "pr_state" {
		rows, err := p.s.DB.Query("SELECT id,chat,sandbox,status,proposal,outcome,delivered FROM pull_requests ORDER BY rowid")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		requests := []any{}
		for rows.Next() {
			var r pullRow
			if err = rows.Scan(&r.id, &r.chat, &r.sandbox, &r.status, &r.proposal, &r.outcome, &r.delivered); err != nil {
				return nil, err
			}
			requests = append(requests, p.result(&r, false))
		}
		return map[string]any{"requests": requests}, rows.Err()
	}
	r, err := p.rowLocked(stringField(data, "id"))
	if err != nil {
		return nil, errors.New("unknown pull request proposal")
	}
	switch op {
	case "pr_preview":
		return p.result(r, true), nil
	case "pr_get":
		if r.chat != stringField(data, "chatID") || r.sandbox != stringField(data, "sandboxID") {
			return nil, errors.New("proposal belongs to another conversation")
		}
		return p.result(r, false), nil
	}
	allow, isBool := data["allow"].(bool)
	if op != "pr_resolve" || !isBool {
		return nil, errors.New("invalid review decision")
	}
	var proposal map[string]any
	_ = json.Unmarshal([]byte(r.proposal), &proposal)
	if r.status != "pending" {
		if body, present := data["body"]; allow && (r.status == "publishing" || r.status == "published") && present {
			text, _ := body.(string)
			if err = reviewedBody(proposal, text); err != nil {
				return nil, err
			}
		}
		return p.result(r, false), nil
	}
	feedback := ""
	if v, present := data["feedback"]; present {
		if feedback, err = proposalText(v, 4000, true); err != nil {
			return nil, err
		}
	}
	bodyValue, hasBody := data["body"]
	if allow && !hasBody {
		return nil, errors.New("Approval must include the exact reviewed PR body")
	}
	if !hasBody {
		bodyValue = proposal["body"]
	}
	body, err := proposalText(bodyValue, 32768, true)
	if err != nil {
		return nil, err
	}
	submitted, _ := proposal["body"].(string)
	proposal["submitted_body"] = submitted
	proposal["body"] = body
	outcome := map[string]any{"feedback": feedback}
	if allow {
		proposal["approved_body_sha256"] = sha256Hex([]byte(body))
	} else if body != submitted {
		outcome["edited_body"] = body
	}
	status := "rejected"
	if allow {
		status = "publishing"
	}
	if _, err = p.s.DB.Exec("UPDATE pull_requests SET status=?,proposal=?,outcome=? WHERE id=?", status, string(mustJSON(proposal)), string(mustJSON(outcome)), r.id); err != nil {
		return nil, err
	}
	r, err = p.rowLocked(r.id)
	if err != nil {
		return nil, err
	}
	result := p.result(r, false)
	if allow {
		go p.Publish(r.id)
	}
	return result, nil
}

// Publish creates the branch and pull request from the approved snapshot.
// No retry after an ambiguous response: creating a PR is not idempotent.
func (p *PullRequests) Publish(id string) {
	p.s.mu.Lock()
	r, err := p.rowLocked(id)
	if err != nil || r.status != "publishing" {
		p.s.mu.Unlock()
		return
	}
	var proposal map[string]any
	_ = json.Unmarshal([]byte(r.proposal), &proposal)
	data := map[string]any{"chatID": r.chat, "sandboxID": r.sandbox}
	p.s.mu.Unlock()
	repo, _ := proposal["repository"].(string)
	head, _ := proposal["head"].(string)
	out := map[string]any{}
	phase := "validating"
	status := "failed"
	err = func() error {
		body, _ := proposal["body"].(string)
		if err := reviewedBody(proposal, body); err != nil {
			return err
		}
		appID, _ := asInt(proposal["app_id"])
		if p.s.GitHub == nil {
			return valueErr("Connected GitHub account changed; submit a new proposal")
		}
		owner, currentApp, err := p.s.GitHub.Identity()
		if err != nil || owner != proposal["owner"] || currentApp != appID {
			return valueErr("Connected GitHub account changed; submit a new proposal")
		}
		rid, _ := asInt(proposal["repository_id"])
		if _, err := p.active(data, repo, &rid); err != nil {
			return err
		}
		call := p.api(repo, rid)
		base, _ := proposal["base"].(string)
		baseSHA, _ := proposal["base_sha"].(string)
		current, err := githubBranchRef(call, base)
		if err != nil {
			return err
		}
		if current != baseSHA {
			return valueErr("The base branch changed since review. Submit a fresh proposal for a new review.")
		}
		imageEntries := []any{}
		images, _ := proposal["images"].([]any)
		for _, item := range images {
			image, _ := item.(map[string]any)
			stored, err := p.s.Images.Get(stringField(image, "image_id"), r.chat, r.sandbox)
			if err != nil {
				return err
			}
			if stored.Digest != image["sha256"] {
				return valueErr("Reviewed image changed")
			}
			blob, err := call("POST", "/git/blobs", "git/create-blob", map[string]any{"content": base64.StdEncoding.EncodeToString(stored.PNG), "encoding": "base64"})
			if err != nil {
				return err
			}
			sha, err := objectSHA(blob["sha"])
			if err != nil {
				return err
			}
			imageEntries = append(imageEntries, map[string]any{"path": image["path"], "mode": "100644", "type": "blob", "sha": sha})
		}
		phase = "creating tree"
		treeEntries := []any{}
		files, _ := proposal["files"].([]any)
		for _, item := range files {
			f, _ := item.(map[string]any)
			entry := map[string]any{"path": f["path"], "mode": f["mode"], "type": "blob"}
			if f["content"] == nil {
				entry["sha"] = nil
			} else {
				entry["content"] = f["content"]
			}
			treeEntries = append(treeEntries, entry)
		}
		treeEntries = append(treeEntries, imageEntries...)
		tree, err := call("POST", "/git/trees", "git/create-tree", map[string]any{"base_tree": proposal["base_tree"], "tree": treeEntries})
		if err != nil {
			return err
		}
		treeSHA, err := objectSHA(tree["sha"])
		if err != nil {
			return err
		}
		phase = "creating commit"
		message, _ := proposal["title"].(string)
		if _, ok := proposal["pull_request"]; ok && strings.TrimSpace(body) != "" {
			// An update's reviewed body is the commit message body: the
			// pull request's own description is not rewritten.
			message += "\n\n" + body
		}
		commit, err := call("POST", "/git/commits", "git/create-commit", map[string]any{"message": message, "tree": treeSHA, "parents": []any{baseSHA}})
		if err != nil {
			return err
		}
		commitSHA, err := objectSHA(commit["sha"])
		if err != nil {
			return err
		}
		if _, err = p.active(data, repo, &rid); err != nil {
			return err
		}
		// Check again before making a branch externally visible.
		current, err = githubBranchRef(call, base)
		if err != nil {
			return err
		}
		if current != baseSHA {
			return valueErr("The base branch changed; submit a new proposal")
		}
		if update, ok := proposal["pull_request"].(map[string]any); ok {
			// An update: fast-forward the pull request's branch to the
			// reviewed commit (its parent is the reviewed head, so a moved
			// branch is refused twice: by the check above and by GitHub).
			phase = "updating branch"
			if _, err = call("PATCH", "/git/refs/heads/"+quotePath(head), "git/update-ref", map[string]any{"sha": commitSHA, "force": false}); err != nil {
				return err
			}
			number, _ := asInt(update["number"])
			out["branch_url"] = "https://github.com/" + repo + "/tree/" + head
			out["url"] = "https://github.com/" + repo + "/pull/" + strconv.FormatInt(number, 10)
			out["number"] = number
			out["commit"] = commitSHA
			status = "published"
			return nil
		}
		phase = "creating branch"
		if _, err = call("POST", "/git/refs", "git/create-ref", map[string]any{"ref": "refs/heads/" + head, "sha": commitSHA}); err != nil {
			return err
		}
		out["branch_url"] = "https://github.com/" + repo + "/tree/" + head
		phase = "creating pull request"
		if _, err = p.active(data, repo, &rid); err != nil {
			return err
		}
		payload := map[string]any{"title": proposal["title"], "body": proposal["body"], "base": base, "head": head, "maintainer_can_modify": false}
		if err = reviewedBody(proposal, body); err != nil {
			return err
		}
		pr, err := call("POST", "/pulls", "pulls/create", payload)
		if err != nil {
			return err
		}
		number, ok := asInt(pr["number"])
		if !ok || number <= 0 {
			return valueErr("GitHub returned an unexpected pull request response")
		}
		out["url"] = "https://github.com/" + repo + "/pull/" + strconv.FormatInt(number, 10)
		out["number"] = number
		status = "published"
		return nil
	}()
	if err != nil {
		// Do not return upstream response bodies or credential diagnostics to the agent.
		message := "GitHub publication failed. Check installation permissions and GitHub before submitting again."
		if isValueError(err) {
			message = err.Error()
		}
		out["error"] = message
		out["phase"] = phase
		out["branch_url"] = "https://github.com/" + repo + "/tree/" + head
		status = "failed"
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if p.s.DB != nil {
		_, _ = p.s.DB.Exec("UPDATE pull_requests SET status=?,outcome=? WHERE id=?", status, string(mustJSON(out)), id)
	}
}

var _ = sql.ErrNoRows

// wardenPullHead reads the branch an update proposal commits onto: the
// pull request must be open, its head in this repository, and the branch
// one Warden published (warden/pr-…). Returns the head ref and the base
// branch name.
func wardenPullHead(pr map[string]any, repo string) (head, base string, err error) {
	if pr["state"] != "open" {
		return "", "", valueErr("The pull request is not open")
	}
	headInfo, _ := pr["head"].(map[string]any)
	headRepo, _ := headInfo["repo"].(map[string]any)
	if lowerString(headRepo["full_name"]) != strings.ToLower(repo) {
		return "", "", valueErr("The pull request's branch is in another repository")
	}
	head, _ = headInfo["ref"].(string)
	if !strings.HasPrefix(head, "warden/pr-") || !branchShape.MatchString(head) {
		return "", "", valueErr("Only a pull request Warden published (branch warden/pr-…) can be updated")
	}
	baseInfo, _ := pr["base"].(map[string]any)
	base, _ = baseInfo["ref"].(string)
	return head, base, nil
}

// LogTransport fetches one GitHub Actions job log (the API answers with
// a redirect to a short-lived download URL) and returns its text.
type LogTransport func(path, token string) (string, error)

// githubLogRequest is the real log transport: the redirect is followed
// once, the credential is not sent to the download host (net/http drops
// Authorization across hosts), at most 4 MiB is read.
func githubLogRequest(path, token string) (string, error) {
	req, err := http.NewRequest("GET", "https://api.github.com"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Warden-GitHub-App")
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("GitHub is unavailable; retry the request")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("the job log is not available (HTTP " + strconv.Itoa(resp.StatusCode) + ")")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// logTail keeps the last limit bytes of a log on a line boundary, and the
// ##[error] lines wherever they are, so the agent reads why a job failed
// without the whole transcript.
func logTail(text string, limit int) (tail string, errorLines []string) {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "##[error]") && len(errorLines) < 20 {
			errorLines = append(errorLines, strings.TrimSpace(line))
		}
	}
	if len(text) > limit {
		text = text[len(text)-limit:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return text, errorLines
}

var shaShape = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Checks answers view_ci_results: the check runs GitHub recorded for a
// pull request's head (or a branch / commit), and for each failed GitHub
// Actions job its steps and the tail of its log. A read with the owner's
// credential, never a token for the agent; the repository must be
// selected for the conversation.
func (p *PullRequests) Checks(data map[string]any) (map[string]any, error) {
	if p.s.GitHub == nil {
		return nil, valueErr("GitHub is not connected")
	}
	repoText, err := proposalText(data["repository"], 201, false)
	if err != nil {
		return nil, err
	}
	repo := strings.ToLower(repoText)
	rid, err := p.active(data, repo, nil)
	if err != nil {
		return nil, err
	}
	call := p.api(repo, rid)
	out := map[string]any{"repository": repo}
	ref := ""
	if v, present := data["pull_request"]; present && v != nil {
		number, ok := asInt(v)
		if !ok || number <= 0 {
			return nil, valueErr("pull_request must be a pull request number")
		}
		pr, err := call("GET", "/pulls/"+strconv.FormatInt(number, 10), "pulls/get", nil)
		if err != nil {
			return nil, valueErr("Pull request #" + strconv.FormatInt(number, 10) + " could not be read in " + repo)
		}
		headInfo, _ := pr["head"].(map[string]any)
		ref, _ = headInfo["sha"].(string)
		out["pull_request"], out["url"], out["state"] = number, "https://github.com/"+repo+"/pull/"+strconv.FormatInt(number, 10), pr["state"]
		if !shaShape.MatchString(ref) {
			return nil, valueErr("The pull request has no head commit")
		}
	} else {
		ref, _ = data["ref"].(string)
		ref = strings.TrimSpace(ref)
		if ref == "" || !(shaShape.MatchString(ref) || (branchShape.MatchString(ref) && !strings.Contains(ref, ".."))) {
			return nil, valueErr("Give pull_request (a number) or ref (a branch or commit)")
		}
	}
	out["ref"] = ref
	listing, err := call("GET", "/commits/"+quotePath(ref)+"/check-runs?per_page=100", "checks/list-for-ref", nil)
	if err != nil {
		return nil, valueErr("The checks of " + ref + " could not be read")
	}
	runs, _ := listing["check_runs"].([]any)
	checks := []any{}
	failed := []any{}
	overall := "success"
	if len(runs) == 0 {
		overall = "none"
	}
	for _, item := range runs {
		run, _ := item.(map[string]any)
		output, _ := run["output"].(map[string]any)
		app, _ := run["app"].(map[string]any)
		status, _ := run["status"].(string)
		conclusion, _ := run["conclusion"].(string)
		entry := map[string]any{"name": run["name"], "status": status, "conclusion": conclusion, "url": run["html_url"], "started_at": run["started_at"], "completed_at": run["completed_at"], "title": output["title"], "summary": output["summary"]}
		checks = append(checks, entry)
		switch {
		case status != "completed":
			if overall != "failure" {
				overall = "pending"
			}
		case conclusion == "failure" || conclusion == "timed_out" || conclusion == "cancelled" || conclusion == "action_required" || conclusion == "startup_failure":
			overall = "failure"
		}
		failedRun := status == "completed" && (conclusion == "failure" || conclusion == "timed_out" || conclusion == "cancelled" || conclusion == "startup_failure")
		if !failedRun || app["slug"] != "github-actions" {
			continue
		}
		id, ok := asInt(run["id"])
		if !ok {
			continue
		}
		detail := map[string]any{"name": run["name"], "job_id": id, "url": run["html_url"], "conclusion": conclusion}
		if job, err := call("GET", "/actions/jobs/"+strconv.FormatInt(id, 10), "actions/get-job-for-workflow-run", nil); err == nil {
			steps := []any{}
			stepList, _ := asList(job["steps"])
			for _, s := range stepList {
				step, _ := s.(map[string]any)
				steps = append(steps, map[string]any{"name": step["name"], "status": step["status"], "conclusion": step["conclusion"]})
			}
			detail["steps"], detail["workflow_run"] = steps, job["run_id"]
		}
		if p.Logs != nil {
			if token, err := p.token(repo, rid, "actions/download-job-logs-for-workflow-run"); err == nil {
				if text, err := p.Logs("/repos/"+repo+"/actions/jobs/"+strconv.FormatInt(id, 10)+"/logs", token); err == nil {
					detail["log_tail"], detail["errors"] = logTail(text, 16384)
				} else {
					detail["log_error"] = err.Error()
				}
			}
		}
		failed = append(failed, detail)
	}
	out["conclusion"], out["checks"], out["failed_jobs"] = overall, checks, failed
	switch overall {
	case "none":
		out["hint"] = "No checks have been recorded for this commit yet; a workflow may still be queued. Check again in a minute."
	case "pending":
		out["hint"] = "Checks are still running; check again in a minute."
	case "failure":
		out["hint"] = "A check failed: read failed_jobs (steps and the log tail), fix the cause, and submit the fix with request_pull_request and pull_request set to update the same pull request."
	}
	return out, nil
}

// token is the credential one operation runs with, as api() obtains it.
func (p *PullRequests) token(repo string, rid int64, operation string) (string, error) {
	authorization, err := p.s.GitHub.Authorization(repo, operation, &rid)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(authorization, "Bearer "), nil
}
