package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Resource names an API resource: the empty group is the core API
// (/api/v1), any other group is served under /apis. Kind is what Create
// and Apply put in the object's kind field when the caller leaves it empty.
type Resource struct {
	Group, Version, Resource, Kind string
	// Subresource names one of the object's subresources (resize, status);
	// the path puts it after the object's name.
	Subresource string
}

// Sub is the resource's named subresource.
func (r Resource) Sub(name string) Resource {
	r.Subresource = name
	return r
}

// The resources Warden uses.
var (
	Pods                              = Resource{Group: "", Version: "v1", Resource: "pods", Kind: "Pod"}
	PersistentVolumeClaims            = Resource{Group: "", Version: "v1", Resource: "persistentvolumeclaims", Kind: "PersistentVolumeClaim"}
	Secrets                           = Resource{Group: "", Version: "v1", Resource: "secrets", Kind: "Secret"}
	ConfigMaps                        = Resource{Group: "", Version: "v1", Resource: "configmaps", Kind: "ConfigMap"}
	Namespaces                        = Resource{Group: "", Version: "v1", Resource: "namespaces", Kind: "Namespace"}
	NetworkPolicies                   = Resource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies", Kind: "NetworkPolicy"}
	RuntimeClasses                    = Resource{Group: "node.k8s.io", Version: "v1", Resource: "runtimeclasses", Kind: "RuntimeClass"}
	ValidatingAdmissionPolicies       = Resource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies", Kind: "ValidatingAdmissionPolicy"}
	ValidatingAdmissionPolicyBindings = Resource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings", Kind: "ValidatingAdmissionPolicyBinding"}
	Nodes                             = Resource{Group: "", Version: "v1", Resource: "nodes", Kind: "Node"}
	// PodsResize is the pods/resize subresource (Kubernetes 1.33+): a
	// patch to it changes a running container's requests and limits in
	// place, which the kubelet then applies without a restart.
	PodsResize = Pods.Sub("resize")
	// NodeMetrics and PodMetrics are the metrics server's live usage
	// (metrics.k8s.io); absent on a cluster without one.
	NodeMetrics = Resource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes", Kind: "NodeMetrics"}
	PodMetrics  = Resource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods", Kind: "PodMetrics"}
)

// APIVersion is the group/version string of the resource.
func (r Resource) APIVersion() string {
	if r.Group == "" {
		return r.Version
	}
	return r.Group + "/" + r.Version
}

// path is the URL path of the collection (name empty) or the object.
// Cluster-scoped resources pass an empty namespace.
func (r Resource) path(namespace, name string) string {
	var b strings.Builder
	if r.Group == "" {
		b.WriteString("/api/")
	} else {
		b.WriteString("/apis/")
		b.WriteString(r.Group)
		b.WriteByte('/')
	}
	b.WriteString(r.Version)
	if namespace != "" {
		b.WriteString("/namespaces/")
		b.WriteString(url.PathEscape(namespace))
	}
	b.WriteByte('/')
	b.WriteString(r.Resource)
	if name != "" {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(name))
		if r.Subresource != "" {
			b.WriteByte('/')
			b.WriteString(r.Subresource)
		}
	}
	return b.String()
}

// DefaultTimeout bounds each non-streaming call when the caller's context
// has no earlier deadline.
const DefaultTimeout = 30 * time.Second

// Client talks to one API server. Every call honours its context; the
// non-streaming verbs are also bounded by Timeout. A Client is safe for
// concurrent use.
type Client struct {
	// Timeout bounds Get, List, Create, Update, Delete, Patch and Apply,
	// and the connection and handshake of Watch, Logs and Exec.
	Timeout time.Duration
	// UserAgent is sent with every request.
	UserAgent string

	base      *url.URL
	tls       *tls.Config
	http      *http.Client
	token     *tokenSource
	namespace string
}

