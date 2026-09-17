package kube

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	api "warden/chat/internal/kube"
)

// fakeAPI is enough of the Kubernetes API for this package's tests: the
// verbs over an in-memory store with resource versions, label and field
// selectors, merge patches, watch with resume, pods/log, and hooks to
// script what happens to a created object (a canary pod ending with its
// log) and to fail requests (a conflict, a forbidden verb).
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	rv       int64
	uid      int64
	objects  map[string]map[string]any
	history  []histEvent
	watchers map[*watcher]struct{}
	requests []recordedRequest
	logs     map[string]string // pod path -> log text

	// onCreate runs after an object is stored (under mu); it may mutate
	// the object (a pod's status) and set its log.
	onCreate func(res apiPath, obj map[string]any)
	// before may answer a request itself (returning true) to inject
	// failures; it runs without mu.
	before func(w http.ResponseWriter, r *http.Request) bool
}

type histEvent struct {
	rv     int64
	typ    api.EventType
	res    apiPath
	object map[string]any
}

type watcher struct {
	res    apiPath
	labels string
	fields string
	events chan histEvent
}

func (w *watcher) covers(res apiPath) bool {
	return w.res.group == res.group && w.res.version == res.version && w.res.resource == res.resource && (w.res.namespace == "" || w.res.namespace == res.namespace)
}

type recordedRequest struct {
	Method      string
	Path        string
	Query       url.Values
	ContentType string
	Body        []byte
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, objects: map[string]map[string]any{}, watchers: map[*watcher]struct{}{}, logs: map[string]string{}}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0) // handshake noise from watches ending with the test
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

// client is a Client that trusts the fake, with "warden" as its own
// namespace (the core namespace).
func (f *fakeAPI) client() *api.Client {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	c, err := api.NewClient(&api.Config{Host: f.srv.URL, CAData: ca, Token: "test-token", Namespace: "warden"})
	if err != nil {
		f.t.Fatal(err)
	}
	c.Timeout = 10 * time.Second
	return c
}

// seed stores an object without an event.
func (f *fakeAPI) seed(r api.Resource, namespace string, obj any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := toMap(f.t, obj)
	name := metaString(m, "name")
	f.stamp(m, r, namespace, true)
	f.objects[resourcePath(r, namespace, name)] = m
}

// object returns a stored object decoded into into, or false.
func (f *fakeAPI) object(r api.Resource, namespace, name string, into any) bool {
	f.mu.Lock()
	obj, ok := f.objects[resourcePath(r, namespace, name)]
	f.mu.Unlock()
	if !ok {
		return false
	}
	raw, _ := json.Marshal(obj)
	if err := json.Unmarshal(raw, into); err != nil {
		f.t.Fatal(err)
	}
	return true
}

// remove deletes an object with an event, as the API would.
func (f *fakeAPI) remove(r api.Resource, namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := resourcePath(r, namespace, name)
	obj, ok := f.objects[path]
	if !ok {
		return
	}
	delete(f.objects, path)
	f.rv++
	obj["metadata"].(map[string]any)["resourceVersion"] = strconv.FormatInt(f.rv, 10)
	f.emit(api.Deleted, pathOf(f.t, path), obj)
}

// put replaces an object with an event (a Modified), as an update would.
func (f *fakeAPI) put(r api.Resource, namespace string, obj any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := toMap(f.t, obj)
	name := metaString(m, "name")
	path := resourcePath(r, namespace, name)
	current, ok := f.objects[path]
	if !ok {
		f.stamp(m, r, namespace, true)
		f.objects[path] = m
		f.emit(api.Added, pathOf(f.t, path), m)
		return
	}
	meta := m["metadata"].(map[string]any)
	meta["uid"] = metaString(current, "uid")
	f.stamp(m, r, namespace, false)
	f.objects[path] = m
	f.emit(api.Modified, pathOf(f.t, path), m)
}

