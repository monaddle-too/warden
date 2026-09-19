package chats

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"warden/chat/internal/agent"
)

// Resource mentions (docs/claude-parity.md, R2.14): besides workspace
// paths, the composer's "@" menu offers the chat's shared resources —
// the documents, repositories and previews of its workspace — and inserts
// a token for the one picked: `@doc:Title`, `@repo:owner/name`,
// `@preview:Name`, the name quoted when it has spaces (`@doc:"Budget
// 2026"`). The transcript keeps the token as typed; when the message is
// handed to the agent the token expands to what the agent can act on: a
// document's id and URL (read_google_document takes the id), a
// repository's clone URL and read access (what list_shared_repositories
// reports), a preview's URL. A token that names nothing shared stays as
// written. GET chats/{id}/resources lists what a chat can mention, from
// the same policy operations the sharing panels use and the chat's own
// port bindings.

// Resources is what a chat's workspace shares, as the "@" menu offers it.
type Resources struct {
	Documents    []ResourceDocument   `json:"documents"`
	Repositories []ResourceRepository `json:"repositories"`
	Previews     []ResourcePreview    `json:"previews"`
}

// ResourceDocument is a shared Google document or spreadsheet: its id
// (what the document tools take), title, kind ("document" or
// "spreadsheet"), URL, the grant's access level and expiry (unix
// seconds, 0 for none).
type ResourceDocument struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Kind    string  `json:"kind"`
	URL     string  `json:"url"`
	Access  string  `json:"access"`
	Expires float64 `json:"expires,omitempty"`
}

// ResourceRepository is a shared GitHub repository: owner/name, its page
// and clone URLs, and the read categories granted.
type ResourceRepository struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	CloneURL string   `json:"cloneURL"`
	Access   []string `json:"access"`
}

// ResourcePreview is a published preview: the binding's id and title,
// the guest port behind it and its URL.
type ResourcePreview struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Port  int    `json:"port"`
	URL   string `json:"url"`
}

// Resources lists what chat id can mention: the workspace's document
// grants and repositories from the policy service (each list empty when
// sharing is not configured or the call fails, so the menu still offers
// the rest), and its approved port bindings from the state.
func (e *Engine) Resources(ctx context.Context, id string) (Resources, error) {
	st := e.Store.Snapshot()
	c := st.chat(id)
	if c == nil {
		return Resources{}, errors.New("chat not found")
	}
	out := Resources{Documents: []ResourceDocument{}, Repositories: []ResourceRepository{}, Previews: []ResourcePreview{}}
	for _, p := range st.Ports {
		if p.SandboxID == c.SandboxID && p.State == "approved" && p.URL != "" {
			out.Previews = append(out.Previews, ResourcePreview{ID: p.ID, Title: p.Title, Port: p.Port, URL: p.URL})
		}
	}
	if e.PolicyAddress == "" {
		return out, nil
	}
	if result, err := e.sharingCall(ctx, "list", map[string]any{"chatID": c.ID, "sandboxID": c.SandboxID}); err == nil {
		for _, value := range agent.Array(result["grants"]) {
			grant := agent.Map(value)
			expires, _ := grant["expires_at"].(float64)
			for _, d := range agent.Array(grant["documents"]) {
				doc := agent.Map(d)
				if agent.String(doc["id"]) == "" {
					continue
				}
				out.Documents = append(out.Documents, ResourceDocument{ID: agent.String(doc["id"]), Title: agent.String(doc["title"]), Kind: agent.String(doc["kind"]), URL: agent.String(doc["url"]), Access: agent.String(grant["access"]), Expires: expires})
			}
		}
	}
	if result, err := e.sharingCall(ctx, "github_list", map[string]any{"chatID": c.ID, "sandboxID": c.SandboxID}); err == nil {
		for _, value := range agent.Array(result["repositories"]) {
			repo := agent.Map(value)
			name := agent.String(repo["full_name"])
			if name == "" {
				continue
			}
			var access []string
			for _, a := range agent.Array(repo["access"]) {
				access = append(access, agent.String(a))
			}
			out.Repositories = append(out.Repositories, ResourceRepository{Name: name, URL: agent.String(repo["url"]), CloneURL: agent.String(repo["clone_url"]), Access: access})
		}
	}
	return out, nil
}

// mentionPattern is a resource token: the kind, then a quoted name or a
// bare word. A bare word may end with punctuation the sentence owns,
// which the resolver gives back.
var mentionPattern = regexp.MustCompile(`@((?i:doc|repo|preview)):(?:"([^"\n]*)"|(\S+))`)