// NewClient builds a client for the configuration.
func NewClient(cfg *Config) (*Client, error) {
	base, err := cfg.baseURL()
	if err != nil {
		return nil, err
	}
	tlsConfig, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}
	// The transport gets its own copy: with ForceAttemptHTTP2 it adds h2
	// to NextProtos in place, and the exec WebSocket must not offer h2.
	transport := &http.Transport{
		Proxy:                 nil, // the API server is reached directly, never through HTTP_PROXY
		DialContext:           (&net.Dialer{Timeout: DefaultTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConfig.Clone(),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   DefaultTimeout,
		ResponseHeaderTimeout: DefaultTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          8,
	}
	return &Client{
		Timeout:   DefaultTimeout,
		UserAgent: "warden-kube",
		base:      base,
		tls:       tlsConfig,
		http:      &http.Client{Transport: transport},
		token:     newTokenSource(cfg),
		namespace: cfg.Namespace,
	}, nil
}

// Namespace is the configuration's default namespace, empty when unknown.
func (c *Client) Namespace() string { return c.namespace }

// Host is the API server URL.
func (c *Client) Host() string { return c.base.String() }

// Get reads one object into into (a typed struct or an *Object).
func (c *Client) Get(ctx context.Context, r Resource, namespace, name string, into any) error {
	if name == "" {
		return errors.New("kube: Get needs a name")
	}
	return c.do(ctx, http.MethodGet, r.path(namespace, name), nil, "", nil, into)
}

// List reads a collection into into, a *List[T] or *List[Object]; an empty
// namespace lists across all namespaces for namespaced resources.
func (c *Client) List(ctx context.Context, r Resource, namespace string, opts ListOptions, into any) error {
	q := url.Values{}
	if opts.LabelSelector != "" {
		q.Set("labelSelector", opts.LabelSelector)
	}
	if opts.FieldSelector != "" {
		q.Set("fieldSelector", opts.FieldSelector)
	}
	return c.do(ctx, http.MethodGet, r.path(namespace, ""), q, "", nil, into)
}

// Create posts obj; the server's answer is decoded into into when it is
// not nil. apiVersion and kind are filled from r when obj leaves them
// empty.
func (c *Client) Create(ctx context.Context, r Resource, namespace string, obj, into any) error {
	body, err := encodeObject(r, namespace, "", obj)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, r.path(namespace, ""), nil, "application/json", body, into)
}

// Update replaces the object; obj's resourceVersion, when set, makes the
// update conditional (IsConflict on a stale one).
func (c *Client) Update(ctx context.Context, r Resource, namespace, name string, obj, into any) error {
	if name == "" {
		return errors.New("kube: Update needs a name")
	}
	body, err := encodeObject(r, namespace, name, obj)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, r.path(namespace, name), nil, "application/json", body, into)
}

// Delete removes the object. A missing object is IsNotFound; a UID
// precondition that no longer holds is IsConflict.
func (c *Client) Delete(ctx context.Context, r Resource, namespace, name string, opts DeleteOptions) error {
	if name == "" {
		return errors.New("kube: Delete needs a name")
	}
	options := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions"}
	if opts.GracePeriodSeconds != nil {
		options["gracePeriodSeconds"] = *opts.GracePeriodSeconds
	}
	if opts.Propagation != "" {
		options["propagationPolicy"] = opts.Propagation
	}
	if opts.UID != "" {
		options["preconditions"] = map[string]any{"uid": opts.UID}
	}
	body, err := json.Marshal(options)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, r.path(namespace, name), nil, "application/json", body, nil)
}

// Patch sends a JSON merge patch (RFC 7386): a null removes a field, which
// is how a label is taken away. MergePatchLabels builds the common one.
func (c *Client) Patch(ctx context.Context, r Resource, namespace, name string, patch []byte, into any) error {
	if name == "" {
		return errors.New("kube: Patch needs a name")
	}
	if !json.Valid(patch) {
		return errors.New("kube: patch is not valid JSON")
	}
	return c.do(ctx, http.MethodPatch, r.path(namespace, name), nil, "application/merge-patch+json", patch, into)
}