func (f *fakeAPI) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *fakeAPI) count(method, pathSuffix string) int {
	n := 0
	for _, r := range f.recorded() {
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			n++
		}
	}
	return n
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), ContentType: r.Header.Get("Content-Type"), Body: body})
	before := f.before
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		writeStatus(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	if before != nil && before(w, r) {
		return
	}
	res, err := parseAPIPath(r.URL.Path)
	if err != nil {
		writeStatus(w, http.StatusNotFound, "NotFound", err.Error())
		return
	}
	if res.subresource == "log" {
		f.serveLog(w, res)
		return
	}
	if res.subresource != "" {
		writeStatus(w, http.StatusNotFound, "NotFound", "unknown subresource "+res.subresource)
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("watch") == "true":
		f.serveWatch(w, r, res)
	case r.Method == http.MethodGet && res.name == "":
		f.serveList(w, res, q)
	case r.Method == http.MethodGet:
		f.serveGet(w, res)
	case r.Method == http.MethodPost:
		f.serveCreate(w, res, body)
	case r.Method == http.MethodPut:
		f.serveUpdate(w, res, body)
	case r.Method == http.MethodDelete:
		f.serveDelete(w, res)
	case r.Method == http.MethodPatch:
		f.servePatch(w, r, res, body)
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

type apiPath struct {
	group, version, namespace, resource, name, subresource string
}

func (p apiPath) objectPath() string {
	return resourcePath(api.Resource{Group: p.group, Version: p.version, Resource: p.resource}, p.namespace, p.name)
}

// resourcePath is the URL path of a collection (name empty) or an object,
// as the client builds it.
func resourcePath(r api.Resource, namespace, name string) string {
	var b strings.Builder
	if r.Group == "" {
		b.WriteString("/api/")
	} else {
		b.WriteString("/apis/" + r.Group + "/")
	}
	b.WriteString(r.Version)
	if namespace != "" {
		b.WriteString("/namespaces/" + url.PathEscape(namespace))
	}
	b.WriteString("/" + r.Resource)
	if name != "" {
		b.WriteString("/" + url.PathEscape(name))
	}
	return b.String()
}

func pathOf(t *testing.T, path string) apiPath {
	p, err := parseAPIPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
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
	return p, nil
}

func kindOf(resource string) string {
	for _, r := range []api.Resource{api.Pods, api.PersistentVolumeClaims, api.Secrets, api.ConfigMaps, api.Namespaces, api.NetworkPolicies, api.RuntimeClasses, api.ValidatingAdmissionPolicies, api.ValidatingAdmissionPolicyBindings} {
		if r.Resource == resource {
			return r.Kind
		}
	}
	return strings.ToUpper(resource[:1]) + strings.TrimSuffix(resource[1:], "s")
}

func (f *fakeAPI) resourceOf(res apiPath) api.Resource {
	return api.Resource{Group: res.group, Version: res.version, Resource: res.resource, Kind: kindOf(res.resource)}
}

// stamp fills server-owned metadata. Caller holds mu.
func (f *fakeAPI) stamp(obj map[string]any, r api.Resource, namespace string, create bool) {
	f.rv++
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["resourceVersion"] = strconv.FormatInt(f.rv, 10)
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if create {
		f.uid++
		meta["uid"] = fmt.Sprintf("uid-%d", f.uid)
		meta["creationTimestamp"] = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}
	if obj["apiVersion"] == nil {
		obj["apiVersion"] = r.APIVersion()
	}
	if obj["kind"] == nil {
		obj["kind"] = r.Kind
	}
}

// emit records and broadcasts an event. Caller holds mu.
func (f *fakeAPI) emit(typ api.EventType, res apiPath, obj map[string]any) {
	// Watchers serialise the event on their own goroutine while the caller
	// may keep mutating the stored object, so the event carries a copy.
	raw, err := json.Marshal(obj)
	if err != nil {
		f.t.Errorf("emit: %v", err)
		return
	}
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		f.t.Errorf("emit: %v", err)
		return
	}
	ev := histEvent{rv: f.rv, typ: typ, res: res, object: snapshot}
	f.history = append(f.history, ev)
	for w := range f.watchers {
		if w.covers(res) && selectorsMatch(obj, w.labels, w.fields) {
			select {
			case w.events <- ev:
			default:
				f.t.Errorf("watcher overflow")
			}
		}
	}
}

func (f *fakeAPI) serveGet(w http.ResponseWriter, res apiPath) {
	f.mu.Lock()
	obj, ok := f.objects[res.objectPath()]
	f.mu.Unlock()
	if !ok {
		writeNotFound(w, res)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (f *fakeAPI) serveList(w http.ResponseWriter, res apiPath, q url.Values) {
	f.mu.Lock()
	items := f.collect(res, q.Get("labelSelector"), q.Get("fieldSelector"))
	rv := f.rv
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": f.resourceOf(res).APIVersion(), "kind": kindOf(res.resource) + "List",
		"metadata": map[string]any{"resourceVersion": strconv.FormatInt(rv, 10)},
		"items":    items,
	})
}

// collect returns the collection's objects sorted by name. Caller holds mu.
func (f *fakeAPI) collect(res apiPath, labels, fields string) []map[string]any {
	items := []map[string]any{}
	for path, obj := range f.objects {
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
	return items
}

func (f *fakeAPI) serveCreate(w http.ResponseWriter, res apiPath, body []byte) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	res.name = metaString(obj, "name")
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.objects[res.objectPath()]; exists {
		writeStatus(w, http.StatusConflict, "AlreadyExists", fmt.Sprintf("%s %q already exists", res.resource, res.name))
		return
	}
	f.stamp(obj, f.resourceOf(res), res.namespace, true)
	f.objects[res.objectPath()] = obj
	f.emit(api.Added, res, obj)
	if f.onCreate != nil {
		f.onCreate(res, obj)
	}
	writeJSON(w, http.StatusCreated, obj)
}

func (f *fakeAPI) serveUpdate(w http.ResponseWriter, res apiPath, body []byte) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.objects[res.objectPath()]
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
	f.stamp(obj, f.resourceOf(res), res.namespace, false)
	f.objects[res.objectPath()] = obj
	f.emit(api.Modified, res, obj)
	writeJSON(w, http.StatusOK, obj)
}

