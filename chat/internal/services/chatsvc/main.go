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
	"warden/chat/internal/chats"
	"warden/chat/internal/conversation"
	"warden/chat/internal/handshake"
	"warden/chat/internal/imageguard"
	"warden/chat/internal/sandbox"
	"warden/chat/internal/services"
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
	wardenSocket := fs.String("warden-socket", "", "Private Warden sharing/broker socket (paths.state/policy/sbx-control.sock)")
	root := fs.String("state", "", "Private persistent Warden chat state directory (paths.state/app)")
	socket := fs.String("runner-socket", "", "Warden runner Unix socket (paths.state/runner/worker.sock)")
	listen := fs.String("listen", "127.0.0.1:18780", "Loopback chat address (chat.listen)")
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
	root, socket, wardenSocket, listen, web, suffix = &s.state, &s.runnerSocket, &s.wardenSocket, &s.listen, &s.web, &s.suffix
	if *root == "" || *socket == "" {
		return errors.New("--state and --runner-socket are required")
	}
	if err := chats.ValidatePreviewSuffix(*suffix); err != nil {
		return err
	}
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
	peers, err := handshake.Verify(ctx, self, *socket, *wardenSocket, handshake.Options{Wait: handshakeWait, Warn: func(s string) { log.Print("warning: ", s) }})
	if err != nil {
		return err
	}
	log.Print(self, peerSummary(peers))
	store, err := chats.Open(*root)
	if err != nil {
		return err
	}
	defer store.Close()
	// Rotate the private owner capability on each start. It is never given to SBX.
	token := conversation.ID() + conversation.ID()
	origin := "http://" + net.JoinHostPort(host, port)
	endpoint := map[string]string{"url": origin, "token": token}
	b, _ := json.Marshal(endpoint)
	endpointPath := filepath.Join(*root, "endpoint.json")
	if err = os.WriteFile(endpointPath, b, 0600); err != nil {
		return err
	}
	if err = os.Chmod(endpointPath, 0600); err != nil {
		return err
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
