package main

import (
	"context"
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
)

func main() {
	file := flag.String("config", "", "warden.json (default $WARDEN_CONFIG) or the original private edge JSON config")
	version := flag.Bool("version", false, "print the build revision and protocol number")
	flag.Parse()
	if *version {
		fmt.Println(handshake.Self("warden-edge"))
		return
	}
	c, err := loadEdgeConfig(*file)
	if err != nil {
		log.Fatal(err)
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(host) == nil || (!net.ParseIP(host).IsLoopback() && !net.ParseIP(host).IsPrivate()) {
		log.Fatal("edge listener must be a private or loopback IP")
	}
	handler, err := edge.New(c)
	if err != nil {
		log.Fatal(err)
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
		log.Fatal(err)
	}
}