func (f *fakeAPI) serveDelete(w http.ResponseWriter, res apiPath) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.objects[res.objectPath()]
	if !ok {
		writeNotFound(w, res)
		return
	}
	delete(f.objects, res.objectPath())
	f.rv++
	current["metadata"].(map[string]any)["resourceVersion"] = strconv.FormatInt(f.rv, 10)
	f.emit(api.Deleted, res, current)
	writeJSON(w, http.StatusOK, current)
}

func (f *fakeAPI) servePatch(w http.ResponseWriter, r *http.Request, res apiPath, body []byte) {
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.objects[res.objectPath()]
	switch r.Header.Get("Content-Type") {
	case "application/merge-patch+json":
		if !ok {
			writeNotFound(w, res)
			return
		}
	case "application/apply-patch+yaml":
		if !ok {
			f.stamp(patch, f.resourceOf(res), res.namespace, true)
			f.objects[res.objectPath()] = patch
			f.emit(api.Added, res, patch)
			writeJSON(w, http.StatusCreated, patch)
			return
		}
	default:
		writeStatus(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", r.Header.Get("Content-Type"))
		return
	}
	merged := mergePatch(current, patch).(map[string]any)
	f.stamp(merged, f.resourceOf(res), res.namespace, false)
	f.objects[res.objectPath()] = merged
	f.emit(api.Modified, res, merged)
	writeJSON(w, http.StatusOK, merged)
}

func (f *fakeAPI) serveLog(w http.ResponseWriter, res apiPath) {
	f.mu.Lock()
	text, ok := f.logs[res.objectPath()]
	_, exists := f.objects[res.objectPath()]
	f.mu.Unlock()
	if !exists {
		writeNotFound(w, res)
		return
	}
	if !ok {
		text = ""
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, text)
}

func (f *fakeAPI) serveWatch(w http.ResponseWriter, r *http.Request, res apiPath) {
	q := r.URL.Query()
	wt := &watcher{res: res, labels: q.Get("labelSelector"), fields: q.Get("fieldSelector"), events: make(chan histEvent, 256)}
	var replay []histEvent
	f.mu.Lock()
	if rv := q.Get("resourceVersion"); rv != "" && rv != "0" {
		since, err := strconv.ParseInt(rv, 10, 64)
		if err != nil {
			f.mu.Unlock()
			writeStatus(w, http.StatusBadRequest, "BadRequest", "bad resourceVersion")
			return
		}
		for _, ev := range f.history {
			if ev.rv > since && wt.covers(ev.res) && selectorsMatch(ev.object, wt.labels, wt.fields) {
				replay = append(replay, ev)
			}
		}
	} else {
		for _, obj := range f.collect(res, wt.labels, wt.fields) {
			rv, _ := strconv.ParseInt(metaString(obj, "resourceVersion"), 10, 64)
			replay = append(replay, histEvent{rv: rv, typ: api.Added, res: res, object: obj})
		}
	}
	f.watchers[wt] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.watchers, wt)
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
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
		case <-r.Context().Done():
			return
		}
	}
}

// selectorsMatch evaluates a label selector (k=v, k!=v, k, !k, comma
// separated) and a field selector (dotted path = or !=).
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
		case strings.Contains(term, "="):
			kv := strings.SplitN(strings.Replace(term, "==", "=", 1), "=", 2)
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
		if (fieldValue(obj, kv[0]) == kv[1]) == negate {
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

func toMap(t *testing.T, obj any) map[string]any {
	t.Helper()
	if m, ok := obj.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
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
