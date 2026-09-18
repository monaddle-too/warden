// Package chatsvc is `warden serve`, the chat HTTP API and web UI.
package chatsvc

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"warden/chat/internal/bugreport"
	"warden/chat/internal/chats"
	"warden/chat/internal/config"
	"warden/chat/internal/conversation"
	"warden/chat/internal/handshake"
	"warden/chat/internal/imageguard"
	"warden/chat/internal/sandbox"
	"warden/chat/internal/services"
	"warden/chat/internal/transport"
)

// Main runs the chat service and returns the exit status.
func Main(args []string) int { return services.Run(run, args) }

func run(args []string) error {
	if len(args) == 1 && args[0] == "--normalize-image" {
		imageguard.Main()
		return nil
	}
	fs := flag.NewFlagSet("warden serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional warden.json (default $WARDEN_CONFIG); flags override its computed defaults and must agree with its values")
	wardenSocket := fs.String("warden-socket", "", "Private Warden sharing/broker socket (services.policy.address, default paths.state/policy/sbx-control.sock)")
	root := fs.String("state", "", "Private persistent Warden chat state directory (paths.state/app)")
	socket := fs.String("runner-socket", "", "Warden runner Unix socket (services.runner.address, default paths.state/runner/worker.sock)")
	listen := fs.String("listen", "127.0.0.1:18780", "Loopback chat address (chat.listen; services.chat.listen may be tls:// instead)")
	web := fs.String("web-dir", "chat/web/dist", "Built Warden chat assets (paths.webAssets)")
	suffix := fs.String("preview-suffix", "", "Authenticated preview hostname suffix (previews.hostSuffix); empty leaves external previews unconfigured")
	version := fs.Bool("version", false, "print the build revision and protocol number")
	if err := services.ParseFlags(fs, args); err != nil {
		return err
	}
	if *version {
		fmt.Println(handshake.Self("warden-chat"))
		return nil
	}
	s, err := resolveSettings(fs, chatFlags{configPath: configPath, state: root, wardenSocket: wardenSocket, runnerSocket: socket, listen: listen, web: web, suffix: suffix})
	if err != nil {
		return err
	}
	root, listen, web, suffix = &s.state, &s.listen, &s.web, &s.suffix
	if *root == "" || s.runner == "" {
		return errors.New("--state and --runner-socket are required")
	}
	if err := chats.ValidatePreviewSuffix(*suffix); err != nil {
		return err
	}
	// The listener is either today's loopback HTTP server, reached by the
	// edge with the bearer capability from endpoint.json, or a mutual-TLS
	// server that admits only the edge's certificate (docs/warden-kubernetes-plan.md,
	// decision 5). Everything the edge forwards is the same either way.
	mutual := transport.IsTLS(s.listenURL)
	host, port, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("listen must be an IP loopback address")
	}
	if strings.HasPrefix(host, "::") {
		return errors.New("use IPv4 loopback")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Version handshake before anything is served or the endpoint file is
	// written: a runner or policy service on another protocol is a refusal
	// naming both revisions; another revision on the same protocol is a
	// warning (one service at a time is how OVH updates).
	self := handshake.Self("warden-chat")
	peers, err := handshake.Verify(ctx, self, s.runner, s.policy, handshake.Options{Wait: handshakeWait, TLS: s.tls, Warn: func(s string) { log.Print("warning: ", s) }})
	if err != nil {
		return err
	}
	log.Print(self, peerSummary(peers))
	store, err := chats.Open(*root)
	if err != nil {
		return err
	}
	defer store.Close()
	handler := &chats.HTTP{Host: net.JoinHostPort(host, port), Origin: "http://" + net.JoinHostPort(host, port), WebDir: *web}
	endpointPath := ""
	if mutual {
		// The edge's certificate is its authority; no capability exists and
		// no endpoint file is written.
		handler.Peer = transport.Edge
		handler.Host = config.HostOf(s.address)
		handler.Origin = "https://" + handler.Host
	} else {
		// Rotate the private owner capability on each start. It is never given to SBX.
		handler.Token = conversation.ID() + conversation.ID()
		endpoint := map[string]string{"url": handler.Origin, "token": handler.Token}
		b, _ := json.Marshal(endpoint)
		endpointPath = filepath.Join(*root, "endpoint.json")
		if err = os.WriteFile(endpointPath, b, 0600); err != nil {
			return err
		}
		if err = os.Chmod(endpointPath, 0600); err != nil {
			return err
		}
	}
	engine := chats.NewEngine(store, &sandbox.Client{Address: s.runner, TLS: s.tls})
	engine.PublicPreviewSuffix = *suffix
	engine.PreviewScheme, engine.PreviewPort = s.previewScheme, s.previewPort
	engine.PolicyAddress, engine.PolicyTLS = s.policy, s.tls
	// The runner's shared preview server on Kubernetes (services.runner.
	// previews.address, a tls:// URL the ports proxy dials as https:// with
	// this service's certificate); "" keeps the loopback attachment URLs.
	engine.RunnerPreviewHost, engine.RunnerPreviewTLS = config.HostOf(s.runnerPreviews), s.tls
	// LocalMode offers the owner's own directories to an agent: a
	// single-owner install on a machine with such directories, which the
	// Kubernetes shape is not (the runner is a pod).
	engine.LocalMode = s.cfg.Auth.Mode == config.AuthOwner && s.cfg.RuntimeKind() != config.RuntimeKubernetes
	engine.DefaultModels = map[string]string{}
	if claude := s.cfg.Providers.Claude; claude != nil {
		engine.AllowFastMode, engine.AllowLongContext = claude.AllowFastMode, claude.AllowLongContext
		engine.DefaultModels["claude"] = claude.DefaultModel
	}
	if codex := s.cfg.Providers.Codex; codex != nil {
		engine.DefaultModels["codex"] = codex.DefaultModel
	}
	handler.Engine = engine
	// Bug reports (docs/bug-reporting-plan.md): a recovered panic in a
	// handler or a run goroutine is drafted; the capability is one of the
	// values the redaction removes.
	bugs := bugreport.New(s.cfg, s.configPath, bugreport.ComponentChat, handler.Token)
	bugreport.SetDefault(bugs)
	engine.Bugs = bugs
	go engine.Serve(ctx)
	defer func() { cancel(); <-engine.Done() }()
	server := &http.Server{Addr: *listen, Handler: bugs.Handler(handler), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: transport.ProbeQuietLog()}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		server.Shutdown(shutdown)
	}()
	if mutual {
		l, err := transport.Listen(s.listenURL, transport.ListenOptions{TLS: s.tls, Peers: []string{transport.Edge}})
		if err != nil {
			return err
		}
		fmt.Printf("Warden chat listening at %s as %s; admitting %s\n", s.listenURL, handler.Host, transport.Edge)
		err = server.Serve(l)
	} else {
		fmt.Printf("Warden chat listening at %s; private launcher: %s\n", handler.Origin, endpointPath)
		err = server.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// handshakeWait is how long the runner and policy sockets are retried at
// startup; Compose starts the three services together and the policy
// service prepares its gateway CA before it listens.
const handshakeWait = 30 * time.Second

func peerSummary(peers []handshake.Peer) string {
	var b strings.Builder
	for _, p := range peers {
		b.WriteString("; ")
		b.WriteString(p.String())
	}
	return b.String()
}
