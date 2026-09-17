package chats

import (
	"encoding/json"
	"net/http"
	"warden/chat/internal/sandbox"
)

// pathsHTTP completes a partial workspace path for the composer's
// @-mentions: GET chats/{id}/paths?q=src/comp answers {"paths": [...]}
// from the worker's "paths" op (a bounded, symlink-refusing listing made
// inside the guest). The names come from the agent's workspace, so the
// client shows them as text only. A stopped sandbox is an error the
// composer shows in place of the list.
func (h *HTTP) pathsHTTP(w http.ResponseWriter, r *http.Request, chatID string) {
	query := r.URL.Query().Get("q")
	if len(query) > sandbox.MaxPathQuery {
		http.Error(w, "path query too long", 400)
		return
	}
	c := h.Engine.Store.Snapshot().chat(chatID)
	if c == nil {
		http.Error(w, "chat not found", 404)
		return
	}
	req := request(c, "paths")
	req.Directory = query
	res, err := h.Engine.Worker.Call(r.Context(), req)
	if err != nil {
		respond(w, nil, err)
		return
	}
	paths := res.Paths
	if paths == nil {
		paths = []string{}
	}
	json.NewEncoder(w).Encode(map[string][]string{"paths": paths})
}