// StrategicPatch sends a strategic merge patch: unlike a merge patch it
// merges a list of named objects (a pod's containers) entry by entry, so a
// container's resources can be changed without restating the container.
// It is the patch type the pods/resize subresource takes from kubectl.
func (c *Client) StrategicPatch(ctx context.Context, r Resource, namespace, name string, patch []byte, into any) error {
	if name == "" {
		return errors.New("kube: StrategicPatch needs a name")
	}
	if !json.Valid(patch) {
		return errors.New("kube: patch is not valid JSON")
	}
	return c.do(ctx, http.MethodPatch, r.path(namespace, name), nil, "application/strategic-merge-patch+json", patch, into)
}

// MergePatchLabels is the merge patch that sets and removes labels.
func MergePatchLabels(set map[string]string, remove ...string) []byte {
	labels := map[string]any{}
	for k, v := range set {
		labels[k] = v
	}
	for _, k := range remove {
		labels[k] = nil
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	return patch
}

// Apply is server-side apply: obj is the caller's whole intent for the
// fields it manages, sent as application/apply-patch+yaml with the field
// manager; the object is created when missing. Force takes over fields
// another manager owns; without it such a field is IsConflict.
func (c *Client) Apply(ctx context.Context, r Resource, namespace, name string, obj any, opts ApplyOptions, into any) error {
	if name == "" {
		return errors.New("kube: Apply needs a name")
	}
	if opts.FieldManager == "" {
		return errors.New("kube: Apply needs a field manager")
	}
	body, err := encodeObject(r, namespace, name, obj)
	if err != nil {
		return err
	}
	q := url.Values{"fieldManager": {opts.FieldManager}}
	if opts.Force {
		q.Set("force", "true")
	}
	return c.do(ctx, http.MethodPatch, r.path(namespace, name), q, "application/apply-patch+yaml", body, into)
}

// ServerVersion reads /version.
func (c *Client) ServerVersion(ctx context.Context) (Version, error) {
	var v Version
	err := c.do(ctx, http.MethodGet, "/version", nil, "", nil, &v)
	return v, err
}

// encodeObject marshals obj with apiVersion, kind, metadata.name and
// metadata.namespace filled in when absent.
func encodeObject(r Resource, namespace, name string, obj any) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("kube: encode %s: %w", r.Kind, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("kube: %s must encode as a JSON object", r.Kind)
	}
	if isEmptyJSON(m["apiVersion"]) {
		m["apiVersion"] = jsonString(r.APIVersion())
	}
	if isEmptyJSON(m["kind"]) {
		m["kind"] = jsonString(r.Kind)
	}
	var meta map[string]json.RawMessage
	if len(m["metadata"]) > 0 {
		if err := json.Unmarshal(m["metadata"], &meta); err != nil {
			return nil, fmt.Errorf("kube: %s metadata must be a JSON object", r.Kind)
		}
	}
	if meta == nil {
		meta = map[string]json.RawMessage{}
	}
	if name != "" {
		if got := jsonStringValue(meta["name"]); got == "" {
			meta["name"] = jsonString(name)
		} else if got != name {
			return nil, fmt.Errorf("kube: object name %q does not match %q", got, name)
		}
	}
	if namespace != "" {
		if got := jsonStringValue(meta["namespace"]); got == "" {
			meta["namespace"] = jsonString(namespace)
		} else if got != namespace {
			return nil, fmt.Errorf("kube: object namespace %q does not match %q", got, namespace)
		}
	}
	if m["metadata"], err = json.Marshal(meta); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func isEmptyJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == `""` || string(raw) == "null"
}

func jsonString(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

func jsonStringValue(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// do performs one bounded request and decodes a JSON answer. A 401 is
// retried once after re-reading the token file.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, contentType string, body []byte, into any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	resp, err := c.roundTrip(ctx, method, path, query, contentType, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp)
	}
	if into == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("kube: decode %s %s: %w", method, path, err)
	}
	return nil
}

