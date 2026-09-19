package chats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
	"warden/chat/internal/agent"
	cv "warden/chat/internal/conversation"
	"warden/chat/internal/sandbox"
	"warden/chat/internal/transport"
)

// A published address always maps to this exact chat, sandbox and port. IDs are
// never recycled, including after revocation. The URL is not an access token.
type PortBinding struct {
	ID           string `json:"id"`
	ChatID       string `json:"chatID"`
	SandboxID    string `json:"sandboxID"`
	AttachmentID string `json:"attachmentID"`
	Port         int    `json:"port"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	State        string `json:"state"`
	// Upstream is "host" for a host port a jailbroken workspace exposed
	// (host.go), "" for a sandbox port.
	Upstream string `json:"upstream,omitempty"`
}
type portInput struct {
	Port     int    `json:"port"`
	Path     string `json:"path"`
	Title    string `json:"title"`
	Upstream string `json:"-"`
}

func decodePort(value any) (portInput, error) {
	var input portInput
	raw, _ := json.Marshal(value)
	if s, ok := value.(string); ok {
		raw = []byte(s)
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	err := d.Decode(&input)
	if err == nil && d.Decode(&struct{}{}) != io.EOF {
		err = errors.New("one port request required")
	}
	if input.Path == "" {
		input.Path = "/"
	}
	if err == nil && (input.Port < 1 || input.Port > 65535 || len(input.Title) > 160 || input.Title == "" || !strings.HasPrefix(input.Path, "/") || strings.HasPrefix(input.Path, "//")) {
		err = errors.New("a valid sandbox port, relative path and title are required")
	}
	return input, err
}
func toolResult(value any, err error) map[string]any {
	b, _ := json.Marshal(value)
	text := string(b)
	if err != nil {
		text = err.Error()
	}
	return map[string]any{"success": err == nil, "contentItems": []any{map[string]any{"type": "inputText", "text": text}}}
}
func (e *Engine) requestPort(c *Chat, client *agent.Client, f agent.Frame) error {
	if e.PublicPreviewSuffix == "" {
		return client.Reply(f.ID, toolResult(nil, errors.New("external previews are not configured")))
	}
	input, err := decodePort(f.Params["arguments"])
	if err != nil {
		return client.Reply(f.ID, toolResult(nil, err))
	}
	// Existing approved bindings can be revalidated after a sandbox restart; a
	// revoked binding requires a new human approval and receives a fresh URL.
	st := e.Store.Snapshot()
	for _, p := range st.Ports {
		if p.ChatID == c.ID && p.SandboxID == c.SandboxID && p.Port == input.Port && p.Upstream == "" && p.State == "approved" {
			value, err := e.bindPort(c, input, p.ID)
			return client.Reply(f.ID, toolResult(value, err))
		}
	}
	return e.Store.update(func(st *State) error {
		chat := st.chat(c.ID)
		if chat.Status != "running" || chat.RunID != c.RunID {
			return errors.New("run expired")
		}
		chat.Approvals = append(chat.Approvals, Approval{ID: cv.ID(), RunID: c.RunID, RPCID: append(json.RawMessage(nil), f.ID...), Method: "warden/ports/bind", Params: map[string]any{"port": input.Port, "path": input.Path, "title": input.Title, "access": "Signed-in Warden users only"}, State: "pending"})
		return nil
	})
}
func (e *Engine) bindPort(c *Chat, input portInput, id string) (PortBinding, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	op := "preview.attach"
	if input.Upstream == sandbox.UpstreamHost {
		op = "host.expose" // a host port (host.go), minted by the runner as well
	}
	r := request(c, op)
	r.Port = input.Port
	r.Path = "/"
	r.Title = input.Title
	r.CallID = cv.ID()
	res, err := e.Worker.Call(ctx, r)
	if err != nil {
		return PortBinding{}, err
	}
	a := res.Attachment
	if a == nil || a.ChatID != c.ID || a.SandboxID != c.SandboxID || a.Port != input.Port || a.Upstream != input.Upstream {
		return PortBinding{}, errors.New("worker port binding mismatch")
	}
	if err = e.validateAttachment(*a); err != nil {
		return PortBinding{}, err
	}
	if id == "" {
		id = cv.ID()
	}
	p := PortBinding{ID: id, ChatID: c.ID, SandboxID: c.SandboxID, AttachmentID: a.ID, Port: input.Port, Title: input.Title, URL: e.previewURL(id, input.Path), State: "approved", Upstream: input.Upstream}
	err = e.Store.update(func(st *State) error {
		current := st.chat(c.ID)
		if current.Status != "running" || current.RunID != c.RunID {
			return errors.New("run stopped before binding completed")
		}
		for i, old := range st.Ports {
			if old.ID == id {
				if old.State != "approved" {
					return errors.New("binding was revoked")
				}
				st.Ports[i] = p
				return nil
			}
		}
		if len(st.Ports) >= 1024 {
			return errors.New("port binding limit reached")
		}
		st.Ports = append(st.Ports, p)
		return nil
	})
	if err != nil {
		remove := request(c, "preview.remove")
		remove.AttachmentID = a.ID
		_, _ = e.Worker.Call(ctx, remove)
	}
	return p, err
}
func (e *Engine) resolvePort(c *Chat, a Approval, allow bool) any {
	if !allow {
		return toolResult(nil, errors.New("port binding denied by Warden owner"))
	}
	raw := map[string]any{"port": a.Params["port"], "path": a.Params["path"], "title": a.Params["title"]}
	input, err := decodePort(raw)
	if err != nil {
		return toolResult(nil, err)
	}
	binding, err := e.bindPort(c, input, "")
	return toolResult(binding, err)
}
func (e *Engine) RevokePort(ctx context.Context, id string) error {
	var p PortBinding
	var c *Chat
	// Revoke public access durably before touching the worker. Failure to remove
	// its loopback mapping must never restore external access.
	err := e.Store.update(func(st *State) error {
		for i := range st.Ports {
			if st.Ports[i].ID == id {
				st.Ports[i].State = "revoked"
				p = st.Ports[i]
				copy := *st.chat(p.ChatID)
				c = &copy
				return nil
			}
		}
		return errors.New("binding not found")
	})
	if err != nil {
		return err
	}
	r := request(c, "preview.remove")
	r.AttachmentID = p.AttachmentID
	_, err = e.Worker.Call(ctx, r)
	return err
}
func (e *Engine) ServePort(id, path string, w http.ResponseWriter, r *http.Request) {
	st := e.Store.Snapshot()
	var binding *PortBinding
	for _, p := range st.Ports {
		if p.ID == id && p.State == "approved" {
			copy := p
			binding = &copy
			break
		}
	}
	if binding == nil {
		http.Error(w, "port binding revoked or unknown", 410)
		return
	}
	res, err := e.Runtime(r.Context(), binding.ChatID, "status")
	if err != nil {
		http.Error(w, "sandbox unavailable", 503)
		return
	}
	var a *sandbox.PreviewAttachment
	for _, p := range res.Attachments {
		if p.ID == binding.AttachmentID && p.Port == binding.Port && p.Upstream == binding.Upstream && p.State == "available" {
			copy := p
			a = &copy
			break
		}
	}
	if a == nil {
		http.Error(w, "preview unavailable; resume its sandbox and server", 503)
		return
	}
	// The attachment URL is the runner's loopback listener for this
	// publication (http://127.0.0.1:<port>/) or, on Kubernetes, its shared
	// mutual-TLS preview server with the publication's path prefix
	// (https://warden-runner:<port>/<id>/); the viewer's path is served
	// under that prefix. e.validateAttachment admitted the URL at binding
	// time and the runner's answer is checked again here.
	if err = e.validateAttachment(*a); err != nil {
		http.Error(w, "invalid worker mapping", 502)
		return
	}
	target, err := url.Parse(a.URL)
	if err != nil {
		http.Error(w, "invalid worker mapping", 502)
		return
	}
	prefix := strings.TrimSuffix(target.Path, "/")
	target.Path = ""
	target.RawPath = ""
	target.RawQuery = ""
	upstream, err := e.previewUpstream(target)
	if err != nil {
		http.Error(w, "preview upstream unavailable", 502)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = prefix + path
		req.URL.RawPath = ""
		req.Host = target.Host
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		req.Header.Del("X-Warden-CSRF")
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Forwarded")
		req.Header.Del("X-Forwarded-For")
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-Proto")
		if req.Header.Get("Origin") != "" {
			req.Header.Set("Origin", target.Scheme+"://"+target.Host)
		}
	}
	proxy.Transport = upstream
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(res *http.Response) error {
		res.Header.Del("Set-Cookie")
		res.Header.Del("Content-Security-Policy")
		res.Header.Set("Cache-Control", "no-store")
		if loc := res.Header.Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil && u.Host == target.Host {
				u.Host = ""
				u.Scheme = ""
				if prefix != "" && strings.HasPrefix(u.Path, prefix+"/") {
					u.Path = strings.TrimPrefix(u.Path, prefix)
					u.RawPath = ""
				}
				res.Header.Set("Location", u.String())
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "preview upstream unavailable", 502)
	}
	w.Header().Del("Content-Security-Policy")
	w.Header().Set("Content-Type", "text/html")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				state := e.Store.Snapshot()
				allowed := false
				for _, p := range state.Ports {
					if p.ID == id && p.State == "approved" {
						allowed = true
						break
					}
				}
				if !allowed {
					cancel()
					return
				}
			}
		}
	}()
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// previewUpstream is the transport that reaches an attachment's origin: a
// plain one for the runner's loopback listeners (as before), and for its
// shared preview server one mutual-TLS transport kept for reuse, the
// runner verified against the deployment CA under the advertised host
// name and this service's certificate presented (transport.ClientConfig,
// which re-reads a renewed certificate at each handshake).
func (e *Engine) previewUpstream(target *url.URL) (http.RoundTripper, error) {
	if target.Scheme != "https" {
		return &http.Transport{Proxy: nil, ResponseHeaderTimeout: 15 * time.Second}, nil
	}
	e.previewMu.Lock()
	defer e.previewMu.Unlock()
	if e.previewTransport == nil {
		host := e.RunnerPreviewHost
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		config, err := transport.ClientConfig(e.RunnerPreviewTLS, host)
		if err != nil {
			return nil, err
		}
		e.previewTransport = &http.Transport{Proxy: nil, TLSClientConfig: config, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 90 * time.Second}
	}
	return e.previewTransport, nil
}

// previewURL is the address viewers open for an approved binding. Each
// binding has its own origin under the preview suffix so the edge's
// per-binding session and revocation apply unchanged in both modes.
func (e *Engine) previewURL(id, path string) string {
	scheme := e.PreviewScheme
	if scheme == "" {
		scheme = "https"
	}
	host := id + "." + e.PublicPreviewSuffix
	if e.PreviewPort != "" {
		host += ":" + e.PreviewPort
	}
	return scheme + "://" + host + path
}

// ValidatePreviewSuffix accepts an empty suffix (previews unconfigured),
// "localhost" (loopback previews, resolved by browsers without DNS) or a
// dotted public hostname suffix.
func ValidatePreviewSuffix(s string) error {
	if s == "" || s == "localhost" {
		return nil
	}
	u, err := url.Parse("https://" + s)
	if err != nil || u.Host != s || u.Port() != "" || strings.ContainsAny(s, "/*?#@") || !strings.Contains(s, ".") {
		return fmt.Errorf("invalid public preview hostname suffix %s", strconv.Quote(s))
	}
	return nil
}
