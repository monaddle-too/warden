package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

// Kubernetes events for the owner (docs/startup-detail-events-plan.md):
// the pod's own, newest first, on the workspace panel and the cluster
// page, and the newest of the two namespaces on the cluster page. They
// are what kubectl describe lists, and the only place the autoscaler says
// whether a node is coming.
const (
	// PodEventsMax bounds a pod's list; ClusterEventsMax the cluster's.
	PodEventsMax     = 20
	ClusterEventsMax = 100
)

// podEvents lists the events about one pod, newest first. A refusal
// (no events verb in the Role) is returned; the callers show an empty
// list rather than failing the pod.
func (d *Driver) podEvents(ctx context.Context, namespace, uid string) ([]sandbox.Event, error) {
	var list kube.List[kube.CoreEvent]
	if err := d.client.List(ctx, kube.Events, namespace, kube.ListOptions{FieldSelector: "involvedObject.uid=" + uid}, &list); err != nil {
		return nil, err
	}
	return eventInfos(list.Items, PodEventsMax), nil
}

// namespaceEvents lists a namespace's events, in the API's order.
func (d *Driver) namespaceEvents(ctx context.Context, namespace string) ([]kube.CoreEvent, error) {
	var list kube.List[kube.CoreEvent]
	if err := d.client.List(ctx, kube.Events, namespace, kube.ListOptions{}, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// eventInfos converts and sorts events newest first, keeping at most max.
func eventInfos(events []kube.CoreEvent, max int) []sandbox.Event {
	out := make([]sandbox.Event, 0, len(events))
	for i := range events {
		out = append(out, eventInfo(&events[i]))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func eventInfo(e *kube.CoreEvent) sandbox.Event {
	info := sandbox.Event{At: e.At(), Type: e.Type, Reason: e.Reason, Message: strings.TrimSpace(e.Message), Hint: EventHint(e.Reason, e.Message), Count: int(e.Count), Kind: e.InvolvedObject.Kind, Namespace: e.InvolvedObject.Namespace, Name: e.InvolvedObject.Name, Source: e.Source.Component}
	if info.Count < 1 {
		info.Count = 1
	}
	if info.Source == "" {
		info.Source = e.ReportingComponent
	}
	if info.Namespace == "" {
		info.Namespace = e.Metadata.Namespace
	}
	return info
}

// EventHint reads an event in the owner's words, for the reasons that
// decide how a start goes: the autoscaler's answer to an unschedulable
// pod, the scheduler's placement, the image pull, the workspace volume,
// the container runtime. Other events have no hint; a Warning without one
// is still shown as its reason and message.
func EventHint(reason, message string) string {
	message = strings.TrimSpace(message)
	first, _, _ := strings.Cut(message, ". ")
	first = strings.TrimSuffix(first, ".")
	switch reason {
	case "TriggeredScaleUp":
		return "a node is being added"
	case "NotTriggerScaleUp":
		if _, why, ok := strings.Cut(first, ": "); ok && why != "" {
			return "no node can be added: " + why
		}
		return "no node can be added"
	case "FailedScheduling":
		if first != "" {
			return "no node fits yet: " + first
		}
		return "no node fits yet"
	case "Scheduled":
		if _, node, ok := strings.Cut(message, " to "); ok && node != "" {
			return "placed on node " + strings.TrimSuffix(strings.TrimSpace(node), ".")
		}
		return "placed on a node"
	case "Pulling":
		return "pulling the container image"
	case "Pulled":
		if strings.Contains(message, "already present") {
			return "the container image was already on the node"
		}
		if i := strings.Index(message, " in "); i >= 0 {
			took, _, _ := strings.Cut(message[i+4:], " (")
			return "the container image is pulled (" + strings.TrimSpace(took) + ")"
		}
		return "the container image is pulled"
	case "Failed":
		if strings.Contains(message, "pull") || strings.Contains(message, "image") {
			return "the container image cannot be pulled: " + first
		}
		return "the container failed to start: " + first
	case "BackOff":
		if strings.Contains(message, "image") {
			return "retrying the image pull after failures"
		}
		return "the container keeps exiting; restarts are backing off"
	case "FailedMount", "FailedAttachVolume":
		return "the workspace volume is not attached yet: " + first
	case "SuccessfulAttachVolume":
		return "the workspace volume is attached"
	case "FailedCreatePodSandBox":
		return "the sandbox runtime could not start the pod: " + first
	case "Created":
		return "the container is created"
	case "Started":
		return "the container is started"
	case "Unhealthy":
		return "a health probe failed: " + first
	case "Killing":
		return "the container is being stopped"
	case "Evicted":
		return "evicted from its node: " + first
	case "Preempting", "Preempted":
		return "preempted by a higher-priority pod"
	case "NodeNotReady":
		return "its node stopped answering"
	case "OOMKilling", "OOMKilled":
		return "killed for exceeding its memory"
	}
	return ""
}

// eventDetail is what an event adds to a startup detail: its hint, or a
// Warning's reason and first sentence; nothing for a plain Normal event.
func eventDetail(e sandbox.Event) string {
	if e.Hint != "" {
		return e.Hint
	}
	if e.Type == "Warning" {
		first, _, _ := strings.Cut(e.Message, ". ")
		if first == "" {
			return e.Reason
		}
		return fmt.Sprintf("%s: %s", e.Reason, strings.TrimSuffix(first, "."))
	}
	return ""
}
