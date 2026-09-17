package kube

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWatchEvents(t *testing.T) {
	api := newFakeAPI(t)
	api.seed(Pods, "ns", podObject("existing", nil))
	c := api.client()
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()

	events, err := c.Watch(ctx, Pods, "ns", WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	req := api.lastRequest()
	if req.Query.Get("watch") != "true" || req.Query.Has("resourceVersion") || req.Query.Has("allowWatchBookmarks") {
		t.Fatalf("query %v", req.Query)
	}
	ev := nextEvent(t, events)
	if ev.Type != Added || ev.Key() != "ns/existing" || ev.ResourceVersion == "" {
		t.Fatalf("initial state: %+v", ev)
	}
	var pod Pod
	if err := ev.Decode(&pod); err != nil || pod.Metadata.Name != "existing" {
		t.Fatalf("decode %+v %v", pod, err)
	}

	if err := c.Create(ctx, Pods, "ns", Pod{Metadata: ObjectMeta{Name: "new"}}, nil); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events); ev.Type != Added || ev.Key() != "ns/new" {
		t.Fatalf("create: %+v", ev)
	}
	if err := c.Patch(ctx, Pods, "ns", "new", MergePatchLabels(map[string]string{"a": "b"}), &pod); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, events)
	if ev.Type != Modified || ev.ResourceVersion != pod.Metadata.ResourceVersion {
		t.Fatalf("patch: %+v (pod version %s)", ev, pod.Metadata.ResourceVersion)
	}
	if err := c.Delete(ctx, Pods, "ns", "new", DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events); ev.Type != Deleted || ev.Key() != "ns/new" {
		t.Fatalf("delete: %+v", ev)
	}
	// Another namespace's pod is not seen; cancelling closes the channel.
	if err := c.Create(ctx, Pods, "other", Pod{Metadata: ObjectMeta{Name: "elsewhere"}}, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	expectClosed(t, events)
	if ev, ok := <-events; ok {
		t.Fatalf("event after close: %+v", ev)
	}
}

