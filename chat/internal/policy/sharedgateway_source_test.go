package policy

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The source check (decision 4): a valid credential is honoured only from
// the pod the binding runs in; loopback (health probes) is always allowed.
func TestSharedGatewayRefusesACredentialFromAnotherPod(t *testing.T) {
	f := newSharedFixture(t)
	var mu sync.Mutex
	allowed := map[string]bool{}
	f.gateway.Source = func(runtimeName string, remote net.IP) bool {
		mu.Lock()
		defer mu.Unlock()
		if remote.IsLoopback() {
			return true
		}
		return allowed[runtimeName]
	}
	f.begin("s1")
	f.openEgress("s1")
	// The fixture's clients dial from loopback, which the hook treats as the
	// binding's own pod when the runtime is marked allowed; unmarking it is
	// the "another pod" case, since loopback is not special to the gateway
	// itself.
	f.gateway.Source = func(runtimeName string, remote net.IP) bool {
		mu.Lock()
		defer mu.Unlock()
		return allowed[runtimeName]
	}
	// An absolute-URI request: a refusal comes back as a response (a refused
	// CONNECT surfaces as a client error instead).
	res, body := f.do(f.client(f.endpoint("s1").ProxyURL()), "GET", "http://example.com/plain", nil, "")
	if res == nil || res.StatusCode != 403 || !strings.Contains(string(body), "another sandbox") {
		t.Fatalf("credential from another pod: %v %s", res, body)
	}
	if dials := f.dialCount(); dials != 0 {
		t.Fatalf("upstream dialled %d times for a refused source", dials)
	}
	mu.Lock()
	allowed["sbx-s1"] = true
	mu.Unlock()
	res, _ = f.do(f.client(f.endpoint("s1").ProxyURL()), "GET", "https://example.com/page", map[string]string{"Authorization": "Bearer site-login"}, "")
	if res == nil || res.StatusCode != 200 {
		t.Fatalf("credential from its own pod: %v", res)
	}
	// Bearer (provider route) requests go through the same check.
	mu.Lock()
	allowed["sbx-s1"] = false
	mu.Unlock()
	direct := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DialContext: f.dial}}
	res, _ = f.do(direct, "GET", f.gateway.probe+"/openai/v1/models", map[string]string{"Authorization": "Bearer " + f.endpoint("s1").Placeholder()}, "")
	if res == nil || res.StatusCode != 403 {
		t.Fatalf("bearer from another pod: %v", res)
	}
}
