package kube

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI speaks enough of the Kubernetes API for the client's tests: the
// verbs over an in-memory store with resource versions, label and field
// selectors, JSON merge and apply patches, watch with resume, bookmarks and
// history expiry, a bearer token check, and hooks for exec and log.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	rv       int64
	uid      int64
	objects  map[string]map[string]any // object path -> object
	history  []histEvent
	oldest   int64 // history before this version is gone (410)
	watchers map[*watcher]struct{}
	requests []recordedRequest

	// Knobs, read under mu by serve and set through the setters.
	token       string // required bearer token; empty accepts anything
	goneAsEvent bool   // answer a too-old watch with 200 and an ERROR event instead of 410
	exec        http.HandlerFunc
	logs        http.HandlerFunc
	override    func(w http.ResponseWriter, r *http.Request) bool // handled when true
}

func (api *fakeAPI) setToken(token string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.token = token
}

func (api *fakeAPI) setGoneAsEvent(v bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.goneAsEvent = v
}

func (api *fakeAPI) setExec(h http.HandlerFunc) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.exec = h
}

func (api *fakeAPI) setLogs(h http.HandlerFunc) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.logs = h
}

func (api *fakeAPI) setOverride(f func(w http.ResponseWriter, r *http.Request) bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.override = f
}

type histEvent struct {
	rv     int64
	typ    EventType
	res    apiPath
	object map[string]any
}

type watcher struct {
	res       apiPath
	labels    string
	fields    string
	bookmarks bool
	events    chan histEvent
	stop      chan struct{}
}

// covers reports whether a watch on w's collection sees an event on res.
func (w *watcher) covers(res apiPath) bool {
	return w.res.group == res.group && w.res.version == res.version && w.res.resource == res.resource && (w.res.namespace == "" || w.res.namespace == res.namespace)
}

type recordedRequest struct {
	Method      string
	Proto       string
	Path        string
	Query       url.Values
	ContentType string
	Body        []byte
	Header      http.Header
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	return newFakeAPIWith(t, false)
}

// newFakeAPIWith starts the fake; http2 makes the server offer h2 over
// ALPN as the real API server does.
func newFakeAPIWith(t *testing.T, http2 bool) *fakeAPI {
	t.Helper()
	api := &fakeAPI{t: t, objects: map[string]map[string]any{}, watchers: map[*watcher]struct{}{}, oldest: 1}
	api.srv = httptest.NewUnstartedServer(http.HandlerFunc(api.serve))
	api.srv.EnableHTTP2 = http2
	api.srv.StartTLS()
	t.Cleanup(api.srv.Close)
	return api
}

// config is a Config that trusts the test server.
func (api *fakeAPI) config() *Config {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.srv.Certificate().Raw})
	api.mu.Lock()
	defer api.mu.Unlock()
	return &Config{Host: api.srv.URL, CAData: ca, Token: api.token, Namespace: "default"}
}

func (api *fakeAPI) client() *Client {
	c, err := NewClient(api.config())
	if err != nil {
		api.t.Fatal(err)
	}
	return c
}

// seed stores an object directly, without an event.
func (api *fakeAPI) seed(r Resource, namespace string, obj map[string]any) map[string]any {
	api.mu.Lock()
	defer api.mu.Unlock()
	name := metaString(obj, "name")
	api.stamp(obj, r, namespace, true)
	api.objects[r.path(namespace, name)] = obj
	return obj
}

// expireHistory forgets every event at or below the current version so a
// resume from any version seen so far is 410 Gone until a new list.
func (api *fakeAPI) expireHistory() {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.oldest = api.rv + 1
	api.history = nil
}

// closeWatchers ends every open watch as the server does on its schedule.
func (api *fakeAPI) closeWatchers() {
	api.mu.Lock()
	defer api.mu.Unlock()
	for w := range api.watchers {
		close(w.stop)
		delete(api.watchers, w)
	}
}

// bookmark sends a BOOKMARK at the current version to watchers that asked.
func (api *fakeAPI) bookmark() {
	api.mu.Lock()
	defer api.mu.Unlock()
	for w := range api.watchers {
		if w.bookmarks {
			w.events <- histEvent{rv: api.rv, typ: Bookmark, res: w.res, object: map[string]any{"metadata": map[string]any{"resourceVersion": strconv.FormatInt(api.rv, 10)}}}
		}
	}
}

