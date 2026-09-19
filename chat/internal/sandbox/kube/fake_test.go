package kube

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"warden/chat/internal/kube"
)

// fakeAPI is the slice of the Kubernetes API the driver uses, in memory:
// pods, claims and ConfigMaps in one namespace with resource versions,
// label and field selectors, list, watch (current state then changes) and
// pods/exec over a WebSocket served by a scripted guest. Pods start on
// their own after startDelay unless a test says otherwise, and a deleted
// pod lingers with a deletionTimestamp for deleteDelay as a real one does.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	rv       int64
	uid      int64
	ip       int
	objects  map[string]map[string]any // "<resource>/<name>" -> object
	watchers map[*fakeWatcher]struct{}
	requests []fakeRequest

	startDelay  time.Duration
	deleteDelay time.Duration
	// holdStart keeps created pods Pending (a scheduling or volume wait);
	// failStart makes their container wait with this reason.
	holdStart bool
	failStart string
	// forbidden lists "<verb> <resource>" pairs the server refuses with 403.
	forbidden map[string]bool
	// refuseClone refuses a claim created with a dataSource.
	refuseClone bool
	// resizeMode says what pods/resize does: "" applies the patch and, after
	// startDelay, reports the container running at the new size as a
	// kubelet does; "infeasible" accepts the patch and reports the
	// PodResizePending Infeasible condition; "deferred" accepts it and
	// reports Deferred for good (no room on the node); "absent" is a
	// server without the subresource (404).
	resizeMode string
	// nodes, nodeMetrics and podMetrics are the cluster view (cluster_test.go):
	// nodes under /api/v1/nodes, usage under metrics.k8s.io when metrics is
	// set (otherwise that group is not served, as without a metrics server);
	// logs are what pods/log answers per pod, one line each.
	nodes       []map[string]any
	metrics     bool
	nodeMetrics map[string]map[string]string
	podMetrics  map[string]map[string]string
	logs        map[string][]string

	guest *fakeGuest
}

type fakeRequest struct {
	Method, Path string
	Query        url.Values
	Body         []byte
}

type fakeWatcher struct {
	resource string
	labels   string
	fields   string
	since    int64
	events   chan fakeEvent
}

type fakeEvent struct {
	rv     int64
	typ    kube.EventType
	object map[string]any
}

const testNamespace = "warden-sandboxes"

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{t: t, objects: map[string]map[string]any{}, watchers: map[*fakeWatcher]struct{}{}, startDelay: 20 * time.Millisecond, deleteDelay: 20 * time.Millisecond, forbidden: map[string]bool{}}
	api.guest = newFakeGuest(api)
	api.srv = httptest.NewUnstartedServer(http.HandlerFunc(api.serve))
	api.srv.StartTLS()
	t.Cleanup(api.srv.Close)
	return api
}

func (api *fakeAPI) client() *kube.Client {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.srv.Certificate().Raw})
	c, err := kube.NewClient(&kube.Config{Host: api.srv.URL, CAData: ca, Token: "runner-token", Namespace: testNamespace})
	if err != nil {
		api.t.Fatal(err)
	}
	c.Timeout = 5 * time.Second
	return c
}

// options are driver options for the fake.
func testOptions() Options {
	return Options{Namespace: testNamespace, Tier: "gvisor", RuntimeClass: "gvisor", GuestImage: "ghcr.io/monaddle-too/warden-guest-base", GuestImageDigest: "sha256:" + strings.Repeat("ab", 32), StorageClass: "local-path", WorkspaceSizeGi: 4, TrustConfigMap: "warden-guest-trust", MemoryMB: 1024}
}

func newTestDriver(t *testing.T, api *fakeAPI, opts Options) *Driver {
	t.Helper()
	d, err := New(api.client(), opts)
	if err != nil {
		t.Fatal(err)
	}
	d.execRetry = 10 * time.Millisecond
	d.resizeWait = 300 * time.Millisecond
	return d
}

