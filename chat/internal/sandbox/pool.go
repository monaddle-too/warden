package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"warden/chat/internal/hoststats"
)

type WorkerStatus struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Online     bool              `json:"online"`
	Compatible bool              `json:"compatible"`
	Active     int               `json:"active"`
	Capacity   int               `json:"capacity"`
	Revision   string            `json:"revision,omitempty"`
	Stats      *hoststats.Sample `json:"stats,omitempty"`
}
type PoolNode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Socket      string `json:"socket,omitempty"`
	Address     string `json:"address,omitempty"`
	ServerName  string `json:"serverName,omitempty"`
	CA          string `json:"ca,omitempty"`
	Certificate string `json:"certificate,omitempty"`
	Key         string `json:"key,omitempty"`
	Capacity    int    `json:"capacity"`
	client      *Client
}
type PoolConfig struct {
	Local  string     `json:"local"`
	Routes string     `json:"routes"`
	Nodes  []PoolNode `json:"nodes"`
}
type placement struct {
	Project string `json:"project"`
	Worker  string `json:"worker"`
}
type Pool struct {
	updating    map[string]string
	config      PoolConfig
	mu          sync.Mutex
	probeMu     sync.Mutex
	statusCache []WorkerStatus
	statusAt    time.Time
	routes      map[string]placement
	leases      map[string]bool
	active      map[string]int
}

