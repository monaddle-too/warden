package kube

import (
	"context"
	"errors"
	"log"

	api "warden/chat/internal/kube"
)

// Start watches the objects the cluster proof depends on and invalidates
// the cached proof on any change to them, so the next ClusterFacts (the
// verifier calls it every refresh cycle) proves the cluster again,
// canaries included. Without an event a pass lasts PassTTL (an hour). The
// watches run until ctx ends; a watch that cannot be opened (a missing
// permission) is an error, and nothing is watched.
func (i *Inspector) Start(ctx context.Context) error {
	c := i.o.Client
	watches := []struct {
		what      string
		resource  api.Resource
		namespace string
		opts      api.ListOptions
	}{
		{"network policies", api.NetworkPolicies, i.o.Namespace, api.ListOptions{}},
		{"namespace", api.Namespaces, "", api.ListOptions{FieldSelector: "metadata.name=" + i.o.Namespace}},
		{"runtime class", api.RuntimeClasses, "", api.ListOptions{FieldSelector: "metadata.name=" + i.o.RuntimeClass}},
		{"admission policies", api.ValidatingAdmissionPolicies, "", api.ListOptions{}},
		{"admission policy bindings", api.ValidatingAdmissionPolicyBindings, "", api.ListOptions{}},
	}
	ctx, cancel := context.WithCancel(ctx)
	var streams []<-chan api.Event
	for _, w := range watches {
		events, err := c.ListWatch(ctx, w.resource, w.namespace, w.opts)
		if err != nil {
			cancel()
			return errors.New("watch " + w.what + ": " + err.Error())
		}
		streams = append(streams, events)
	}
	for idx, events := range streams {
		go i.follow(watches[idx].what, events)
	}
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return nil
}

// follow invalidates the proof on every change after the initial list.
func (i *Inspector) follow(what string, events <-chan api.Event) {
	synced := false
	for ev := range events {
		switch ev.Type {
		case api.Synced:
			if synced {
				// A relist after a lost watch: the difference was delivered
				// as events already.
				continue
			}
			synced = true
		case api.Error:
			if ev.Err != nil {
				log.Printf("kube inspector: watch %s: %v", what, ev.Err)
			}
		case api.Added, api.Modified, api.Deleted:
			if synced {
				i.Invalidate()
			}
		}
	}
}