// publishTrust stores a trust ConfigMap with a bundle (or without one).
func (api *fakeAPI) publishTrust(bundle string) {
	data := map[string]any{}
	if bundle != "" {
		data[TrustBundleKey] = bundle
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	key := "configmaps/warden-guest-trust"
	obj := api.objects[key]
	created := obj == nil
	if created {
		obj = map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "warden-guest-trust"}}
	}
	obj["data"] = data
	api.stamp(obj, "configmaps", created)
	api.objects[key] = obj
	if created {
		api.emit(kube.Added, "configmaps", obj)
	} else {
		api.emit(kube.Modified, "configmaps", obj)
	}
}

func (api *fakeAPI) object(resource, name string, into any) bool {
	api.mu.Lock()
	obj, ok := api.objects[resource+"/"+name]
	var raw []byte
	if ok {
		raw, _ = json.Marshal(obj)
	}
	api.mu.Unlock()
	if !ok {
		return false
	}
	if err := json.Unmarshal(raw, into); err != nil {
		api.t.Fatal(err)
	}
	return true
}

func (api *fakeAPI) pod(name string) (kube.Pod, bool) {
	var pod kube.Pod
	ok := api.object("pods", name, &pod)
	return pod, ok
}

func (api *fakeAPI) claim(name string) (kube.PersistentVolumeClaim, bool) {
	var claim kube.PersistentVolumeClaim
	ok := api.object("persistentvolumeclaims", name, &claim)
	return claim, ok
}

func (api *fakeAPI) names(resource string) []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	var out []string
	for key := range api.objects {
		if strings.HasPrefix(key, resource+"/") {
			out = append(out, strings.TrimPrefix(key, resource+"/"))
		}
	}
	sort.Strings(out)
	return out
}

func (api *fakeAPI) recorded() []fakeRequest {
	api.mu.Lock()
	defer api.mu.Unlock()
	return append([]fakeRequest(nil), api.requests...)
}

// stamp fills server metadata; caller holds the lock.
func (api *fakeAPI) stamp(obj map[string]any, resource string, create bool) {
	api.rv++
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["resourceVersion"] = strconv.FormatInt(api.rv, 10)
	meta["namespace"] = testNamespace
	if create {
		api.uid++
		meta["uid"] = fmt.Sprintf("uid-%s-%d", resource, api.uid)
	}
}

// emit records an event for the watchers; caller holds the lock.
func (api *fakeAPI) emit(typ kube.EventType, resource string, obj map[string]any) {
	ev := fakeEvent{rv: api.rv, typ: typ, object: cloneObject(obj)}
	for w := range api.watchers {
		if w.resource == resource && selectorsMatch(obj, w.labels, w.fields) {
			select {
			case w.events <- ev:
			default:
				api.t.Errorf("watcher overflow")
			}
		}
	}
}

