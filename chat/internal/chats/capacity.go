package chats

import (
	"context"
	"errors"
	"time"

	"warden/chat/internal/sandbox"
)

// capacityTTL bounds how long one capacity answer is reused: the picker
// polls every few seconds and a Kubernetes read lists the nodes.
const capacityTTL = 3 * time.Second

// Capacity is what the execution host has and what running workspaces
// hold of it (sandbox.Capacity), for the size picker on the New chat
// form and in the workspace panel. It is the runner's answer, cached for
// capacityTTL; an error is the runner being unreachable, while a host
// the runner cannot read comes back with its Error field set and the
// reservations filled.
func (e *Engine) Capacity(ctx context.Context) (*sandbox.Capacity, error) {
	if e.Worker == nil {
		return nil, errors.New("the runner is unavailable")
	}
	e.capacityMu.Lock()
	defer e.capacityMu.Unlock()
	if e.capacity != nil && e.now().Before(e.capacityAt.Add(capacityTTL)) {
		return e.capacity, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := e.Worker.Call(ctx, sandbox.Request{Operation: "capacity", ProjectID: "warden-local", PrincipalID: "owner"})
	if err != nil {
		return nil, err
	}
	if res.Capacity == nil {
		return nil, errors.New("the runner reported no capacity")
	}
	e.capacity, e.capacityAt = res.Capacity, e.now()
	return e.capacity, nil
}
