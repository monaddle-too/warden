// Package edgesvc is `warden edge`, the authenticating ingress and preview host.
package edgesvc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"warden/chat/internal/edge"
	"warden/chat/internal/handshake"
	"warden/chat/internal/services"
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
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(host) == nil || (!net.ParseIP(host).IsLoopback() && !net.ParseIP(host).IsPrivate()) {
		return errors.New("edge listener must be a private or loopback IP")
	}
	handler, err := edge.New(c)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go handler.Run(ctx)
	server := &http.Server{Addr: c.Listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
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