func cloneObject(obj map[string]any) map[string]any {
	raw, _ := json.Marshal(obj)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (api *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	api.mu.Lock()
	api.requests = append(api.requests, fakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body})
	forbidden := api.forbidden
	api.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer runner-token" {
		writeStatus(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if api.serveCluster(w, r, parts) {
		return
	}
	// api/v1/namespaces/<ns>/<resource>[/<name>[/<sub>]]
	if len(parts) < 5 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "namespaces" || parts[3] != testNamespace {
		writeStatus(w, http.StatusNotFound, "NotFound", "unknown path "+r.URL.Path)
		return
	}
	resource := parts[4]
	name, sub := "", ""
	if len(parts) > 5 {
		name = parts[5]
	}
	if len(parts) > 6 {
		sub = parts[6]
	}
	q := r.URL.Query()
	verb := ""
	switch {
	case sub == "resize" && r.Method == http.MethodPatch:
		verb = "resize"
	case sub == "exec":
		verb = "exec"
	case sub == "log":
		verb = "log"
	case r.Method == http.MethodGet && q.Get("watch") == "true":
		verb = "watch"
	case r.Method == http.MethodGet && name == "":
		verb = "list"
	case r.Method == http.MethodGet:
		verb = "get"
	case r.Method == http.MethodPost:
		verb = "create"
	case r.Method == http.MethodDelete:
		verb = "delete"
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
		return
	}
	if forbidden[verb+" "+resource] {
		writeStatus(w, http.StatusForbidden, "Forbidden", fmt.Sprintf("%s is forbidden: User \"system:serviceaccount:warden:warden-runner\" cannot %s resource %q", resource, verb, resource))
		return
	}
	switch verb {
	case "resize":
		api.serveResize(w, r, name, body)
	case "exec":
		api.serveExec(w, r, name)
	case "log":
		api.serveLog(w, name, q)
	case "watch":
		api.serveWatch(w, r, resource)
	case "list":
		api.serveList(w, resource, q)
	case "get":
		api.serveGet(w, resource, name)
	case "create":
		api.serveCreate(w, resource, body)
	case "delete":
		api.serveDelete(w, resource, name, body)
	}
}

// serveCluster answers the cluster-wide and metrics paths the cluster view
// reads: /version, /api/v1/nodes and metrics.k8s.io. It reports whether
// it handled the request.
func (api *fakeAPI) serveCluster(w http.ResponseWriter, r *http.Request, parts []string) bool {
	path := strings.Join(parts, "/")
	api.mu.Lock()
	defer api.mu.Unlock()
	switch {
	case path == "version":
		writeJSON(w, http.StatusOK, map[string]any{"major": "1", "minor": "36", "gitVersion": "v1.36.4-fake"})
	case path == "api/v1/nodes":
		if api.forbidden["list nodes"] {
			writeStatus(w, http.StatusForbidden, "Forbidden", "nodes is forbidden")
			return true
		}
		items := api.nodes
		if items == nil {
			items = []map[string]any{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "NodeList", "items": items})
	case strings.HasPrefix(path, "apis/metrics.k8s.io/v1beta1/"):
		if !api.metrics {
			writeStatus(w, http.StatusNotFound, "NotFound", "the server could not find the requested resource")
			return true
		}
		rest := strings.TrimPrefix(path, "apis/metrics.k8s.io/v1beta1/")
		switch {
		case rest == "nodes":
			var items []map[string]any
			for name, usage := range api.nodeMetrics {
				items = append(items, map[string]any{"metadata": map[string]any{"name": name}, "usage": usage})
			}
			writeJSON(w, http.StatusOK, map[string]any{"kind": "NodeMetricsList", "items": items})
		case rest == "namespaces/"+testNamespace+"/pods":
			var items []map[string]any
			for name, usage := range api.podMetrics {
				items = append(items, map[string]any{"metadata": map[string]any{"name": name}, "containers": []any{map[string]any{"name": ContainerName, "usage": usage}}})
			}
			writeJSON(w, http.StatusOK, map[string]any{"kind": "PodMetricsList", "items": items})
		case strings.HasPrefix(rest, "namespaces/"+testNamespace+"/pods/"):
			name := strings.TrimPrefix(rest, "namespaces/"+testNamespace+"/pods/")
			usage, ok := api.podMetrics[name]
			if !ok {
				writeNotFound(w, "pods.metrics.k8s.io", name)
				return true
			}
			writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": name}, "containers": []any{map[string]any{"name": ContainerName, "usage": usage}}})
		default:
			writeStatus(w, http.StatusNotFound, "NotFound", "unknown path "+path)
		}
	default:
		return false
	}
	return true
}

// serveLog answers pods/log with the pod's scripted lines, the last
// tailLines of them, each stamped when timestamps is asked for.
func (api *fakeAPI) serveLog(w http.ResponseWriter, name string, q url.Values) {
	api.mu.Lock()
	_, exists := api.objects["pods/"+name]
	lines := append([]string(nil), api.logs[name]...)
	api.mu.Unlock()
	if !exists {
		writeNotFound(w, "pods", name)
		return
	}
	if q.Get("previous") == "true" {
		lines = []string{"previous instance"}
	}
	if n, err := strconv.Atoi(q.Get("tailLines")); err == nil && n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	w.Header().Set("Content-Type", "text/plain")
	for _, line := range lines {
		if q.Get("timestamps") == "true" {
			line = "2026-09-17T00:00:00Z " + line
		}
		fmt.Fprintln(w, line)
	}
}

func (api *fakeAPI) serveGet(w http.ResponseWriter, resource, name string) {
	api.mu.Lock()
	obj, ok := api.objects[resource+"/"+name]
	if ok {
		obj = cloneObject(obj)
	}
	api.mu.Unlock()
	if !ok {
		writeNotFound(w, resource, name)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (api *fakeAPI) serveList(w http.ResponseWriter, resource string, q url.Values) {
	api.mu.Lock()
	var items []map[string]any
	for key, obj := range api.objects {
		if strings.HasPrefix(key, resource+"/") && selectorsMatch(obj, q.Get("labelSelector"), q.Get("fieldSelector")) {
			items = append(items, cloneObject(obj))
		}
	}
	rv := api.rv
	api.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return metaString(items[i], "name") < metaString(items[j], "name") })
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"apiVersion": "v1", "kind": "List", "metadata": map[string]any{"resourceVersion": strconv.FormatInt(rv, 10)}, "items": items})
}