// Only operator configuration supplies endpoints or certificates. Requests can
// select a registered project/session, never a host address or executable.
func NewPool(config PoolConfig) (*Pool, error) {
	p := &Pool{config: config, routes: map[string]placement{}, leases: map[string]bool{}, active: map[string]int{}}
	ids := map[string]bool{}
	for i := range p.config.Nodes {
		n := &p.config.Nodes[i]
		if !identifier.MatchString(n.ID) || ids[n.ID] || n.Capacity < 1 || n.Capacity > 8 {
			return nil, errors.New("invalid runner configuration")
		}
		ids[n.ID] = true
		n.client = &Client{Socket: n.Socket, Legacy: true}
		if n.ID == config.Local {
			if !filepath.IsAbs(n.Socket) || n.Address != "" {
				return nil, errors.New("local runner requires a Unix socket")
			}
		} else {
			host, _, err := net.SplitHostPort(n.Address)
			if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || n.Socket != "" {
				return nil, errors.New("remote runner must use a loopback tunnel")
			}
			config, err := ClientTLS(n.CA, n.Certificate, n.Key, n.ServerName)
			if err != nil {
				return nil, err
			}
			n.client = &Client{Address: n.Address, TLS: config, Legacy: true}
		}
	}
	if !ids[config.Local] || !filepath.IsAbs(config.Routes) {
		return nil, errors.New("missing local runner or placement store")
	}
	if data, err := os.ReadFile(config.Routes); err == nil {
		if err = json.Unmarshal(data, &p.routes); err != nil || p.routes == nil {
			return nil, errors.New("invalid runner placement store")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for id, r := range p.routes {
		if !identifier.MatchString(id) || !identifier.MatchString(r.Project) || !ids[r.Worker] {
			return nil, errors.New("unknown runner in placement store")
		}
	}
	return p, nil
}

func ClientTLS(ca, certificate, key, name string) (*tls.Config, error) {
	if name == "" {
		return nil, errors.New("runner TLS server name required")
	}
	pool, err := readCA(ca)
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(certificate, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: name, Certificates: []tls.Certificate{pair}}, nil
}
func ServerTLS(ca, certificate, key string) (*tls.Config, error) {
	pool, err := readCA(ca)
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(certificate, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{pair}}, nil
}
func readCA(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("invalid runner CA")
	}
	return pool, nil
}

func (p *Pool) node(id string) *PoolNode {
	for i := range p.config.Nodes {
		if p.config.Nodes[i].ID == id {
			return &p.config.Nodes[i]
		}
	}
	return nil
}
func (p *Pool) statuses(ctx context.Context) []WorkerStatus {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	if time.Since(p.statusAt) < 2*time.Second {
		return p.withUsage(append([]WorkerStatus(nil), p.statusCache...))
	}
	status := make([]WorkerStatus, len(p.config.Nodes))
	var wg sync.WaitGroup
	for i, node := range p.config.Nodes {
		wg.Add(1)
		go func(i int, node PoolNode) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			r, err := node.client.Call(ctx, Request{Operation: "stats"})
			status[i] = WorkerStatus{ID: node.ID, Name: node.Name, Online: err == nil, Capacity: node.Capacity, Revision: r.Revision, Stats: r.Stats, Active: r.ActiveSessions}
		}(i, node)
	}
	wg.Wait()
	localRevision := ""
	for _, s := range status {
		if s.ID == p.config.Local {
			localRevision = s.Revision
		}
	}
	for i := range status {
		s := &status[i]
		s.Compatible = s.ID == p.config.Local || localRevision != "" && localRevision == s.Revision
	}
	p.statusCache, p.statusAt = append([]WorkerStatus(nil), status...), time.Now()
	return p.withUsage(status)
}
func (p *Pool) withUsage(status []WorkerStatus) []WorkerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range status {
		s := &status[i]
		used := p.used(s.ID)
		if used > s.Active {
			s.Active = used
		}
	}
	return status
}
func (p *Pool) used(worker string) int {
	used := 0
	for id, r := range p.routes {
		if r.Worker == worker && (p.leases[id] || p.active[id] > 0) {
			used++
		}
	}
	return used
}
func (p *Pool) savePlacement(id string, route placement) error {
	p.routes[id] = route
	if err := saveRoutes(p.config.Routes, p.routes); err != nil {
		delete(p.routes, id)
		return err
	}
	return nil
}
func saveRoutes(path string, routes map[string]placement) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (p *Pool) Serve(ctx context.Context, listener net.Listener) error {
	go func() { <-ctx.Done(); listener.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	connections := make(chan struct{}, 128)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case connections <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); defer func() { <-connections }(); defer conn.Close(); p.handle(ctx, conn) }()
	}
}
func (p *Pool) handle(parent context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := readLine(reader, 1<<20)
	var request Request
	if err == nil {
		err = json.Unmarshal(line, &request)
	}
	send := func(r Response) {
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_ = json.NewEncoder(conn).Encode(r)
		conn.SetWriteDeadline(time.Time{})
	}
	if err != nil || request.Version != 1 || request.ChatID != "" || request.SandboxID != "" || request.PrincipalID != "" {
		send(Response{Error: "invalid runner pool request"})
		return
	}
	conn.SetReadDeadline(time.Time{})
	if request.Operation == "health" {
		send(Response{Output: "pool"})
		return
	}
	if request.Operation == "stats" {
		statuses := p.statuses(ctx)
		r := Response{Workers: statuses}
		for _, s := range statuses {
			if s.ID == p.config.Local {
				r.Stats = s.Stats
				r.Revision = s.Revision
			}
			r.ActiveSessions += s.Active
			if s.Online && s.Compatible {
				r.SessionLimit += s.Capacity
			}
		}
		send(r)
		return
	}
	if request.Operation == "update" {
		status := p.statuses(ctx)
		p.mu.Lock()
		available := false
		for _, s := range status {
			if s.ID == request.SessionID && s.ID != p.config.Local && s.Revision != request.Expected && commit.MatchString(request.Expected) && p.used(s.ID) == 0 && s.Active == 0 {
				if p.updating == nil {
					p.updating = map[string]string{}
				}
				p.updating[s.ID] = request.Expected
				available = true
			}
		}
		p.mu.Unlock()
		send(Response{Available: available})
		return
	}
	if !identifier.MatchString(request.ProjectID) {
		send(Response{Error: "invalid project ID"})
		return
	}
	if request.Operation == "register" || request.Operation == "refresh" {
		r, err := p.node(p.config.Local).client.Call(ctx, request)
		if err != nil {
			r = Response{Error: err.Error()}
		}
		send(r)
		return
	}
	if !identifier.MatchString(request.SessionID) {
		send(Response{Error: "invalid session ID"})
		return
	}
	if request.Operation == "lease" {
		p.lease(ctx, reader, request, send)
		return
	}
	p.mu.Lock()
	route, ok := p.routes[request.SessionID]
	if !ok {
		route = placement{Project: request.ProjectID, Worker: p.config.Local}
		err = p.savePlacement(request.SessionID, route)
	}
	if err == nil && route.Project != request.ProjectID {
		err = errors.New("session belongs to another project")
	}
	if err == nil {
		p.active[request.SessionID]++
	}
	p.mu.Unlock()
	if err != nil {
		send(Response{Error: err.Error()})
		return
	}
	defer func() {
		p.mu.Lock()
		p.active[request.SessionID]--
		if p.active[request.SessionID] == 0 {
			delete(p.active, request.SessionID)
		}
		p.mu.Unlock()
	}()
	node := p.node(route.Worker)
	if request.Operation == "prepare" && route.Worker != p.config.Local && request.Repository != "" {
		_, err = node.client.Call(ctx, Request{Operation: "refresh", ProjectID: request.ProjectID, Repository: request.Repository, Token: request.Token, Args: request.Args})
		if err != nil {
			send(Response{Error: "Could not prepare the Mac runner repository: " + err.Error()})
			return
		}
	}
	if request.Operation == "import" {
		p.mu.Lock()
		source, exists := p.routes[request.SourceSessionID]
		p.mu.Unlock()
		if !exists {
			source = placement{Project: request.ProjectID, Worker: p.config.Local}
		}
		if source.Project != request.ProjectID {
			send(Response{Error: "source belongs to another project"})
			return
		}
		if source.Worker != route.Worker {
			exported, err := p.node(source.Worker).client.Call(ctx, Request{Operation: "export", ProjectID: request.ProjectID, SessionID: request.SourceSessionID})
			if err != nil || exported.Head != request.Expected || len(exported.Bundle) == 0 || len(exported.Bundle) > 64<<20 {
				send(Response{Error: "source snapshot changed or its runner is offline"})
				return
			}
			request.Operation = "import-bundle"
			request.BundleSize = int64(len(exported.Bundle))
			result, err := node.client.transfer(ctx, request, exported.Bundle)
			if err != nil {
				result = Response{Error: err.Error()}
			}
			send(result)
			return
		}
	}
	upstream, result, err := node.client.Open(ctx, request)
	if err != nil {
		send(Response{Error: fmt.Sprintf("%s runner unavailable: %s", node.Name, err)})
		return
	}
	defer upstream.Close()
	if request.Operation != "stream" {
		send(result)
		return
	}
	send(result)
	go func() { _, _ = io.Copy(upstream, reader); cancel() }()
	_, _ = io.Copy(conn, upstream)
}

