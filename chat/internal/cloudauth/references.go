package cloudauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reference is metadata only. Resolving a link never grants access to its target.
type Reference struct {
	Status   string `json:"status,omitempty"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Subtitle string `json:"subtitle,omitempty"`
}

var referenceID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

type referenceKey struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func referenceRank(title, query string) int {
	title, query = strings.ToLower(title), strings.ToLower(query)
	if title == query {
		return 0
	}
	if strings.HasPrefix(title, query) {
		return 1
	}
	return 2
}
func rankReferences(items []Reference, query string, limit int) []Reference {
	sort.Slice(items, func(i, j int) bool {
		a, b := referenceRank(items[i].Title, query), referenceRank(items[j].Title, query)
		if a != b {
			return a < b
		}
		if items[i].Title != items[j].Title {
			return items[i].Title < items[j].Title
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].ID < items[j].ID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

// ReferenceRoute is handled before the chat proxy. Identity and scope come only
// from the session, never a caller-supplied organization or target URL.
func (a *Auth) ReferenceRoute(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/references/search" && r.URL.Path != "/api/references/resolve" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	s, err := a.current(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "Sign in required"})
		return true
	}
	if s.OrganizationID == "" || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != a.Origin) {
		respond(w, 403, map[string]string{"error": "Choose an organization"})
		return true
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	var keys []referenceKey
	resolve := r.URL.Path == "/api/references/resolve"
	if resolve {
		if r.Method != "POST" || !a.csrf(r, s) {
			respond(w, 403, map[string]string{"error": "Request refused"})
			return true
		}
		var input struct {
			References []referenceKey `json:"references"`
		}
		if decode(r, &input) != nil || len(input.References) > 50 {
			respond(w, 400, map[string]string{"error": "Use at most 50 references"})
			return true
		}
		for _, key := range input.References {
			if !referenceID.MatchString(key.ID) || (key.Kind != "chat" && key.Kind != "document" && key.Kind != "shared_conversation") {
				respond(w, 400, map[string]string{"error": "Invalid reference"})
				return true
			}
		}
		keys = input.References
	} else if r.Method != "GET" || len(query) > 160 {
		respond(w, 400, map[string]string{"error": "Invalid search"})
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	items := []Reference{}
	// Each provider is independently bounded. No transcript or document bodies
	// are selected. Chat records are the existing durable metadata projection.
	type providerResult struct {
		items []Reference
		err   error
	}
	results := make(chan providerResult, 3)
	count := 0
	for _, kind := range []string{"chat", "shared_conversation", "document"} {
		ids := []string{}
		for _, k := range keys {
			if k.Kind == kind {
				ids = append(ids, k.ID)
			}
		}
		if resolve && len(ids) == 0 {
			continue
		}
		count++
		go func(kind string, ids []string) {
			var found []Reference
			var err error
			if kind == "document" {
				found, err = a.documentReferences(ctx, s.OrganizationID, query, ids, resolve)
			} else {
				found, err = a.conversationReferences(ctx, s.OrganizationID, kind, query, ids, resolve)
			}
			results <- providerResult{found, err}
		}(kind, ids)
	}
	for i := 0; i < count; i++ {
		result := <-results
		if result.err != nil {
			respond(w, 503, map[string]string{"error": "Search is temporarily unavailable. Try again."})
			return true
		}
		items = append(items, result.items...)
	}
	limit := 12
	if requested, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && requested > 0 && requested < limit {
		limit = requested
	}
	if resolve {
		limit = 50
	}
	respond(w, 200, map[string]any{"items": rankReferences(items, query, limit)})
	return true
}
func (a *Auth) conversationReferences(ctx context.Context, org, kind, query string, ids []string, resolve bool) ([]Reference, error) {
	source := `SELECT id,record->>'title' AS title FROM warden_cloud.chat_records WHERE instance='main' AND record->>'organizationID'=$1`
	if kind == "shared_conversation" {
		source = `SELECT id,title FROM warden_cloud.shared_conversations WHERE organization_id=$1`
	}
	// strpos treats user % and _ literally; parameters never become SQL syntax.
	statement := `SELECT id,COALESCE(NULLIF(title,''),'Untitled') FROM (` + source + `) scoped WHERE `
	var args []any = []any{org}
	if resolve {
		encoded, _ := json.Marshal(ids)
		args = append(args, string(encoded))
		statement += `id IN (SELECT jsonb_array_elements_text($2::jsonb)) ORDER BY title,id LIMIT 50`
	} else {
		args = append(args, strings.ToLower(query))
		statement += `strpos(lower(COALESCE(title,'')),$2)>0 ORDER BY CASE WHEN lower(title)=$2 THEN 0 WHEN strpos(lower(title),$2)=1 THEN 1 ELSE 2 END,title,id LIMIT 12`
	}
	rows, err := a.Login.Store.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Reference{}
	for rows.Next() {
		item := Reference{Kind: kind}
		if err = rows.Scan(&item.ID, &item.Title); err != nil {
			return nil, err
		}
		item.URL = "/?chat=" + item.ID
		item.Subtitle = "Chat"
		if kind == "shared_conversation" {
			item.URL = "/shared-conversations/" + item.ID
			item.Subtitle = "Shared conversation"
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func (a *Auth) documentReferences(ctx context.Context, org, query string, ids []string, resolve bool) ([]Reference, error) {
	raw, _ := json.Marshal(map[string]any{"action": "references", "query": query, "ids": ids, "resolve": resolve})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.DocsAddress, "/")+"/internal/workspace-documents", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Panta-Key", a.DocsKey)
	req.Header.Set("X-Panta-Organization", org)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, io.ErrUnexpectedEOF
	}
	var result struct {
		Items []Reference `json:"items"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 128<<10)).Decode(&result)
	return result.Items, err
}
