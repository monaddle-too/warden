package chats

import (
	"context"
	"errors"

	cv "warden/chat/internal/conversation"
)

// A workspace's network access is the egress mode its sandbox's gateway
// enforces: "" follows the install's setting (sandboxes.egress in
// warden.json, or the admin console's switch), "restricted" is the policy
// template's destination list, "open" is any public HTTP/HTTPS host with
// credentials still only for approved requests. The owner chooses it when
// the workspace is created (POST chats, the New chat form, warden chat new
// --network) and changes it from the workspace panel
// (environments/{id}/network); agents cannot ask for it (their
// request_network_access stays one host for a bounded time). The policy
// service holds the choice by sandbox ID (sharing/egress_set) and enforces
// it; the chats of the workspace record it so clients can show it.

// NetworkModes are the values a workspace's network access may take.
var NetworkModes = map[string]bool{"": true, "restricted": true, "open": true}

// SetWorkspaceNetwork gives workspace id (a sandbox ID) its own network
// access, or with "" returns it to the install's setting, by actor. The
// policy service is told first, so a refusal leaves the chats unchanged.
func (e *Engine) SetWorkspaceNetwork(ctx context.Context, id, mode string, actor cv.Actor) error {
	st := e.Store.Snapshot()
	chats := st.environmentChats(id)
	if len(chats) == 0 || st.deleted(id) {
		return errors.New("workspace not found")
	}
	if err := e.declareNetwork(ctx, id, mode, actor); err != nil {
		return err
	}
	return e.Store.update(func(st *State) error {
		for _, c := range st.Chats {
			if c.SandboxID == id {
				c.Network = mode
			}
		}
		return nil
	})
}

// createdOnNetwork gives the fresh workspace of a chat CreateFrom just made
// the chosen network access. A shared workspace already has its access; a
// refusal discards the never-used chat and is returned as the creation's
// error.
func (e *Engine) createdOnNetwork(ctx context.Context, id, mode string, actor cv.Actor) (string, error) {
	c := e.Store.Chat(id)
	if c == nil {
		return "", errors.New("chat not found")
	}
	if len(e.Store.Snapshot().environmentChats(c.SandboxID)) > 1 {
		e.discardUnused(id)
		return "", errors.New("a shared workspace already has its network access")
	}
	if err := e.SetWorkspaceNetwork(ctx, c.SandboxID, mode, actor); err != nil {
		e.discardUnused(id)
		return "", err
	}
	return id, nil
}

// declareNetwork tells the policy service a sandbox's own network access
// (mode "" clears it), attributed to actor.
func (e *Engine) declareNetwork(ctx context.Context, sandboxID, mode string, actor cv.Actor) error {
	if !NetworkModes[mode] {
		return errors.New("network access is restricted, open or empty (the install's setting)")
	}
	_, err := e.sharingCall(ctx, "egress_set", map[string]any{"sandboxID": sandboxID, "mode": mode, "actor": sharingActor(actor)})
	return err
}

// sharingActor is how the policy service's sharing store records actor:
// name, else email, else the principal.
func sharingActor(actor cv.Actor) string {
	switch {
	case actor.Name != "":
		return actor.Name
	case actor.Email != "":
		return actor.Email
	case actor.PrincipalID == "":
		return "owner"
	}
	return actor.PrincipalID
}

// discardUnused removes a chat created moments ago and never used (no
// message, no run), as if the creation had failed.
func (e *Engine) discardUnused(id string) {
	_ = e.Store.update(func(st *State) error {
		for i, c := range st.Chats {
			if c.ID == id && c.Status == "idle" && len(c.Conversation.Entries) == 0 {
				st.Chats = append(st.Chats[:i], st.Chats[i+1:]...)
				break
			}
		}
		return nil
	})
}