func (p *Pool) lease(ctx context.Context, reader *bufio.Reader, r Request, send func(Response)) {
	status := p.statuses(ctx)
	// Prefer the additional machine for a new session, leaving OVH available
	// for existing conversations. Placement never changes when a runner sleeps.
	sort.SliceStable(status, func(i, j int) bool { return status[i].ID != p.config.Local && status[j].ID == p.config.Local })
	p.mu.Lock()
	route, exists := p.routes[r.SessionID]
	if exists && route.Project != r.ProjectID {
		p.mu.Unlock()
		send(Response{Error: "session belongs to another project"})
		return
	}
	if p.leases[r.SessionID] {
		p.mu.Unlock()
		send(Response{})
		return
	}
	selected := ""
	for _, s := range status {
		if exists && s.ID != route.Worker || !exists && !r.NewSession && s.ID != p.config.Local {
			continue
		}
		used := p.used(s.ID)
		if s.Active > used {
			used = s.Active
		}
		if target := p.updating[s.ID]; target != "" {
			if s.Revision != target {
				continue
			}
			delete(p.updating, s.ID)
		}
		if s.Online && s.Compatible && used < s.Capacity {
			selected = s.ID
			break
		}
	}
	if selected == "" {
		p.mu.Unlock()
		send(Response{})
		return
	}
	if !exists {
		if err := p.savePlacement(r.SessionID, placement{Project: r.ProjectID, Worker: selected}); err != nil {
			p.mu.Unlock()
			send(Response{Error: err.Error()})
			return
		}
	}
	p.leases[r.SessionID] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.leases, r.SessionID); p.mu.Unlock() }()
	send(Response{Available: true, RunnerID: selected})
	// Keeping this private connection open reserves capacity for the complete
	// task, including gaps between prepare, fetch, and app-server commands.
	_, _ = reader.ReadByte()
}

func (c *Client) transfer(ctx context.Context, r Request, data []byte) (Response, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(5 * time.Minute))
	r.Version = 1
	if err = json.NewEncoder(conn).Encode(r); err == nil {
		_, err = io.Copy(conn, bytes.NewReader(data))
	}
	if err != nil {
		return Response{}, err
	}
	line, err := readLine(bufio.NewReader(conn), 1<<20)
	var result Response
	if err == nil {
		err = json.Unmarshal(line, &result)
	}
	if err == nil && result.Error != "" {
		err = errors.New(result.Error)
	}
	return result, err
}
