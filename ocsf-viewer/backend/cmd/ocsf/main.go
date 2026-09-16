package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/punished-monaddle/warden/ocsf-viewer/backend/internal/browserauth"
	"github.com/punished-monaddle/warden/ocsf-viewer/backend/internal/pipeline"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func handler(publicDir, revision string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "revision": revision})
	})
	files := http.FileServer(http.Dir(publicDir))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Serve exported routes, never a directory listing or an SPA fallback
		// for missing assets/API routes.
		if strings.Contains(r.URL.Path, "/.") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		mux.ServeHTTP(w, r)
	})
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	appHandler := handler(env("PUBLIC_DIR", "dist/client"), env("APP_REVISION", "development"))
	browserAuth, err := browserauth.New(browserauth.Config{ClientID: os.Getenv("GOOGLE_CLIENT_ID"), Origin: os.Getenv("OCSF_AUTH_ORIGIN"), AdminEmails: os.Getenv("OCSF_GOOGLE_ADMIN_EMAILS"), ReadEmails: os.Getenv("OCSF_GOOGLE_READ_EMAILS"), ReadDomains: os.Getenv("OCSF_GOOGLE_READ_DOMAINS")})
	if err != nil {
		slog.Error("browser authentication configuration invalid", "error", err)
		os.Exit(1)
	}
	authMux := http.NewServeMux()
	authMux.Handle("/auth/", browserAuth.Handler())
	authMux.Handle("/", appHandler)
	appHandler = authMux
	var ingestion *pipeline.API
	var workerDone chan struct{}
	if os.Getenv("CLICKHOUSE_URL") != "" {
		capacity, capacityErr := strconv.ParseInt(env("OCSF_QUEUE_BYTES", "536870912"), 10, 64)
		if capacityErr != nil || capacity <= 0 {
			slog.Error("invalid OCSF_QUEUE_BYTES")
			os.Exit(1)
		}
		var err error
		ingestion, err = pipeline.New(pipeline.Config{Path: env("OCSF_QUEUE_PATH", "data/queue.db"), URL: os.Getenv("CLICKHOUSE_URL"), User: env("CLICKHOUSE_USER", "ocsf"), Password: os.Getenv("CLICKHOUSE_PASSWORD"), AdminToken: os.Getenv("OCSF_ADMIN_TOKEN"), ReadToken: os.Getenv("OCSF_READ_TOKEN"), IngestToken: os.Getenv("OCSF_INGEST_TOKEN"), MaxBytes: capacity, BrowserRole: browserAuth.Role})
		if err != nil {
			slog.Error("ingestion startup failed", "error", err)
			os.Exit(1)
		}
		defer ingestion.Store.Close()
		mux := http.NewServeMux()
		mux.Handle("/api/", ingestion)
		mux.HandleFunc("GET /readyz", ingestion.Ready)
		mux.Handle("/", appHandler)
		appHandler = mux
		workerDone = make(chan struct{})
		go func() { defer close(workerDone); ingestion.Run(ctx) }()
	}
	server := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8080"),
		Handler:           appHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()
	slog.Info("listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}

	stop()
	<-shutdownDone
	if workerDone != nil {
		stop()
		<-workerDone
	}
}