func (api *fakeAPI) serveCreate(w http.ResponseWriter, resource string, body []byte) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	name := metaString(obj, "name")
	api.mu.Lock()
	defer api.mu.Unlock()
	if _, exists := api.objects[resource+"/"+name]; exists {
		writeStatus(w, http.StatusConflict, "AlreadyExists", fmt.Sprintf("%s %q already exists", resource, name))
		return
	}
	if resource == "persistentvolumeclaims" && api.refuseClone {
		if spec, _ := obj["spec"].(map[string]any); spec != nil && spec["dataSource"] != nil {
			writeStatus(w, http.StatusUnprocessableEntity, "Invalid", "PersistentVolumeClaim is invalid: spec.dataSource: Forbidden: the storage class does not support cloning")
			return
		}
	}
	api.stamp(obj, resource, true)
	if resource == "pods" {
		// The workspace claim binds when its first pod is created, as a
		// WaitForFirstConsumer class does (the disk lands where the pod
		// schedules); a claim without a pod stays Pending.
		api.bindClaimOf(obj)
		obj["status"] = map[string]any{"phase": "Pending"}
		if api.failStart != "" {
			obj["status"] = map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{"name": ContainerName, "state": map[string]any{"waiting": map[string]any{"reason": api.failStart, "message": "image not present"}}}}}
		} else if !api.holdStart {
			api.ip++
			go api.startPod(name, "10.42.0."+strconv.Itoa(api.ip))
		}
	}
	if resource == "persistentvolumeclaims" {
		obj["status"] = map[string]any{"phase": "Pending"}
	}
	api.objects[resource+"/"+name] = obj
	api.emit(kube.Added, resource, obj)
	writeJSON(w, http.StatusCreated, obj)
}

// bindClaimOf binds the claims a pod mounts: phase Bound and a volume
// name, as the provisioner does once the pod is scheduled. Called with
// api.mu held.
func (api *fakeAPI) bindClaimOf(pod map[string]any) {
	spec, _ := pod["spec"].(map[string]any)
	volumes, _ := spec["volumes"].([]any)
	for _, v := range volumes {
		vol, _ := v.(map[string]any)
		pvc, _ := vol["persistentVolumeClaim"].(map[string]any)
		name, _ := pvc["claimName"].(string)
		claim, ok := api.objects["persistentvolumeclaims/"+name]
		if !ok {
			continue
		}
		claimSpec, _ := claim["spec"].(map[string]any)
		if claimSpec == nil {
			claimSpec = map[string]any{}
			claim["spec"] = claimSpec
		}
		claimSpec["volumeName"] = "pv-" + name
		claim["status"] = map[string]any{"phase": "Bound"}
		api.emit(kube.Modified, "persistentvolumeclaims", claim)
	}
}

// startPod moves a pod to Running with an address after startDelay.
func (api *fakeAPI) startPod(name, ip string) {
	time.Sleep(api.startDelay)
	api.mu.Lock()
	defer api.mu.Unlock()
	obj, ok := api.objects["pods/"+name]
	if !ok {
		return
	}
	// The kubelet reports the size the container runs at (1.33+).
	status := map[string]any{"name": ContainerName, "ready": true, "imageID": "ghcr.io/monaddle-too/warden-guest-base@sha256:" + strings.Repeat("ab", 32), "state": map[string]any{"running": map[string]any{}}}
	if resources := containerResources(obj); resources != nil {
		status["resources"] = cloneObject(resources)
	}
	obj["status"] = map[string]any{"phase": "Running", "podIP": ip, "podIPs": []any{map[string]any{"ip": ip}}, "containerStatuses": []any{status}}
	api.stamp(obj, "pods", false)
	api.emit(kube.Modified, "pods", obj)
}

