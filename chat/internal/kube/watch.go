package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// EventType is a watch event's type.
type EventType string

// The event types. Synced is never sent by the server; ListWatch sends it
// after each full list.
const (
	Added    EventType = "ADDED"
	Modified EventType = "MODIFIED"
	Deleted  EventType = "DELETED"
	Bookmark EventType = "BOOKMARK"
	Error    EventType = "ERROR"
	Synced   EventType = "SYNCED"
)

// Event is one watch event. Object is the object as the server sent it
// (for Bookmark only its metadata, for Error a Status, for Synced empty);
// ResourceVersion is the object's, or the list's for Synced. Err is set on
// Error events: the server's Status as a *StatusError, or the local failure
// that ended the stream.
type Event struct {
	Type            EventType
	Object          json.RawMessage
	ResourceVersion string
	Err             error
}

// Decode unmarshals the event's object into into.
func (e Event) Decode(into any) error {
	if len(e.Object) == 0 {
		return errors.New("kube: event has no object")
	}
	return json.Unmarshal(e.Object, into)
}

// Key returns namespace/name (or name for cluster-scoped objects).
func (e Event) Key() string {
	meta := e.meta()
	if meta.Namespace == "" {
		return meta.Name
	}
	return meta.Namespace + "/" + meta.Name
}

func (e Event) meta() ObjectMeta {
	var obj struct {
		Metadata ObjectMeta `json:"metadata"`
	}
	_ = json.Unmarshal(e.Object, &obj)
	return obj.Metadata
}

// WatchOptions shape a Watch. ResourceVersion resumes from a list's or an
// earlier event's version; empty starts with the current state as Added
// events. AllowBookmarks asks for Bookmark events, which carry a
// resourceVersion to resume from without an object. TimeoutSeconds asks
// the server to end the watch after that long (it ends watches on its own
// schedule anyway).
type WatchOptions struct {
	LabelSelector   string
	FieldSelector   string
	ResourceVersion string
	AllowBookmarks  bool
	TimeoutSeconds  int
}

// Watch opens one watch stream. Semantics: the channel delivers events in
// order; it closes when ctx is done, when the server ends the stream (a
// normal end, resume with the last ResourceVersion) or after an Error
// event, which is always the last one (a 410 Gone, IsGone(ev.Err), means
// the version is too old and a new list is needed). The caller must drain
// the channel or cancel ctx; nothing is buffered beyond a few events. A
// request that fails outright (RBAC, a bad selector, an unreachable
// server) is returned as an error instead of a channel.
func (c *Client) Watch(ctx context.Context, r Resource, namespace string, opts WatchOptions) (<-chan Event, error) {
	q := url.Values{"watch": {"true"}}
	if opts.LabelSelector != "" {
		q.Set("labelSelector", opts.LabelSelector)
	}
	if opts.FieldSelector != "" {
		q.Set("fieldSelector", opts.FieldSelector)
	}
	if opts.ResourceVersion != "" {
		q.Set("resourceVersion", opts.ResourceVersion)
	}
	if opts.AllowBookmarks {
		q.Set("allowWatchBookmarks", "true")
	}
	if opts.TimeoutSeconds > 0 {
		q.Set("timeoutSeconds", strconv.Itoa(opts.TimeoutSeconds))
	}
	ctx, cancel := context.WithCancel(ctx)
	resp, err := c.stream(ctx, http.MethodGet, r.path(namespace, ""), q, "application/json")
	if err != nil {
		cancel()
		return nil, err
	}
	ch := make(chan Event, 4)
	go func() {
		defer close(ch)
		defer cancel()
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			var raw struct {
				Type   EventType       `json:"type"`
				Object json.RawMessage `json:"object"`
			}
			if err := dec.Decode(&raw); err != nil {
				if ctx.Err() != nil || errors.Is(err, io.EOF) {
					return
				}
				send(ctx, ch, Event{Type: Error, Err: err})
				return
			}
			ev := Event{Type: raw.Type, Object: raw.Object}
			if raw.Type == Error {
				var status Status
				_ = json.Unmarshal(raw.Object, &status)
				ev.Err = statusErrorFrom(status)
			} else {
				ev.ResourceVersion = ev.meta().ResourceVersion
			}
			if !send(ctx, ch, ev) || ev.Type == Error {
				return
			}
		}
	}()
	return ch, nil
}

