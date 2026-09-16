package main

import (
	"context"
	"encoding/json"
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
	"warden/chat/internal/chats"
	"warden/chat/internal/conversation"
	"warden/chat/internal/handshake"
	"warden/chat/internal/imageguard"
	"warden/chat/internal/sandbox"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--normalize-image" {
		imageguard.Main()
		return
	}
	configPath := flag.String("config", "", "optional warden.json (default $WARDEN_CONFIG); flags override its computed defaults and must agree with its values")
	wardenSocket := flag.String("warden-socket", "", "Private Warden sharing/broker socket (paths.state/policy/sbx-control.sock)")
	root := flag.String("state", "", "Private persistent Warden chat state directory (paths.state/app)")
	socket := flag.String("runner-socket", "", "Warden runner Unix socket (paths.state/runner/worker.sock)")
	listen := flag.String("listen", "127.0.0.1:18780", "Loopback chat address (chat.listen)")
	web := flag.String("web-dir", "chat/web/dist", "Built Warden chat assets (paths.webAssets)")
	suffix := flag.String("preview-suffix", "", "Authenticated preview hostname suffix (previews.hostSuffix); empty leaves external previews unconfigured")
	version := flag.Bool("version", false, "print the build revision and protocol number")
	flag.Parse()
	if *version {
		fmt.Println(handshake.Self("warden-chat"))
		return
	}
	s, err := resolveSettings(flag.CommandLine, chatFlags{configPath: configPath, state: root, wardenSocket: wardenSocket, runnerSocket: socket, listen: listen, web: web, suffix: suffix})
	if err != nil {
		log.Fatal(err)
	}
	root, socket, wardenSocket, listen, web, suffix = &s.state, &s.runnerSocket, &s.wardenSocket, &s.listen, &s.web, &s.suffix
	if *root == "" || *socket == "" {
		log.Fatal("--state and --runner-socket are required")
	}
	if err := chats.ValidatePreviewSuffix(*suffix); err != nil {
		log.Fatal(err)
	}
	host, port, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("listen must be an IP loopback address")
	}
	if strings.HasPrefix(host, "::") {
		log.Fatal("use IPv4 loopback")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Version handshake before anything is served or the endpoint file is
	// written: a runner or policy service on another protocol is a refusal
	// naming both revisions; another revision on the same protocol is a
	// warning (one service at a time is how OVH updates).
	self := handshake.Self("warden-chat")
	peers, err := handshake.Verify(ctx, self, *socket, *wardenSocket, handshake.Options{Wait: handshakeWait, Warn: func(s string) { log.Print("warning: ", s) }})
	if err != nil {
		log.Fatal(err)
	}
	log.Print(self, peerSummary(peers))
	store, err := chats.Open(*root)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	// Rotate the private owner capability on each start. It is never given to SBX.
	token := conversation.ID() + conversation.ID()
	origin := "http://" + net.JoinHostPort(host, port)
	endpoint := map[string]string{"url": origin, "token": token}
	b, _ := json.Marshal(endpoint)
	endpointPath := filepath.Join(*root, "endpoint.json")
	if err = os.WriteFile(endpointPath, b, 0600); err != nil {
		log.Fatal(err)
	}
	if err = os.Chmod(endpointPath, 0600); err != nil {
		log.Fatal(err)
	}
	engine := chats.NewEngine(store, &sandbox.Client{Socket: *socket})
	engine.PublicPreviewSuffix = *suffix
	engine.PreviewScheme, engine.PreviewPort = s.previewScheme, s.previewPort
	engine.WardenSocket = *wardenSocket
	go engine.Serve(ctx)
	defer func() { cancel(); <-engine.Done() }()
	server := &http.Server{Addr: *listen, Handler: &chats.HTTP{Engine: engine, Token: token, Host: net.JoinHostPort(host, port), Origin: origin, WebDir: *web}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		server.Shutdown(shutdown)
	}()
	fmt.Printf("Warden chat listening at %s; private launcher: %s\n", origin, endpointPath)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
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
