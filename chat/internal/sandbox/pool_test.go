package sandbox

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"warden/chat/internal/hoststats"
)

func poolFixture(t *testing.T) (*Pool, *Client) {
	t.Helper()
	root, err := os.MkdirTemp("", "pool-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &Pool{config: PoolConfig{Local: "ovh", Routes: filepath.Join(root, "routes.json")}, routes: map[string]placement{}, leases: map[string]bool{}, active: map[string]int{}}
	for _, id := range []string{"ovh", "mac"} {
		socket := filepath.Join(root, id+".sock")
		l, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		capacity := 2
		if id == "mac" {
			capacity = 1
		}
		p.config.Nodes = append(p.config.Nodes, PoolNode{ID: id, Name: id, Capacity: capacity, Socket: socket, client: &Client{Socket: socket, Legacy: true}})
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					reader := bufio.NewReader(c)
					var r Request
					line, _ := reader.ReadBytes('\n')
					json.Unmarshal(line, &r)
					result := Response{Output: id}
					switch r.Operation {
					case "stats":
						result = Response{Revision: "same", Stats: &hoststats.Sample{}, SessionLimit: capacity}
					case "export":
						result.Head = strings.Repeat("a", 40)
						result.Bundle = []byte("bundle-bytes")
					case "import-bundle":
						b := make([]byte, r.BundleSize)
						io.ReadFull(reader, b)
						result.Output = string(b)
					case "stream":
						json.NewEncoder(c).Encode(result)
						io.Copy(c, reader)
						return
					}
					json.NewEncoder(c).Encode(result)
				}()
			}
		}()
	}
	socket := filepath.Join(root, "pool.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve(ctx, l)
	return p, &Client{Socket: socket, Pool: true}
}
func leaseTest(t *testing.T, c *Client, id string, fresh bool) (io.ReadWriteCloser, Response) {
	t.Helper()
	conn, r, err := c.Open(context.Background(), Request{Operation: "lease", ProjectID: "project", SessionID: id, NewSession: fresh})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, r
}
func TestPoolPlacementCapacityAndPersistence(t *testing.T) {
	p, c := poolFixture(t)
	a, r := leaseTest(t, c, "task-a", true)
	if !r.Available || r.RunnerID != "mac" {
		t.Fatal(r)
	}
	_, r = leaseTest(t, c, "task-b", true)
	if !r.Available || r.RunnerID != "ovh" {
		t.Fatal(r)
	}
	_, r = leaseTest(t, c, "legacy", false)
	if !r.Available || r.RunnerID != "ovh" {
		t.Fatal(r)
	}
	_, r = leaseTest(t, c, "task-d", true)
	if r.Available {
		t.Fatal("overbooked")
	}
	_, err := c.Call(context.Background(), Request{Operation: "prepare", ProjectID: "other", SessionID: "task-a"})
	if err == nil {
		t.Fatal("cross project accepted")
	}
	var saved map[string]placement
	b, _ := os.ReadFile(p.config.Routes)
	if json.Unmarshal(b, &saved) != nil || saved["task-a"].Worker != "mac" {
		t.Fatal("placement not durable")
	}
	// A queued session is pinned even when its machine goes offline or updates.
	a.Close()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		held := p.leases["task-a"]
		p.mu.Unlock()
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("leaked lease")
		}
		time.Sleep(time.Millisecond)
	}
	p.probeMu.Lock()
	p.statusCache = []WorkerStatus{{ID: "ovh", Online: true, Compatible: true, Capacity: 2}, {ID: "mac", Online: false, Compatible: true, Capacity: 1}}
	p.statusAt = time.Now()
	p.probeMu.Unlock()
	_, r = leaseTest(t, c, "task-a", false)
	if r.Available {
		t.Fatal("offline session migrated")
	}
	p.probeMu.Lock()
	p.statusCache[1].Online = true
	p.statusCache[1].Compatible = false
	p.probeMu.Unlock()
	_, r = leaseTest(t, c, "task-a", false)
	if r.Available {
		t.Fatal("incompatible worker admitted")
	}
}
func TestPoolConcurrentLeaseAndCrossRunnerTransfer(t *testing.T) {
	p, c := poolFixture(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, r, err := c.Open(context.Background(), Request{Operation: "lease", ProjectID: "project", SessionID: "task-" + big.NewInt(int64(i)).String(), NewSession: true})
			if err != nil {
				t.Error(err)
				return
			}
			t.Cleanup(func() { conn.Close() })
			if r.Available {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != 3 {
		t.Fatalf("admitted %d", accepted)
	}
	p.mu.Lock()
	p.routes["source"] = placement{"project", "mac"}
	p.routes["dest"] = placement{"project", "ovh"}
	p.mu.Unlock()
	r, err := c.Call(context.Background(), Request{Operation: "import", ProjectID: "project", SessionID: "dest", SourceSessionID: "source", Expected: strings.Repeat("a", 40)})
	if err != nil || r.Output != "bundle-bytes" {
		t.Fatalf("transfer: %+v %v", r, err)
	}
	_, err = c.Call(context.Background(), Request{Operation: "import", ProjectID: "project", SessionID: "dest", SourceSessionID: "source", Expected: strings.Repeat("b", 40)})
	if err == nil {
		t.Fatal("changed snapshot accepted")
	}
	stream, _, err := c.Open(context.Background(), Request{Operation: "stream", ProjectID: "project", SessionID: "dest"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	stream.Write([]byte("hello"))
	b := make([]byte, 5)
	if _, err = io.ReadFull(stream, b); err != nil || string(b) != "hello" {
		t.Fatal("stream relay failed", err)
	}
}
func TestRemoteWorkerRequiresMutualTLS(t *testing.T) {
	root := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	os.WriteFile(filepath.Join(root, "ca"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	for i, name := range []string{"server", "client"} {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), DNSNames: []string{name}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, _ := x509.CreateCertificate(rand.Reader, cert, ca, &k.PublicKey, key)
		kd, _ := x509.MarshalECPrivateKey(k)
		os.WriteFile(filepath.Join(root, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
		os.WriteFile(filepath.Join(root, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0600)
	}
	server, err := ServerTLS(filepath.Join(root, "ca"), filepath.Join(root, "server.crt"), filepath.Join(root, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var r Request
				if json.NewDecoder(c).Decode(&r) == nil {
					json.NewEncoder(c).Encode(Response{Output: "ok"})
				}
			}()
		}
	}()
	config, err := ClientTLS(filepath.Join(root, "ca"), filepath.Join(root, "client.crt"), filepath.Join(root, "client.key"), "server")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Address: l.Addr().String(), TLS: config, Legacy: true}
	if _, err = c.Call(context.Background(), Request{Operation: "health"}); err != nil {
		t.Fatal(err)
	}
	c.TLS = config.Clone()
	c.TLS.Certificates = nil
	if _, err = c.Call(context.Background(), Request{Operation: "health"}); err == nil {
		t.Fatal("anonymous client accepted")
	}
	c.TLS = config.Clone()
	c.TLS.ServerName = "wrong"
	if _, err = c.Call(context.Background(), Request{Operation: "health"}); err == nil {
		t.Fatal("wrong server accepted")
	}
	c.TLS = nil
	if _, err = c.Call(context.Background(), Request{Operation: "health"}); err == nil {
		t.Fatal("plaintext accepted")
	}
}

func TestPoolUpdateDrainsBeforeReplacingWorker(t *testing.T) {
	p, c := poolFixture(t)
	conn, r := leaseTest(t, c, "task-update", true)
	if r.RunnerID != "mac" {
		t.Fatal(r)
	}
	revision := strings.Repeat("a", 40)
	request := Request{Operation: "update", SessionID: "mac", Expected: revision}
	result, err := c.Call(context.Background(), request)
	if err != nil || result.Available {
		t.Fatal("updating active worker", result, err)
	}
	conn.Close()
	for i := 0; i < 100; i++ {
		p.mu.Lock()
		held := p.leases["task-update"]
		p.mu.Unlock()
		if !held {
			break
		}
		time.Sleep(time.Millisecond)
	}
	result, err = c.Call(context.Background(), request)
	if err != nil || !result.Available {
		t.Fatal("idle worker not drained", result, err)
	}
	_, r = leaseTest(t, c, "task-update", false)
	if r.Available {
		t.Fatal("admitted during update")
	}
	p.probeMu.Lock()
	for i := range p.statusCache {
		p.statusCache[i].Revision = revision
	}
	p.statusAt = time.Now()
	p.probeMu.Unlock()
	conn, r = leaseTest(t, c, "task-update", false)
	if !r.Available {
		t.Fatal("compatible worker not reopened")
	}
	conn.Close()
	for i := 0; i < 100; i++ {
		p.mu.Lock()
		held := p.leases["task-update"]
		p.mu.Unlock()
		if !held {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Replaying an already-installed release must never close/reopen the gate
	// and then restart a worker that can already accept new tasks.
	result, err = c.Call(context.Background(), request)
	if err != nil || result.Available {
		t.Fatal("reinstall of current worker accepted")
	}
}
