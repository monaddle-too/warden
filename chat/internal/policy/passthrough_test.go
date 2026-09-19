package policy

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"
)

// A response too large to inspect from a granted host that is neither a
// model provider nor a Git remote (a dependency download) is delivered
// uninspected, complete, and audited by size; the same size from a
// provider host is still refused, because provider replies are scanned
// for the brokered credential.
func TestGatewayLargeDownloadPassesThroughUninspected(t *testing.T) {
	f := newGatewayFixture(t)
	engine := f.registry.Bindings["s1"].Engine
	policy := engine.PolicyCopy()
	policy["egress"] = map[string]any{"mode": "public", "destinations": []any{}}
	if err := engine.SavePolicy(policy); err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("jar-bytes-"), (17*1024*1024)/10)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/java-archive")
		w.Header().Set("Content-Length", strconv.Itoa(len(large)))
		w.WriteHeader(200)
		w.Write(large)
	})
	res, body, err := f.do("GET", "https://repo1.maven.org/maven2/org/example/big/1.0/big-1.0.jar", nil, "")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("declared-size download: %v %v", res, err)
	}
	if !bytes.Equal(body, large) {
		t.Fatalf("download altered: %d bytes of %d", len(body), len(large))
	}
	if !f.auditContains(`"passthrough":true`) || !f.auditContains(`"complete":true`) || !f.auditContains(`"bytes":`+strconv.Itoa(len(large))) || f.auditContains("jar-bytes") {
		t.Fatal("passthrough not audited by size, or the body leaked")
	}
	// Chunked (no declared size): the buffering attempt overflows and
	// hands over what it read.
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		w.Write(large)
	})
	res, body, err = f.do("GET", "https://repo1.maven.org/maven2/org/example/big/1.0/big-1.0.pom", nil, "")
	if err != nil || res.StatusCode != 200 || !bytes.Equal(body, large) {
		t.Fatalf("undeclared-size download: %v %d bytes %v", res, len(body), err)
	}
	// A provider host of the same size is refused as before.
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write(large)
	})
	res, _, err = f.do("POST", f.providerURL(), providerHeaders, `{}`)
	if err != nil || res.StatusCode != 413 {
		t.Fatalf("oversized provider response: %v %v", res, err)
	}
}