// HasMentions reports whether text has a resource token to expand, so a
// message without one costs no policy round trip.
func HasMentions(text string) bool { return mentionPattern.MatchString(text) }

// MentionToken is the token the composer inserts for a resource: the
// name quoted when it has whitespace or is empty, its own quotes dropped.
// The TUI and the web build the same (composer.ts mentionToken).
func MentionToken(kind, name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), `"`, "")
	if name == "" || strings.ContainsAny(name, " \t\n") {
		return "@" + kind + `:"` + name + `"`
	}
	return "@" + kind + ":" + name
}

// ExpandMentions replaces every resource token in text that names one of
// the resources with what the agent can act on; a token that names none
// stays as written, and trailing punctuation of a bare token is kept
// after the expansion. It returns the text and how many tokens expanded.
func ExpandMentions(text string, r Resources) (string, int) {
	n := 0
	out := mentionPattern.ReplaceAllStringFunc(text, func(token string) string {
		m := mentionPattern.FindStringSubmatch(token)
		kind, name, trail := strings.ToLower(m[1]), m[2], ""
		if m[3] != "" {
			name, trail = trimTrail(m[3])
		}
		expansion := expandMention(kind, name, r)
		if expansion == "" && trail != "" {
			// A shorter bare name with its punctuation: `@repo:o/n.` first
			// tries "o/n", then "o/n." as typed.
			expansion = expandMention(kind, m[3], r)
			if expansion != "" {
				trail = ""
			}
		}
		if expansion == "" {
			return token
		}
		n++
		return expansion + trail
	})
	return out, n
}

// trimTrail splits punctuation a sentence leaves at the end of a bare
// token ("@repo:o/n," → "o/n" and ",") from the name.
func trimTrail(word string) (name, trail string) {
	name = strings.TrimRight(word, ".,;:!?)]}'\"")
	return name, word[len(name):]
}

// expandMention is the agent-facing form of one resource, or "" when the
// name matches none (case-insensitively, whitespace collapsed).
func expandMention(kind, name string, r Resources) string {
	key := mentionKey(name)
	if key == "" {
		return ""
	}
	switch kind {
	case "doc":
		for _, d := range r.Documents {
			if mentionKey(d.Title) == key || d.ID == name {
				return documentMention(d)
			}
		}
	case "repo":
		for _, repo := range r.Repositories {
			if mentionKey(repo.Name) == key {
				return repositoryMention(repo)
			}
		}
	case "preview":
		for _, p := range r.Previews {
			if mentionKey(p.Title) == key || p.ID == name {
				return previewMention(p)
			}
		}
	}
	return ""
}

// mentionKey is how names compare: lower-cased, whitespace collapsed.
func mentionKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func documentMention(d ResourceDocument) string {
	kind := d.Kind
	if kind == "" {
		kind = "document"
	}
	access := d.Access
	if access == "" {
		access = "read"
	}
	title := d.Title
	if title == "" {
		title = d.ID
	}
	return fmt.Sprintf("the shared Google %s %q (document_id %s, %s access%s)", kind, title, d.ID, access, urlNote(d.URL))
}

func repositoryMention(repo ResourceRepository) string {
	access := "read access"
	if len(repo.Access) > 0 {
		access = "read access: " + strings.Join(repo.Access, ", ")
	}
	clone := repo.CloneURL
	if clone == "" {
		clone = "https://github.com/" + repo.Name + ".git"
	}
	return fmt.Sprintf("the shared repository %s (clone URL %s, %s)", repo.Name, clone, access)
}

func previewMention(p ResourcePreview) string {
	title := p.Title
	if title == "" {
		title = fmt.Sprintf("port %d", p.Port)
	}
	port := ""
	if p.Port > 0 {
		port = fmt.Sprintf(", port %d", p.Port)
	}
	return fmt.Sprintf("the preview %q (%s%s)", title, p.URL, port)
}

func urlNote(url string) string {
	if url == "" {
		return ""
	}
	return ", " + url
}

// expandMessage is the text the agent gets for a user message: its
// resource tokens expanded against what the chat shares now (looked up
// only when there is a token), the rest as written.
func (e *Engine) expandMessage(ctx context.Context, c *Chat, text string) string {
	if !HasMentions(text) {
		return text
	}
	resources, err := e.Resources(ctx, c.ID)
	if err != nil {
		return text
	}
	expanded, _ := ExpandMentions(text, resources)
	return expanded
}