func (api *fakeAPI) watcherCount() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return len(api.watchers)
}

func (api *fakeAPI) recorded() []recordedRequest {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]recordedRequest(nil), api.requests...)
}

func (api *fakeAPI) lastRequest() recordedRequest {
	reqs := api.recorded()
	if len(reqs) == 0 {
		api.t.Fatal("no requests recorded")
	}
	return reqs[len(reqs)-1]
}

func (api *fakeAPI) currentVersion() string {
	api.mu.Lock()
	defer api.mu.Unlock()
	return strconv.FormatInt(api.rv, 10)
}

func (api *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	api.mu.Lock()
	api.requests = append(api.requests, recordedRequest{Method: r.Method, Proto: r.Proto, Path: r.URL.Path, Query: r.URL.Query(), ContentType: r.Header.Get("Content-Type"), Body: body, Header: r.Header.Clone()})
	token, override, exec, logs := api.token, api.override, api.exec, api.logs
	api.mu.Unlock()
	if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
		writeStatus(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	if override != nil && override(w, r) {
		return
	}
	if r.URL.Path == "/version" {
		writeJSON(w, http.StatusOK, map[string]any{"major": "1", "minor": "34", "gitVersion": "v1.34.1+k3s1", "platform": "linux/arm64"})
		return
	}
	res, err := parseAPIPath(r.URL.Path)
	if err != nil {
		writeStatus(w, http.StatusNotFound, "NotFound", err.Error())
		return
	}
	switch res.subresource {
	case "exec":
		if exec == nil {
			writeStatus(w, http.StatusNotFound, "NotFound", "no exec handler")
			return
		}
		exec(w, r)
		return
	case "log":
		if logs == nil {
			writeStatus(w, http.StatusNotFound, "NotFound", "no log handler")
			return
		}
		logs(w, r)
		return
	case "":
	default:
		writeStatus(w, http.StatusNotFound, "NotFound", "unknown subresource "+res.subresource)
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("watch") == "true":
		api.serveWatch(w, r, res)
	case r.Method == http.MethodGet && res.name == "":
		api.serveList(w, res, q)
	case r.Method == http.MethodGet:
		api.serveGet(w, res)
	case r.Method == http.MethodPost:
		api.serveCreate(w, res, body)
	case r.Method == http.MethodPut:
		api.serveUpdate(w, res, body)
	case r.Method == http.MethodDelete:
		api.serveDelete(w, res, body)
	case r.Method == http.MethodPatch:
		api.servePatch(w, r, res, body)
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

type apiPath struct {
	group, version, namespace, resource, name, subresource string
}

func (p apiPath) objectPath() string {
	return Resource{Group: p.group, Version: p.version, Resource: p.resource}.path(p.namespace, p.name)
}

func parseAPIPath(path string) (apiPath, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var p apiPath
	switch {
	case len(parts) >= 2 && parts[0] == "api":
		p.version = parts[1]
		parts = parts[2:]
	case len(parts) >= 3 && parts[0] == "apis":
		p.group, p.version = parts[1], parts[2]
		parts = parts[3:]
	default:
		return p, fmt.Errorf("not an API path: %s", path)
	}
	if len(parts) >= 3 && parts[0] == "namespaces" {
		p.namespace = parts[1]
		parts = parts[2:]
	}
	if len(parts) == 0 {
		return p, fmt.Errorf("no resource in %s", path)
	}
	p.resource = parts[0]
	if len(parts) > 1 {
		p.name = parts[1]
	}
	if len(parts) > 2 {
		p.subresource = parts[2]
	}
	if len(parts) > 3 {
		return p, fmt.Errorf("path too deep: %s", path)
	}
	return p, nil
}

func kindOf(resource string) string {
	for _, r := range []Resource{Pods, PersistentVolumeClaims, Secrets, ConfigMaps, Namespaces, NetworkPolicies, RuntimeClasses, ValidatingAdmissionPolicies, ValidatingAdmissionPolicyBindings} {
		if r.Resource == resource {
			return r.Kind
		}
	}
	return strings.ToUpper(resource[:1]) + strings.TrimSuffix(resource[1:], "s")
}

// stamp fills the server-owned metadata. Caller holds the lock.
func (api *fakeAPI) stamp(obj map[string]any, r Resource, namespace string, create bool) {
	api.rv++
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["resourceVersion"] = strconv.FormatInt(api.rv, 10)
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if create {
		api.uid++
		meta["uid"] = fmt.Sprintf("uid-%d", api.uid)
		meta["creationTimestamp"] = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}
	if obj["apiVersion"] == nil {
		obj["apiVersion"] = r.APIVersion()
	}
	if obj["kind"] == nil {
		obj["kind"] = r.Kind
	}
}

// emit records and broadcasts an event. Caller holds the lock.
func (api *fakeAPI) emit(typ EventType, res apiPath, obj map[string]any) {
	ev := histEvent{rv: api.rv, typ: typ, res: res, object: obj}
	api.history = append(api.history, ev)
	for w := range api.watchers {
		if !w.covers(res) || !selectorsMatch(obj, w.labels, w.fields) {
			continue
		}
		select {
		case w.events <- ev:
		default:
			api.t.Errorf("watcher overflow")
		}
	}
}

func (api *fakeAPI) resourceOf(res apiPath) Resource {
	return Resource{Group: res.group, Version: res.version, Resource: res.resource, Kind: kindOf(res.resource)}
}

func (api *fakeAPI) serveGet(w http.ResponseWriter, res apiPath) {
	api.mu.Lock()
	obj, ok := api.objects[res.objectPath()]
	api.mu.Unlock()
	if !ok {
		writeNotFound(w, res)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (api *fakeAPI) serveList(w http.ResponseWriter, res apiPath, q url.Values) {
	api.mu.Lock()
	items := api.collect(res, q.Get("labelSelector"), q.Get("fieldSelector"))
	rv := api.rv
	api.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": api.resourceOf(res).APIVersion(), "kind": kindOf(res.resource) + "List",
		"metadata": map[string]any{"resourceVersion": strconv.FormatInt(rv, 10)},
		"items":    items,
	})
}

// collect returns the collection's objects, sorted by name. Caller holds
// the lock. An empty namespace on a namespaced resource spans namespaces.
func (api *fakeAPI) collect(res apiPath, labels, fields string) []map[string]any {
	var items []map[string]any
	for path, obj := range api.objects {
		p, err := parseAPIPath(path)
		if err != nil || p.group != res.group || p.version != res.version || p.resource != res.resource {
			continue
		}
		if res.namespace != "" && p.namespace != res.namespace {
			continue
		}
		if selectorsMatch(obj, labels, fields) {
			items = append(items, obj)
		}
	}
	sort.Slice(items, func(i, j int) bool { return metaString(items[i], "name") < metaString(items[j], "name") })
	if items == nil {
		items = []map[string]any{}
	}
	return items
}

func (api *fakeAPI) serveCreate(w http.ResponseWriter, res apiPath, body []byte) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	res.name = metaString(obj, "name")
	if res.name == "" {
		writeStatus(w, http.StatusUnprocessableEntity, "Invalid", "metadata.name is required")
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if _, exists := api.objects[res.objectPath()]; exists {
		writeStatus(w, http.StatusConflict, "AlreadyExists", fmt.Sprintf("%s %q already exists", res.resource, res.name))
		return
	}
	api.stamp(obj, api.resourceOf(res), res.namespace, true)
	api.objects[res.objectPath()] = obj
	api.emit(Added, res, obj)
	writeJSON(w, http.StatusCreated, obj)
}

func (api *fakeAPI) serveUpdate(w http.ResponseWriter, res apiPath, body []byte) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	current, ok := api.objects[res.objectPath()]
	if !ok {
		writeNotFound(w, res)
		return
	}
	if rv := metaString(obj, "resourceVersion"); rv != "" && rv != metaString(current, "resourceVersion") {
		writeStatus(w, http.StatusConflict, "Conflict", fmt.Sprintf("Operation cannot be fulfilled on %s %q: the object has been modified", res.resource, res.name))
		return
	}
	meta := obj["metadata"].(map[string]any)
	meta["uid"] = metaString(current, "uid")
	meta["creationTimestamp"] = current["metadata"].(map[string]any)["creationTimestamp"]
	api.stamp(obj, api.resourceOf(res), res.namespace, false)
	api.objects[res.objectPath()] = obj
	api.emit(Modified, res, obj)
	writeJSON(w, http.StatusOK, obj)
}

func (api *fakeAPI) serveDelete(w http.ResponseWriter, res apiPath, body []byte) {
	var opts struct {
		Preconditions struct {
			UID string `json:"uid"`
		} `json:"preconditions"`
	}
	_ = json.Unmarshal(body, &opts)
	api.mu.Lock()
	defer api.mu.Unlock()
	current, ok := api.objects[res.objectPath()]
	if !ok {
		writeNotFound(w, res)
		return
	}
	if opts.Preconditions.UID != "" && opts.Preconditions.UID != metaString(current, "uid") {
		writeStatus(w, http.StatusConflict, "Conflict", fmt.Sprintf("Precondition failed: UID in precondition: %s, UID in object meta: %s", opts.Preconditions.UID, metaString(current, "uid")))
		return
	}
	delete(api.objects, res.objectPath())
	api.rv++
	current["metadata"].(map[string]any)["resourceVersion"] = strconv.FormatInt(api.rv, 10)
	api.emit(Deleted, res, current)
	writeJSON(w, http.StatusOK, current)
}

func (api *fakeAPI) servePatch(w http.ResponseWriter, r *http.Request, res apiPath, body []byte) {
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	current, ok := api.objects[res.objectPath()]
	switch r.Header.Get("Content-Type") {
	case "application/merge-patch+json":
		if !ok {
			writeNotFound(w, res)
			return
		}
		merged := mergePatch(current, patch).(map[string]any)
		api.stamp(merged, api.resourceOf(res), res.namespace, false)
		api.objects[res.objectPath()] = merged
		api.emit(Modified, res, merged)
		writeJSON(w, http.StatusOK, merged)
	case "application/apply-patch+yaml":
		if r.URL.Query().Get("fieldManager") == "" {
			writeStatus(w, http.StatusBadRequest, "BadRequest", "fieldManager is required for apply")
			return
		}
		if !ok {
			api.stamp(patch, api.resourceOf(res), res.namespace, true)
			api.objects[res.objectPath()] = patch
			api.emit(Added, res, patch)
			writeJSON(w, http.StatusCreated, patch)
			return
		}
		merged := mergePatch(current, patch).(map[string]any)
		api.stamp(merged, api.resourceOf(res), res.namespace, false)
		api.objects[res.objectPath()] = merged
		api.emit(Modified, res, merged)
		writeJSON(w, http.StatusOK, merged)
	default:
		writeStatus(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", r.Header.Get("Content-Type"))
	}
}

func (api *fakeAPI) serveWatch(w http.ResponseWriter, r *http.Request, res apiPath) {
	q := r.URL.Query()
	wt := &watcher{res: res, labels: q.Get("labelSelector"), fields: q.Get("fieldSelector"), bookmarks: q.Get("allowWatchBookmarks") == "true", events: make(chan histEvent, 256), stop: make(chan struct{})}
	var replay []histEvent
	api.mu.Lock()
	since := int64(0)
	if rv := q.Get("resourceVersion"); rv != "" && rv != "0" {
		n, err := strconv.ParseInt(rv, 10, 64)
		if err != nil {
			api.mu.Unlock()
			writeStatus(w, http.StatusBadRequest, "BadRequest", "bad resourceVersion")
			return
		}
		if n < api.oldest {
			api.mu.Unlock()
			if api.goneAsEvent {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				status := map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Expired", "message": "too old resource version: " + rv, "code": 410}
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "ERROR", "object": status})
				return
			}
			writeStatus(w, http.StatusGone, "Expired", "too old resource version: "+rv)
			return
		}
		since = n
		for _, ev := range api.history {
			if ev.rv > since && wt.covers(ev.res) && selectorsMatch(ev.object, wt.labels, wt.fields) {
				replay = append(replay, ev)
			}
		}
	} else {
		for _, obj := range api.collect(res, wt.labels, wt.fields) {
			rv, _ := strconv.ParseInt(metaString(obj, "resourceVersion"), 10, 64)
			replay = append(replay, histEvent{rv: rv, typ: Added, res: res, object: obj})
		}
	}
	api.watchers[wt] = struct{}{}
	api.mu.Unlock()
	defer func() {
		api.mu.Lock()
		delete(api.watchers, wt)
		api.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush() // the API server sends the headers before the first event
	}
	write := func(ev histEvent) bool {
		if err := json.NewEncoder(w).Encode(map[string]any{"type": ev.typ, "object": ev.object}); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for _, ev := range replay {
		if !write(ev) {
			return
		}
	}
	for {
		select {
		case ev := <-wt.events:
			if !write(ev) {
				return
			}
		case <-wt.stop:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// selectorsMatch evaluates a label selector (k=v, k==v, k!=v, k, !k,
// comma-separated) and a field selector (dotted path = or !=).
func selectorsMatch(obj map[string]any, labels, fields string) bool {
	labelMap := map[string]string{}
	if meta, _ := obj["metadata"].(map[string]any); meta != nil {
		if raw, _ := meta["labels"].(map[string]any); raw != nil {
			for k, v := range raw {
				labelMap[k], _ = v.(string)
			}
		}
	}
	for _, term := range splitSelector(labels) {
		switch {
		case strings.Contains(term, "!="):
			kv := strings.SplitN(term, "!=", 2)
			if v, ok := labelMap[kv[0]]; ok && v == kv[1] {
				return false
			}
		case strings.Contains(term, "=="):
			kv := strings.SplitN(term, "==", 2)
			if labelMap[kv[0]] != kv[1] {
				return false
			}
		case strings.Contains(term, "="):
			kv := strings.SplitN(term, "=", 2)
			if v, ok := labelMap[kv[0]]; !ok || v != kv[1] {
				return false
			}
		case strings.HasPrefix(term, "!"):
			if _, ok := labelMap[term[1:]]; ok {
				return false
			}
		default:
			if _, ok := labelMap[term]; !ok {
				return false
			}
		}
	}
	for _, term := range splitSelector(fields) {
		negate := strings.Contains(term, "!=")
		kv := strings.SplitN(term, "!=", 2)
		if !negate {
			kv = strings.SplitN(term, "=", 2)
		}
		if len(kv) != 2 {
			return false
		}
		got := fieldValue(obj, kv[0])
		if (got == kv[1]) == negate {
			return false
		}
	}
	return true
}

func splitSelector(s string) []string {
	var terms []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			terms = append(terms, t)
		}
	}
	return terms
}

func fieldValue(obj map[string]any, path string) string {
	var cur any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[part]
	}
	switch v := cur.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func metaString(obj map[string]any, key string) string {
	meta, _ := obj["metadata"].(map[string]any)
	s, _ := meta[key].(string)
	return s
}

// mergePatch applies an RFC 7386 merge patch.
func mergePatch(target, patch any) any {
	patchMap, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	targetMap, ok := target.(map[string]any)
	if !ok {
		targetMap = map[string]any{}
	}
	out := make(map[string]any, len(targetMap))
	for k, v := range targetMap {
		out[k] = v
	}
	for k, v := range patchMap {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatch(out[k], v)
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, reason, message string) {
	writeJSON(w, code, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure", "message": message, "reason": reason, "code": code})
}

func writeNotFound(w http.ResponseWriter, res apiPath) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
		"message": fmt.Sprintf("%s %q not found", res.resource, res.name), "reason": "NotFound",
		"details": map[string]any{"name": res.name, "kind": res.resource}, "code": 404,
	})
}

// testContext is a context that ends with the test.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// nextEvent receives one event or fails the test.
func nextEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return Event{}
}

// expectClosed asserts the channel closes without further events.
func expectClosed(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event %s %v", ev.Type, ev.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("channel did not close")
	}
}

// podObject is a minimal pod as a map.
func podObject(name string, labels map[string]string) map[string]any {
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	return map[string]any{"metadata": map[string]any{"name": name, "labels": l}, "spec": map[string]any{"containers": []any{map[string]any{"name": "guest", "image": "ghcr.io/example/guest@sha256:abc"}}}}
}
