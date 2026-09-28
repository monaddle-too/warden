package chats

import (
	"encoding/json"
	"net/http"
	"strings"
)

// organizationView removes every other organization's chat and workspace
// metadata before the HTTP response or event stream is encoded. The stored
// state is not mutated.
func organizationView(view View, organization string) View {
	chats := make([]*Chat, 0, len(view.Chats))
	chatIDs := map[string]bool{}
	sandboxes := map[string]bool{}
	for _, c := range view.Chats {
		if c.OrganizationID != organization {
			continue
		}
		chats = append(chats, c)
		chatIDs[c.ID] = true
		sandboxes[c.SandboxID] = true
	}
	view.Chats = chats
	ports := make([]PortBinding, 0, len(view.Ports))
	for _, p := range view.Ports {
		if chatIDs[p.ChatID] {
			ports = append(ports, p)
		}
	}
	view.Ports = ports
	deleted := make([]string, 0, len(view.DeletedSandboxes))
	for _, id := range view.DeletedSandboxes {
		if sandboxes[id] {
			deleted = append(deleted, id)
		}
	}
	view.DeletedSandboxes = deleted
	envs := map[string]*EnvironmentRecord{}
	for id, record := range view.Environments {
		if sandboxes[id] {
			envs[id] = record
		}
	}
	view.Environments = envs
	view.Instructions = nil
	return view
}
func (h *HTTP) viewForOrganization(organization string) []byte {
	view := organizationView(h.Engine.View(), organization)
	data, _ := json.Marshal(view)
	return data
}

// organizationGuard authorizes every resource-addressed API route before
// its handler can read a chat, sandbox, attachment, image, or preview.
func (h *HTTP) organizationGuard(w http.ResponseWriter, path, organization string) bool {
	st := h.Engine.Store.Snapshot()
	parts := strings.Split(path, "/")
	deny := func() bool { http.Error(w, "not found", http.StatusNotFound); return false }
	if len(parts) == 0 {
		return deny()
	}
	switch parts[0] {
	case "chats":
		if len(parts) == 1 || len(parts) > 1 && parts[1] == "search" {
			return true
		}
		c := st.chat(parts[1])
		if c == nil || c.OrganizationID != organization {
			return deny()
		}
		return true
	case "environments":
		if len(parts) == 1 {
			return true
		}
		for _, c := range st.Chats {
			if c.SandboxID == parts[1] && c.OrganizationID == organization {
				return true
			}
		}
		return deny()
	case "ports":
		if len(parts) == 1 {
			return true
		}
		for _, p := range st.Ports {
			if p.ID == parts[1] {
				c := st.chat(p.ChatID)
				if c != nil && c.OrganizationID == organization {
					return true
				}
			}
		}
		return deny()
	case "sharing":
		// Existing Google/GitHub sharing is still globally stored. Keep it
		// closed in cloud organization mode until its policy rows are scoped.
		http.Error(w, "organization sharing is not ready", http.StatusForbidden)
		return false
	case "state", "events", "capacity", "spend", "cluster", "me":
		return true
	default:
		return deny()
	}
}
