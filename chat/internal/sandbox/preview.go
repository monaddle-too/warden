package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// publication is one guest port the runtime exposes to the core at
// Address:HostPort (the SBX driver's loopback port; a pod address later)
// and the runner's own availability listener serves at ProxyPort. HostPort
// keeps its name so existing worker state files load unchanged.
type publication struct {
	ID         string
	SandboxID  string
	Port       int
	Address    string
	HostPort   int
	ProxyPort  int
	Generation string
	State      string
	listener   net.Listener
	server     *http.Server
}
type savedAttachmentCall struct {
	Fingerprint  string
	AttachmentID string
}

func validatePreview(r Request) error {
	if r.Port < 1 || r.Port > 65535 {
		return errors.New("preview port must be an integer from 1 to 65535")
	}
	if r.Path == "" || len(r.Path) > 2048 || !strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "//") || strings.ContainsAny(r.Path, "\\?#\r\n\x00") {
		return errors.New("preview path must be an absolute relative path without authority, query or fragment")
	}
	decoded, err := url.PathUnescape(r.Path)
	if err != nil {
		return errors.New("malformed preview path")
	}
	if strings.HasPrefix(decoded, "//") || strings.ContainsAny(decoded, "\\?#\r\n\x00") {
		return errors.New("invalid encoded preview path")
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == ".." || part == "." {
			return errors.New("preview path traversal is not allowed")
		}
	}
	for _, r := range decoded {
		if r < 32 || r == 127 {
			return errors.New("preview path contains control characters")
		}
	}
	if strings.TrimSpace(r.Title) == "" || len(r.Title) > 200 {
		return errors.New("preview title must contain 1 to 200 bytes")
	}
	if r.CallID == "" || len(r.CallID) > 200 {
		return errors.New("tool call ID required")
	}
	return nil
}
func pubKey(sandbox string, port int) string { return sandbox + ":" + strconv.Itoa(port) }
func (w *Worker) attachLocked(ctx context.Context, r Request) (Response, error) {
	s, _, err := w.runLocked(r)
	if err != nil {
		return Response{}, err
	}
	if !s.Active.Streaming {
		return Response{}, errors.New("preview attachment requires an active agent stream")
	}
	if r.Path == "" {
		r.Path = "/"
	}
	if err = validatePreview(r); err != nil {
		return Response{}, err
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", r.Port, r.Path, r.Title)))
	fingerprint := hex.EncodeToString(hash[:])
	call := runKey(r) + ":" + r.CallID
	if saved, ok := w.managed.Calls[call]; ok {
		if saved.Fingerprint != fingerprint {
			return Response{}, errors.New("tool retry changed preview parameters")
		}
		if a := w.managed.Attachments[saved.AttachmentID]; a != nil {
			copy := *a
			return Response{Attachment: &copy}, nil
		}
	}
	if err = w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
		w.failEnforcementLocked(s)
		return Response{}, err
	}
	s.previewAuditAt = w.now()
	key := pubKey(s.ID, r.Port)
	p := w.managed.Publications[key]
	if p == nil {
		if len(w.managed.Publications) >= 256 {
			return Response{}, errors.New("retained preview publication capacity reached")
		}
		p = &publication{ID: randomID(), SandboxID: s.ID, Port: r.Port, State: "publishing"}
		w.managed.Publications[key] = p
	}
	if err = w.verifyMappingsLocked(ctx, s, nil, false); err != nil {
		w.failEnforcementLocked(s)
		return Response{}, err
	}
	if err = w.ensureProxyLocked(p); err != nil {
		return Response{}, err
	}
	if p.State == "available" && p.Generation == s.Generation {
		if err = w.verifyMappingsLocked(ctx, s, p, true); err != nil {
			p.State = "unavailable"
		}
	}
	if p.State != "available" || p.Generation != s.Generation {
		p.State = "publishing"
		if err = w.saveManagedLocked(); err != nil {
			return Response{}, err
		}
		if p.HostPort != 0 {
			if err = w.unpublishIfPresentLocked(ctx, s, p); err != nil {
				p.State = "error"
				_ = w.saveManagedLocked()
				return Response{}, err
			}
		}
		// The driver chooses the endpoint and hands it back through reserve
		// before publishing, so the record is durable before the mapping
		// exists (a crash in between is reconciled on resume).
		_, err = w.Runtime.Publish(ctx, s.RuntimeName, p.Port, func(m PortMapping) error {
			p.Address, p.HostPort = m.Address, m.Port
			return w.saveManagedLocked()
		})
		if err != nil {
			p.State = "error"
			_ = w.saveManagedLocked()
			return Response{}, fmt.Errorf("preview publication failed: %w", err)
		}
		if err = w.verifyMappingsLocked(ctx, s, p, true); err != nil {
			p.State = "error"
			w.failEnforcementLocked(s)
			_ = w.saveManagedLocked()
			return Response{}, err
		}
		p.Generation = s.Generation
		p.State = "available"
	}
	var a *PreviewAttachment
	for _, existing := range w.managed.Attachments {
		if existing.ChatID == r.ChatID && existing.Port == r.Port && existing.Path == r.Path && existing.State != "removed" {
			a = existing
			break
		}
	}
	if a == nil {
		a = &PreviewAttachment{ID: randomID(), ChatID: r.ChatID, SandboxID: r.SandboxID, Port: r.Port, Path: r.Path}
		w.managed.Attachments[a.ID] = a
	}
	a.Title = r.Title
	a.State = "available"
	a.URL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(p.ProxyPort)) + a.Path
	w.managed.Calls[call] = savedAttachmentCall{fingerprint, a.ID}
	s.LastActivity = w.now()
	if err = w.saveManagedLocked(); err != nil {
		p.State = "error"
		a.State = "error"
		a.URL = ""
		return Response{}, err
	}
	copy := *a
	return Response{Attachment: &copy}, nil
}
func (w *Worker) removeLocked(ctx context.Context, r Request) (Response, error) {
	s, _, err := w.bindingLocked(r)
	if err != nil {
		return Response{}, err
	}
	a := w.managed.Attachments[r.AttachmentID]
	if a == nil || a.ChatID != r.ChatID || a.SandboxID != r.SandboxID {
		return Response{}, errors.New("attachment is not owned by this chat")
	}
	a.State = "removed"
	a.URL = ""
	key := pubKey(a.SandboxID, a.Port)
	p := w.managed.Publications[key]
	refs := 0
	for _, other := range w.managed.Attachments {
		if other.SandboxID == a.SandboxID && other.Port == a.Port && other.State != "removed" {
			refs++
		}
	}
	if refs == 0 && p != nil {
		p.State = "removed"
		if err = w.saveManagedLocked(); err != nil {
			return Response{}, err
		}
		// Keep the availability listener bound, serving 410. Its old URL must never
		// become a different local service while this worker is alive.
		if p.HostPort != 0 && s.State == "running" {
			if err = w.Gate.Check(ctx, s.Grant, "runtime"); err != nil {
				return Response{}, err
			}
			if err = w.unpublishIfPresentLocked(ctx, s, p); err != nil {
				return Response{}, err
			}
			// Retain the removed mapping identity: SBX may restore it on resume.
		}
	}
	copy := *a
	return Response{Attachment: &copy}, w.saveManagedLocked()
}
func (w *Worker) ensureProxyLocked(p *publication) error {
	if p.listener != nil {
		return nil
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.ProxyPort))
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return fmt.Errorf("preview availability port cannot be reclaimed: %w", err)
	}
	p.listener = listener
	p.ProxyPort = listener.Addr().(*net.TCPAddr).Port
	id := p.ID
	p.server = &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { w.servePreview(id, rw, r) }), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32768}
	go func(server *http.Server) { _ = server.Serve(listener) }(p.server)
	return nil
}
func (w *Worker) servePreview(id string, rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	var pub *publication
	for _, p := range w.managed.Publications {
		if p.ID == id {
			copy := *p
			pub = &copy
			break
		}
	}
	if pub == nil {
		w.mu.Unlock()
		http.NotFound(rw, r)
		return
	}
	expected := net.JoinHostPort("127.0.0.1", strconv.Itoa(pub.ProxyPort))
	if r.Host != expected {
		w.mu.Unlock()
		http.Error(rw, "Invalid preview host", http.StatusForbidden)
		return
	}
	origin := r.Header.Get("Origin")
	if origin != "" && origin != "http://"+expected {
		w.mu.Unlock()
		http.Error(rw, "Invalid preview origin", http.StatusForbidden)
		return
	}
	s := w.managed.Sandboxes[pub.SandboxID]
	available := s != nil && s.State == "running" && pub.State == "available" && s.Generation == pub.Generation
	audited := s != nil && !s.previewAuditAt.IsZero() && w.now().Before(s.previewAuditAt.Add(90*time.Second))
	w.mu.Unlock()
	rw.Header().Set("Cache-Control", "no-store")
	if !available {
		status := http.StatusServiceUnavailable
		if pub.State == "removed" {
			status = http.StatusGone
		}
		http.Error(rw, "Preview is stopped or unavailable", status)
		return
	}
	if !audited {
		http.Error(rw, "Preview isolation audit is stale; awaiting background verification", http.StatusServiceUnavailable)
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(pub.Address, strconv.Itoa(pub.HostPort))}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		req.Host = target.Host
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("X-CSRF-Token")
		req.Header.Del("Forwarded")
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-Proto")
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Del("Set-Cookie")
		response.Header.Set("Cache-Control", "no-store")
		w.mu.Lock()
		for _, a := range w.managed.Attachments {
			if a.SandboxID == pub.SandboxID && a.Port == pub.Port && a.State == "unavailable" {
				a.State = "available"
			}
		}
		w.mu.Unlock()
		return nil
	}
	proxy.Transport = &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		s := w.managed.Sandboxes[pub.SandboxID]
		current := w.managed.Publications[pubKey(pub.SandboxID, pub.Port)]
		if s == nil || current == nil || s.State != "running" || current.State != "available" || current.Generation != pub.Generation || current.mapping() != pub.mapping() {
			return nil, errors.New("preview generation changed")
		}
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
	}, DisableKeepAlives: true, ResponseHeaderTimeout: 10 * time.Second}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, _ error) {
		w.mu.Lock()
		for _, a := range w.managed.Attachments {
			if a.SandboxID == pub.SandboxID && a.Port == pub.Port && a.State == "available" {
				a.State = "unavailable"
			}
		}
		w.mu.Unlock()
		http.Error(rw, "Preview service is unavailable; restart it from the chat", http.StatusBadGateway)
	}
	proxy.ServeHTTP(rw, r)
}
