package kube

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"

	api "warden/chat/internal/kube"
)

// The source-pod check of plan decision 4: the shared gateway admits a
// binding's credential only from the pod that runs the binding's sandbox.
// It is defence in depth behind the credential (anti-spoofing is a CNI
// property), so it fails closed whenever the pod is unknown.

// sources is the watch-fed map from runtime name to the address of its one
// live pod.
type sources struct {
	mu     sync.Mutex
	synced bool
	byName map[string]map[string]string // runtime name -> pod UID -> pod IP
}

func (s *sources) apply(ev api.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Type {
	case api.Synced:
		s.synced = true
		return
	case api.Added, api.Modified, api.Deleted:
	default:
		return
	}
	var pod api.Pod
	if ev.Decode(&pod) != nil {
		return
	}
	name := pod.Metadata.Labels[LabelSandbox]
	if name == "" || pod.Metadata.UID == "" {
		return
	}
	if s.byName == nil {
		s.byName = map[string]map[string]string{}
	}
	pods := s.byName[name]
	if pods == nil {
		pods = map[string]string{}
		s.byName[name] = pods
	}
	// A pod counts while it is running with an address and not being
	// deleted; anything else is dropped from the map.
	if ev.Type == api.Deleted || pod.Metadata.DeletionTimestamp != nil || pod.Status.Phase != "Running" || pod.Status.PodIP == "" {
		delete(pods, pod.Metadata.UID)
	} else {
		pods[pod.Metadata.UID] = pod.Status.PodIP
	}
	if len(pods) == 0 {
		delete(s.byName, name)
	}
}

// allows reports whether remote is the address of the runtime's single live
// pod. Two live pods (a replaced generation still terminating without a
// deletion timestamp) are ambiguous and refused.
func (s *sources) allows(runtimeName string, remote net.IP) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.synced || runtimeName == "" || remote == nil {
		return false
	}
	pods := s.byName[runtimeName]
	if len(pods) != 1 {
		return false
	}
	for _, ip := range pods {
		return net.ParseIP(ip) != nil && net.ParseIP(ip).Equal(remote)
	}
	return false
}

// watchSources follows the sandbox pods for the source check.
func (i *Inspector) watchSources(ctx context.Context) error {
	events, err := i.o.Client.ListWatch(ctx, api.Pods, i.o.Namespace, api.ListOptions{LabelSelector: LabelSandbox})
	if err != nil {
		return errors.New("watch sandbox pods: " + err.Error())
	}
	go func() {
		for ev := range events {
			if ev.Type == api.Error && ev.Err != nil {
				log.Printf("kube inspector: watch sandbox pods: %v", ev.Err)
				continue
			}
			i.sources.apply(ev)
		}
	}()
	return nil
}

// SourceAllowed is the shared gateway's source check: the credential of the
// binding running as runtimeName is honoured only from that pod's address.
// Loopback is the policy pod itself (health probes) and is always allowed.
func (i *Inspector) SourceAllowed(runtimeName string, remote net.IP) bool {
	if remote != nil && remote.IsLoopback() {
		return true
	}
	return i.sources.allows(runtimeName, remote)
}