// stream performs a request whose body the caller reads (watch, logs). The
// returned response has a 2xx status; the caller closes its body, and the
// context's cancellation ends reads with an error.
func (c *Client) stream(ctx context.Context, method, path string, query url.Values, accept string) (*http.Response, error) {
	resp, err := c.roundTrip(ctx, method, path, query, "", nil, accept)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, statusError(resp)
	}
	return resp, nil
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// roundTrip sends the request, once more after a 401 when the token can be
// re-read. Any non-401 response is returned as is.
func (c *Client) roundTrip(ctx context.Context, method, path string, query url.Values, contentType string, body []byte, accept string) (*http.Response, error) {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = query.Encode()
	for attempt := 0; ; attempt++ {
		token, err := c.token.get(attempt > 0)
		if err != nil {
			return nil, err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.ContentLength = int64(len(body))
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", c.UserAgent)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("kube: %s %s: %w", method, path, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.token.refreshable() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
}

// StatusError is a non-2xx answer from the API server: Code is the HTTP
// status, Reason the API's machine-readable reason (NotFound, Conflict,
// Forbidden, AlreadyExists, Expired, ...) and Message its explanation.
// Details carries the object's name and any causes. A response that is not
// a Status object (a proxy's error page) still becomes a StatusError with
// the code and a trimmed body.
type StatusError struct {
	Code    int
	Reason  string
	Message string
	Details *StatusDetails
}

// Error implements error.
func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("kube: %s (%d %s)", e.Message, e.Code, reasonOrText(e.Code, e.Reason))
	}
	return fmt.Sprintf("kube: %d %s", e.Code, reasonOrText(e.Code, e.Reason))
}

func reasonOrText(code int, reason string) string {
	if reason != "" {
		return reason
	}
	return http.StatusText(code)
}

func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	e := &StatusError{Code: resp.StatusCode}
	var status Status
	if err := json.Unmarshal(body, &status); err == nil && status.Kind == "Status" {
		e.Reason, e.Message, e.Details = status.Reason, status.Message, status.Details
		if status.Code != 0 {
			e.Code = status.Code
		}
		return e
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		if len(text) > 200 {
			text = text[:200] + "..."
		}
		e.Message = text
	}
	return e
}

// statusErrorFrom converts a Status object (an ERROR watch event, an exec
// status) into a StatusError.
func statusErrorFrom(status Status) *StatusError {
	code := status.Code
	if code == 0 {
		code = http.StatusInternalServerError
	}
	return &StatusError{Code: code, Reason: status.Reason, Message: status.Message, Details: status.Details}
}

func statusCode(err error) (int, string) {
	var e *StatusError
	if errors.As(err, &e) {
		return e.Code, e.Reason
	}
	return 0, ""
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool { code, _ := statusCode(err); return code == http.StatusNotFound }

// IsConflict reports a 409 with reason Conflict (a stale resourceVersion,
// a failed precondition, an apply conflict).
func IsConflict(err error) bool {
	code, reason := statusCode(err)
	return code == http.StatusConflict && reason != "AlreadyExists"
}

// IsAlreadyExists reports a 409 with reason AlreadyExists.
func IsAlreadyExists(err error) bool {
	code, reason := statusCode(err)
	return code == http.StatusConflict && reason == "AlreadyExists"
}

// IsForbidden reports a 403.
func IsForbidden(err error) bool { code, _ := statusCode(err); return code == http.StatusForbidden }

// IsUnauthorized reports a 401.
func IsUnauthorized(err error) bool {
	code, _ := statusCode(err)
	return code == http.StatusUnauthorized
}

// IsGone reports a 410, which a watch returns when its resourceVersion is
// too old to resume from.
func IsGone(err error) bool {
	code, reason := statusCode(err)
	return code == http.StatusGone || reason == "Expired"
}

// RetryAfter returns the server's retry hint from a StatusError, or zero.
func RetryAfter(err error) time.Duration {
	var e *StatusError
	if errors.As(err, &e) && e.Details != nil && e.Details.RetryAfterSeconds > 0 {
		return time.Duration(e.Details.RetryAfterSeconds) * time.Second
	}
	return 0
}

// quoteBool renders a boolean query parameter.
func quoteBool(b bool) string { return strconv.FormatBool(b) }
