// Package edgesvc is `warden edge`, the authenticating ingress and preview host.
package edgesvc

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"warden/chat/internal/edge"
	"warden/chat/internal/handshake"
	"warden/chat/internal/services"
	"warden/chat/internal/transport"
)

// Main runs the edge and returns the exit status.
func Main(args []string) int { return services.Run(run, args) }

func run(args []string) error {
	fs := flag.NewFlagSet("warden edge", flag.ContinueOnError)
	file := fs.String("config", "", "warden.json (default $WARDEN_CONFIG) or the original private edge JSON config")
	version := fs.Bool("version", false, "print the build revision and protocol number")
	if err := services.ParseFlags(fs, args); err != nil {
		return err
	}
	if *version {
		fmt.Println(handshake.Self("warden-edge"))
		return nil
	}
	c, err := loadEdgeConfig(*file)
	if err != nil {
		return err
	}
	handler, err := edge.New(c)
	if err != nil {
		return err
	}
	// In owner mode over a tls:// upstream the edge holds the owner
	// capability (docs/warden-kubernetes-plan.md, step 6): a fresh one at
	// every start, as the chat rotates its own, kept in the edge's state
	// directory and announced in the log, the one place it can be read from.
	if handler.MintsOwnerCapability() {
		if _, err = handler.RotateOwnerCapability(time.Now()); err != nil {
			return err
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go handler.Run(ctx)
	server := &http.Server{Addr: c.Listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: transport.ProbeQuietLog()}
	go func() {
		<-ctx.Done()
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		server.Shutdown(c)
	}()
	log.Printf("Warden ingress listening on %s", c.Listen)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