// containerResources is the guest container's resources in a pod object.
func containerResources(pod map[string]any) map[string]any {
	spec, _ := pod["spec"].(map[string]any)
	containers, _ := spec["containers"].([]any)
	for _, c := range containers {
		container, _ := c.(map[string]any)
		if container["name"] == ContainerName {
			resources, _ := container["resources"].(map[string]any)
			return resources
		}
	}
	return nil
}

// serveResize is pods/resize under resizeMode: the strategic patch is
// applied to the guest container's resources (only what the kubelet's
// resize path needs is modelled), and the status follows as the mode
// says. The patch must be a strategic one, as kubectl sends.
func (api *fakeAPI) serveResize(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	if r.Header.Get("Content-Type") != "application/strategic-merge-patch+json" {
		writeStatus(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", r.Header.Get("Content-Type"))
		return
	}
	api.mu.Lock()
	mode := api.resizeMode
	obj, ok := api.objects["pods/"+name]
	if !ok || mode == "absent" {
		api.mu.Unlock()
		writeNotFound(w, "pods", name)
		return
	}
	var patch struct {
		Spec struct {
			Containers []struct {
				Name      string         `json:"name"`
				Resources map[string]any `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &patch); err != nil || len(patch.Spec.Containers) != 1 || patch.Spec.Containers[0].Name != ContainerName {
		api.mu.Unlock()
		writeStatus(w, http.StatusUnprocessableEntity, "Invalid", "resize patch must name the guest container's resources")
		return
	}
	spec := obj["spec"].(map[string]any)
	for _, c := range spec["containers"].([]any) {
		container := c.(map[string]any)
		if container["name"] == ContainerName {
			container["resources"] = patch.Spec.Containers[0].Resources
		}
	}
	status, _ := obj["status"].(map[string]any)
	if status != nil {
		if mode == "infeasible" {
			status["conditions"] = []any{map[string]any{"type": "PodResizePending", "status": "True", "reason": "Infeasible", "message": "Node didn't have enough capacity: cpu, requested: 4000, capacity: 2000"}}
		} else if mode == "deferred" {
			status["conditions"] = []any{map[string]any{"type": "PodResizePending", "status": "True", "reason": "Deferred", "message": "Node didn't have enough resource: memory"}}
		} else {
			status["conditions"] = []any{map[string]any{"type": "PodResizeInProgress", "status": "True"}}
		}
	}
	api.stamp(obj, "pods", false)
	api.emit(kube.Modified, "pods", obj)
	response := cloneObject(obj)
	api.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
	if mode == "" && status != nil {
		go func() {
			time.Sleep(api.startDelay)
			api.mu.Lock()
			defer api.mu.Unlock()
			obj, ok := api.objects["pods/"+name]
			if !ok {
				return
			}
			status := obj["status"].(map[string]any)
			delete(status, "conditions")
			for _, c := range status["containerStatuses"].([]any) {
				cs := c.(map[string]any)
				if cs["name"] == ContainerName {
					cs["resources"] = cloneObject(containerResources(obj))
				}
			}
			api.stamp(obj, "pods", false)
			api.emit(kube.Modified, "pods", obj)
		}()
	}
}

func (api *fakeAPI) serveDelete(w http.ResponseWriter, resource, name string, body []byte) {
	var opts struct {
		GracePeriodSeconds *int64 `json:"gracePeriodSeconds"`
		Preconditions      struct {
			UID string `json:"uid"`
		} `json:"preconditions"`
	}
	_ = json.Unmarshal(body, &opts)
	api.mu.Lock()
	defer api.mu.Unlock()
	obj, ok := api.objects[resource+"/"+name]
	if !ok {
		writeNotFound(w, resource, name)
		return
	}
	if opts.Preconditions.UID != "" && opts.Preconditions.UID != metaString(obj, "uid") {
		writeStatus(w, http.StatusConflict, "Conflict", "Precondition failed: UID in precondition: "+opts.Preconditions.UID)
		return
	}
	if resource == "pods" && api.deleteDelay > 0 {
		meta := obj["metadata"].(map[string]any)
		if meta["deletionTimestamp"] == nil {
			meta["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			api.stamp(obj, resource, false)
			api.emit(kube.Modified, resource, obj)
			go func() {
				time.Sleep(api.deleteDelay)
				api.mu.Lock()
				defer api.mu.Unlock()
				if current, ok := api.objects[resource+"/"+name]; ok && metaString(current, "uid") == metaString(obj, "uid") {
					delete(api.objects, resource+"/"+name)
					api.rv++
					current["metadata"].(map[string]any)["resourceVersion"] = strconv.FormatInt(api.rv, 10)
					api.emit(kube.Deleted, resource, current)
				}
			}()
		}
		writeJSON(w, http.StatusOK, obj)
		return
	}
	delete(api.objects, resource+"/"+name)
	api.rv++
	obj["metadata"].(map[string]any)["resourceVersion"] = strconv.FormatInt(api.rv, 10)
	api.emit(kube.Deleted, resource, obj)
	writeJSON(w, http.StatusOK, obj)
}

func (api *fakeAPI) serveWatch(w http.ResponseWriter, r *http.Request, resource string) {
	q := r.URL.Query()
	wt := &fakeWatcher{resource: resource, labels: q.Get("labelSelector"), fields: q.Get("fieldSelector"), events: make(chan fakeEvent, 256)}
	var replay []fakeEvent
	api.mu.Lock()
	if rv := q.Get("resourceVersion"); rv != "" && rv != "0" {
		wt.since, _ = strconv.ParseInt(rv, 10, 64)
	} else {
		for key, obj := range api.objects {
			if strings.HasPrefix(key, resource+"/") && selectorsMatch(obj, wt.labels, wt.fields) {
				replay = append(replay, fakeEvent{typ: kube.Added, object: cloneObject(obj)})
			}
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
	write := func(ev fakeEvent) bool {
		if err := json.NewEncoder(w).Encode(map[string]any{"type": ev.typ, "object": ev.object}); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if flusher != nil {
		flusher.Flush()
	}
	for _, ev := range replay {
		if !write(ev) {
			return
		}
	}
	for {
		select {
		case ev := <-wt.events:
			if ev.rv <= wt.since {
				continue
			}
			if !write(ev) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// selectorsMatch evaluates k=v and bare-key label terms and metadata.name
// field terms, what the driver uses.
func selectorsMatch(obj map[string]any, labels, fields string) bool {
	labelMap := map[string]string{}
	if meta, _ := obj["metadata"].(map[string]any); meta != nil {
		if raw, _ := meta["labels"].(map[string]any); raw != nil {
			for k, v := range raw {
				labelMap[k], _ = v.(string)
			}
		}
	}
	for _, term := range strings.Split(labels, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		if k, v, ok := strings.Cut(term, "="); ok {
			if got, present := labelMap[k]; !present || got != v {
				return false
			}
		} else if _, present := labelMap[term]; !present {
			return false
		}
	}
	for _, term := range strings.Split(fields, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		k, v, _ := strings.Cut(term, "=")
		if k != "metadata.name" || metaString(obj, "name") != v {
			return false
		}
	}
	return true
}

func metaString(obj map[string]any, key string) string {
	meta, _ := obj["metadata"].(map[string]any)
	s, _ := meta[key].(string)
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, reason, message string) {
	writeJSON(w, code, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure", "message": message, "reason": reason, "code": code})
}

func writeNotFound(w http.ResponseWriter, resource, name string) {
	writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure", "message": fmt.Sprintf("%s %q not found", resource, name), "reason": "NotFound", "details": map[string]any{"name": name, "kind": resource}, "code": 404})
}

// The exec side: a WebSocket server speaking v5.channel.k8s.io to the
// scripted guest.

const (
	channelStdin  = 0
	channelStdout = 1
	channelStderr = 2
	channelStatus = 3
	channelClose  = 255
	wsOpBinary    = 0x2
	wsOpClose     = 0x8
	wsOpPing      = 0x9
	wsOpPong      = 0xA
)

// serveExec upgrades the connection and runs the guest's handler for the
// command with the session's channels.
func (api *fakeAPI) serveExec(w http.ResponseWriter, r *http.Request, pod string) {
	q := r.URL.Query()
	api.mu.Lock()
	_, exists := api.objects["pods/"+pod]
	api.mu.Unlock()
	if !exists {
		writeNotFound(w, "pods", pod)
		return
	}
	if q.Get("container") != ContainerName || q.Get("stdout") != "true" {
		writeStatus(w, http.StatusBadRequest, "BadRequest", "exec query "+q.Encode())
		return
	}
	command := q["command"]
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" || !strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), "v5.channel.k8s.io") {
		writeStatus(w, http.StatusBadRequest, "BadRequest", "not a v5 websocket upgrade")
		return
	}
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		api.t.Error(err)
		return
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: v5.channel.k8s.io\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
	ws := &wsServer{conn: conn, br: brw.Reader}
	defer conn.Close()
	stdinReader, stdinWriter := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stdinWriter.Close()
		for {
			op, msg, err := ws.readMessage()
			if err != nil {
				return
			}
			switch op {
			case wsOpClose:
				return
			case wsOpPing:
				ws.writeFrame(wsOpPong, msg)
				continue
			}
			if op != wsOpBinary || len(msg) == 0 {
				continue
			}
			switch msg[0] {
			case channelStdin:
				if _, err := stdinWriter.Write(msg[1:]); err != nil {
					return
				}
			case channelClose:
				if len(msg) > 1 && msg[1] == channelStdin {
					stdinWriter.Close()
				}
			}
		}
	}()
	stdin := io.Reader(stdinReader)
	if q.Get("stdin") != "true" {
		stdin = strings.NewReader("")
	}
	code := api.guest.exec(pod, command, stdin, ws.channelWriter(channelStdout), ws.channelWriter(channelStderr))
	status := map[string]any{"apiVersion": "v1", "kind": "Status", "metadata": map[string]any{}, "status": "Success"}
	if code != 0 {
		status = map[string]any{"apiVersion": "v1", "kind": "Status", "metadata": map[string]any{}, "status": "Failure", "reason": "NonZeroExitCode", "message": fmt.Sprintf("command terminated with non-zero exit code: exit code %d", code), "details": map[string]any{"causes": []any{map[string]any{"reason": "ExitCode", "message": strconv.Itoa(code)}}}}
	}
	raw, _ := json.Marshal(status)
	ws.writeChannel(channelStatus, raw)
	ws.writeFrame(wsOpClose, binary.BigEndian.AppendUint16(nil, 1000))
	_ = stdinReader.CloseWithError(io.EOF)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

type wsServer struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

func (s *wsServer) readMessage() (byte, []byte, error) {
	var message []byte
	var messageOp byte
	for {
		var head [2]byte
		if _, err := io.ReadFull(s.br, head[:]); err != nil {
			return 0, nil, err
		}
		fin := head[0]&0x80 != 0
		op := head[0] & 0x0f
		if head[1]&0x80 == 0 {
			return 0, nil, errors.New("client frame not masked")
		}
		length := uint64(head[1] & 0x7f)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(s.br, ext[:]); err != nil {
				return 0, nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(s.br, ext[:]); err != nil {
				return 0, nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		var mask [4]byte
		if _, err := io.ReadFull(s.br, mask[:]); err != nil {
			return 0, nil, err
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(s.br, payload); err != nil {
			return 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
		if op >= 0x8 {
			return op, payload, nil
		}
		if op != 0 {
			messageOp = op
		}
		message = append(message, payload...)
		if fin {
			return messageOp, message, nil
		}
	}
}

func (s *wsServer) writeFrame(op byte, payload []byte) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	head := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n))
	case n <= 0xffff:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	_, _ = s.conn.Write(append(head, payload...))
}

func (s *wsServer) writeChannel(channel byte, data []byte) {
	s.writeFrame(wsOpBinary, append([]byte{channel}, data...))
}

type channelWriter struct {
	s       *wsServer
	channel byte
}

func (w channelWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), 16<<10)
		w.s.writeChannel(w.channel, p[:n])
		p = p[n:]
	}
	return total, nil
}

func (s *wsServer) channelWriter(channel byte) io.Writer { return channelWriter{s, channel} }