func send(ctx context.Context, ch chan<- Event, ev Event) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// ListWatch keeps a continuous view of a collection: it lists, sends the
// items as Added events followed by one Synced event, then watches from
// the list's resourceVersion with bookmarks. When the server ends a watch
// it resumes from the last version seen; when a watch answers 410 Gone (or
// any other server error) it lists again and sends the difference against
// what it had delivered: Added for new objects, Modified for objects whose
// resourceVersion changed, Deleted (with the last known object) for
// objects that are gone, then Synced. A consumer that applies the events
// to a map keyed by Event.Key therefore always converges on the server's
// state. Failures are retried with backoff and reported as Error events
// that do not end the stream; the channel closes only when ctx is done.
// Only the first list can fail the call, so a missing permission is seen
// immediately.
func (c *Client) ListWatch(ctx context.Context, r Resource, namespace string, opts ListOptions) (<-chan Event, error) {
	known, version, err := c.listRaw(ctx, r, namespace, opts)
	if err != nil {
		return nil, err
	}
	ch := make(chan Event, 4)
	go func() {
		defer close(ch)
		lw := &listWatcher{c: c, r: r, namespace: namespace, opts: opts, ch: ch, known: map[string]knownObject{}, version: version}
		if !lw.deliverList(ctx, known) {
			return
		}
		lw.run(ctx)
	}()
	return ch, nil
}

type knownObject struct {
	resourceVersion string
	raw             json.RawMessage
}

type listWatcher struct {
	c         *Client
	r         Resource
	namespace string
	opts      ListOptions
	ch        chan Event
	known     map[string]knownObject
	version   string
}

// listRaw lists the collection as raw items keyed by namespace/name.
func (c *Client) listRaw(ctx context.Context, r Resource, namespace string, opts ListOptions) (map[string]knownObject, string, error) {
	var list List[json.RawMessage]
	if err := c.List(ctx, r, namespace, opts, &list); err != nil {
		return nil, "", err
	}
	items := make(map[string]knownObject, len(list.Items))
	for _, raw := range list.Items {
		ev := Event{Object: raw}
		items[ev.Key()] = knownObject{resourceVersion: ev.meta().ResourceVersion, raw: raw}
	}
	return items, list.Metadata.ResourceVersion, nil
}

// deliverList sends the difference between the known set and a fresh list,
// then Synced, and makes the list the known set.
func (lw *listWatcher) deliverList(ctx context.Context, fresh map[string]knownObject) bool {
	keys := make([]string, 0, len(fresh)+len(lw.known))
	for k := range fresh {
		keys = append(keys, k)
	}
	for k := range lw.known {
		if _, ok := fresh[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		now, ok := fresh[k]
		old, had := lw.known[k]
		var ev Event
		switch {
		case ok && !had:
			ev = Event{Type: Added, Object: now.raw, ResourceVersion: now.resourceVersion}
		case ok && had && old.resourceVersion != now.resourceVersion:
			ev = Event{Type: Modified, Object: now.raw, ResourceVersion: now.resourceVersion}
		case !ok:
			ev = Event{Type: Deleted, Object: old.raw, ResourceVersion: old.resourceVersion}
		default:
			continue
		}
		if !send(ctx, lw.ch, ev) {
			return false
		}
	}
	lw.known = fresh
	return send(ctx, lw.ch, Event{Type: Synced, ResourceVersion: lw.version})
}

func (lw *listWatcher) run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	relist := false
	for ctx.Err() == nil {
		if relist {
			fresh, version, err := lw.c.listRaw(ctx, lw.r, lw.namespace, lw.opts)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if !send(ctx, lw.ch, Event{Type: Error, Err: err}) || !sleep(ctx, backoff) {
					return
				}
				backoff = nextBackoff(backoff)
				continue
			}
			lw.version = version
			if !lw.deliverList(ctx, fresh) {
				return
			}
			relist = false
			backoff = 500 * time.Millisecond
		}
		events, err := lw.c.Watch(ctx, lw.r, lw.namespace, WatchOptions{
			LabelSelector: lw.opts.LabelSelector, FieldSelector: lw.opts.FieldSelector,
			ResourceVersion: lw.version, AllowBookmarks: true,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !send(ctx, lw.ch, Event{Type: Error, Err: err}) {
				return
			}
			if IsGone(err) {
				relist = true
				continue
			}
			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		started := time.Now()
		alive := false
		for ev := range events {
			switch ev.Type {
			case Error:
				// The server ended the watch with a Status; whatever the
				// reason, a fresh list is the safe way to continue.
				if !send(ctx, lw.ch, ev) {
					return
				}
				relist = true
			case Bookmark:
				lw.version = ev.ResourceVersion
				alive = true
			case Deleted:
				delete(lw.known, ev.Key())
				lw.version = ev.ResourceVersion
				alive = true
				if !send(ctx, lw.ch, ev) {
					return
				}
			default:
				lw.known[ev.Key()] = knownObject{resourceVersion: ev.ResourceVersion, raw: ev.Object}
				lw.version = ev.ResourceVersion
				alive = true
				if !send(ctx, lw.ch, ev) {
					return
				}
			}
		}
		if alive || time.Since(started) > healthyWatch {
			// A watch that delivered something or lived a while ended on
			// the server's schedule; reconnect at once.
			backoff = 500 * time.Millisecond
		} else if !relist {
			// A watch that ended at once without delivering anything is
			// retried with backoff so a flapping server is not hammered.
			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// healthyWatch is how long a watch must last, without events, to count as
// a normal server-side end rather than a failure.
const healthyWatch = 10 * time.Second

func nextBackoff(d time.Duration) time.Duration {
	if d *= 2; d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