func TestWatchResumeSelectorsAndBookmarks(t *testing.T) {
	api := newFakeAPI(t)
	api.seed(Pods, "ns", podObject("old", map[string]string{"warden.monaddle.com/egress": "gateway"}))
	c := api.client()
	ctx := testContext(t)
	var list List[Pod]
	if err := c.List(ctx, Pods, "ns", ListOptions{}, &list); err != nil {
		t.Fatal(err)
	}
	// Changes between the list and the watch are delivered on resume.
	if err := c.Create(ctx, Pods, "ns", Pod{Metadata: ObjectMeta{Name: "between", Labels: map[string]string{"warden.monaddle.com/egress": "gateway"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, Pods, "ns", Pod{Metadata: ObjectMeta{Name: "unlabelled"}}, nil); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := c.Watch(wctx, Pods, "ns", WatchOptions{ResourceVersion: list.Metadata.ResourceVersion, LabelSelector: "warden.monaddle.com/egress=gateway", AllowBookmarks: true, TimeoutSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	q := api.lastRequest().Query
	if q.Get("resourceVersion") != list.Metadata.ResourceVersion || q.Get("labelSelector") != "warden.monaddle.com/egress=gateway" || q.Get("allowWatchBookmarks") != "true" || q.Get("timeoutSeconds") != "300" {
		t.Fatalf("query %v", q)
	}
	if ev := nextEvent(t, events); ev.Type != Added || ev.Key() != "ns/between" {
		t.Fatalf("resume: %+v", ev)
	}
	api.bookmark()
	ev := nextEvent(t, events)
	if ev.Type != Bookmark || ev.ResourceVersion != api.currentVersion() || ev.Key() != "" {
		t.Fatalf("bookmark: %+v", ev)
	}
	// The server ends the watch; the channel closes without an error event.
	api.closeWatchers()
	expectClosed(t, events)
}

func TestWatchGoneAndRequestErrors(t *testing.T) {
	api := newFakeAPI(t)
	api.seed(Pods, "ns", podObject("a", nil))
	api.seed(Pods, "ns", podObject("b", nil))
	c := api.client()
	ctx := testContext(t)
	api.expireHistory()
	// As an HTTP status.
	_, err := c.Watch(ctx, Pods, "ns", WatchOptions{ResourceVersion: "1"})
	if !IsGone(err) {
		t.Fatalf("want Gone, got %v", err)
	}
	// As an ERROR event on a 200 stream, which is the last event.
	api.setGoneAsEvent(true)
	events, err := c.Watch(ctx, Pods, "ns", WatchOptions{ResourceVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, events)
	if ev.Type != Error || !IsGone(ev.Err) || !strings.Contains(ev.Err.Error(), "too old resource version") {
		t.Fatalf("error event: %+v", ev)
	}
	expectClosed(t, events)
	// A refused request is an error, not a channel.
	api.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("watch") == "true" {
			writeStatus(w, http.StatusForbidden, "Forbidden", "pods is forbidden")
			return true
		}
		return false
	})
	if _, err := c.Watch(ctx, Pods, "ns", WatchOptions{}); !IsForbidden(err) {
		t.Fatalf("want Forbidden, got %v", err)
	}
	// A stream that breaks mid-event reports the local error and closes.
	api.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"ADDED","object":{"metadata":{"name":"x"}}}` + "\n" + `{"type":"MODI`))
			return true
		}
		return false
	})
	events, err = c.Watch(ctx, Pods, "ns", WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events); ev.Type != Added {
		t.Fatalf("first event %+v", ev)
	}
	if ev := nextEvent(t, events); ev.Type != Error || ev.Err == nil || IsGone(ev.Err) {
		t.Fatalf("broken stream: %+v", ev)
	}
	expectClosed(t, events)
}

func TestListWatch(t *testing.T) {
	api := newFakeAPI(t)
	api.seed(Pods, "ns", podObject("a", map[string]string{"warden.monaddle.com/sandbox": "a"}))
	api.seed(Pods, "ns", podObject("b", map[string]string{"warden.monaddle.com/sandbox": "b"}))
	api.seed(Pods, "ns", podObject("ignored", nil))
	c := api.client()
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()

	events, err := c.ListWatch(ctx, Pods, "ns", ListOptions{LabelSelector: "warden.monaddle.com/sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	cache := map[string]Pod{}
	apply := func(ev Event) {
		t.Helper()
		var pod Pod
		if ev.Type != Synced {
			if err := ev.Decode(&pod); err != nil {
				t.Fatal(err)
			}
		}
		switch ev.Type {
		case Added, Modified:
			cache[ev.Key()] = pod
		case Deleted:
			delete(cache, ev.Key())
		}
	}
	// Initial list: Added in name order, then Synced at the list version.
	for _, key := range []string{"ns/a", "ns/b"} {
		ev := nextEvent(t, events)
		if ev.Type != Added || ev.Key() != key {
			t.Fatalf("initial: %+v", ev)
		}
		apply(ev)
	}
	ev := nextEvent(t, events)
	if ev.Type != Synced || ev.ResourceVersion == "" || ev.Key() != "" {
		t.Fatalf("synced: %+v", ev)
	}
	listVersion := ev.ResourceVersion
	waitFor(t, func() bool { return api.watcherCount() == 1 })
	if q := api.lastRequest().Query; q.Get("resourceVersion") != listVersion || q.Get("allowWatchBookmarks") != "true" || q.Get("labelSelector") != "warden.monaddle.com/sandbox" {
		t.Fatalf("watch query %v", q)
	}

	// Live events flow through.
	if err := c.Create(ctx, Pods, "ns", Pod{Metadata: ObjectMeta{Name: "c", Labels: map[string]string{"warden.monaddle.com/sandbox": "c"}}}, nil); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, events)
	if ev.Type != Added || ev.Key() != "ns/c" {
		t.Fatalf("live: %+v", ev)
	}
	apply(ev)

	// The server ends the watch: it resumes from the last version without
	// listing again, and bookmarks advance that version.
	api.bookmark()
	waitFor(t, func() bool { return api.watcherCount() == 1 })
	before := len(api.recorded())
	api.closeWatchers()
	waitFor(t, func() bool { return api.watcherCount() == 1 })
	resumed := api.recorded()[before:]
	if len(resumed) != 1 || resumed[0].Query.Get("watch") != "true" || resumed[0].Query.Get("resourceVersion") != api.currentVersion() {
		t.Fatalf("resume requests: %+v (version %s)", resumed, api.currentVersion())
	}
	if err := c.Patch(ctx, Pods, "ns", "a", MergePatchLabels(map[string]string{"warden.monaddle.com/egress": "gateway"}), nil); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, events)
	if ev.Type != Modified || ev.Key() != "ns/a" {
		t.Fatalf("after resume: %+v", ev)
	}
	apply(ev)

	// The history expires and the watch ends: a relist delivers the
	// difference (a deleted b, a changed c, a new d) and a Synced.
	if err := c.Delete(ctx, Pods, "ns", "b", DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events); ev.Type != Deleted || ev.Key() != "ns/b" {
		t.Fatalf("delete: %+v", ev)
	} else {
		apply(ev)
	}
	// Expire the history, end the watch and change things behind its back.
	api.expireHistory()
	api.closeWatchers()
	api.mu.Lock()
	delete(api.objects, Pods.path("ns", "a"))
	api.mu.Unlock()
	api.seed(Pods, "ns", podObject("d", map[string]string{"warden.monaddle.com/sandbox": "d"}))
	// Reseed c so its version changes.
	api.seed(Pods, "ns", podObject("c", map[string]string{"warden.monaddle.com/sandbox": "c", "changed": "true"}))
	got := map[EventType][]string{}
	for i := 0; i < 4; i++ {
		ev := nextEvent(t, events)
		if ev.Type == Error {
			if !IsGone(ev.Err) {
				t.Fatalf("unexpected error event: %v", ev.Err)
			}
			i--
			continue
		}
		got[ev.Type] = append(got[ev.Type], ev.Key())
		apply(ev)
	}
	if strings.Join(got[Deleted], ",") != "ns/a" || strings.Join(got[Added], ",") != "ns/d" || strings.Join(got[Modified], ",") != "ns/c" || len(got[Synced]) != 1 {
		t.Fatalf("relist difference: %v", got)
	}
	if _, ok := cache["ns/a"]; ok || cache["ns/c"].Metadata.Labels["changed"] != "true" || cache["ns/d"].Metadata.Name != "d" || len(cache) != 2 {
		t.Fatalf("cache %v", cache)
	}
	// It keeps watching after the relist.
	waitFor(t, func() bool { return api.watcherCount() == 1 })
	if err := c.Delete(ctx, Pods, "ns", "d", DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events); ev.Type != Deleted || ev.Key() != "ns/d" {
		t.Fatalf("after relist: %+v", ev)
	}
	cancel()
	expectClosed(t, events)
}

func TestListWatchFailures(t *testing.T) {
	api := newFakeAPI(t)
	c := api.client()
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	// The first list failing fails the call.
	api.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		writeStatus(w, http.StatusForbidden, "Forbidden", "pods is forbidden")
		return true
	})
	if _, err := c.ListWatch(ctx, Pods, "ns", ListOptions{}); !IsForbidden(err) {
		t.Fatalf("want Forbidden, got %v", err)
	}
	// Later failures are reported and retried; the channel closes only
	// with the context.
	api.setOverride(nil)
	events, err := c.ListWatch(ctx, Pods, "ns", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events); ev.Type != Synced {
		t.Fatalf("want Synced, got %+v", ev)
	}
	waitFor(t, func() bool { return api.watcherCount() == 1 })
	api.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("watch") == "true" {
			writeStatus(w, http.StatusInternalServerError, "InternalError", "etcd is down")
			return true
		}
		return false
	})
	api.closeWatchers()
	ev := nextEvent(t, events)
	if ev.Type != Error || ev.Err == nil || !strings.Contains(ev.Err.Error(), "etcd is down") {
		t.Fatalf("want an informational error, got %+v", ev)
	}
	api.setOverride(nil)
	waitFor(t, func() bool { return api.watcherCount() == 1 })
	if err := c.Create(ctx, Pods, "ns", Pod{Metadata: ObjectMeta{Name: "after"}}, nil); err != nil {
		t.Fatal(err)
	}
	for {
		ev := nextEvent(t, events)
		if ev.Type == Error {
			continue
		}
		if ev.Type != Added || ev.Key() != "ns/after" {
			t.Fatalf("after recovery: %+v", ev)
		}
		break
	}
	cancel()
	expectClosed(t, events)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
