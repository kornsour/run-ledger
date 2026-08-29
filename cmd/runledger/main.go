// Command runledger serves the experiment run ledger over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kornsour/run-ledger/internal/api"
	"github.com/kornsour/run-ledger/internal/store"
)

func main() {
	addr := flag.String("addr", envOr("RUNLEDGER_ADDR", ":8080"), "listen address")
	tokenFile := flag.String("token-file", "", "file containing the bearer token that grants read and write access (overrides RUNLEDGER_TOKEN)")
	readTokenFile := flag.String("read-token-file", "", "file containing the bearer token that grants read-only access (overrides RUNLEDGER_READ_TOKEN)")
	storeKind := flag.String("store", envOr("RUNLEDGER_STORE", "memory"), "storage backend: memory or duckdb")
	dsn := flag.String("dsn", envOr("RUNLEDGER_DSN", ""), "backend DSN (duckdb: a database file path; empty keeps everything in process memory, gone on restart either way)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	writeToken, err := loadToken(*tokenFile, "RUNLEDGER_TOKEN")
	if err != nil {
		log.Error("loading token-file", "err", err)
		os.Exit(1)
	}
	readToken, err := loadToken(*readTokenFile, "RUNLEDGER_READ_TOKEN")
	if err != nil {
		log.Error("loading read-token-file", "err", err)
		os.Exit(1)
	}
	auth := api.Auth{WriteToken: writeToken, ReadToken: readToken}

	st, err := openStore(*storeKind, *dsn)
	if err != nil {
		log.Error("opening store", "store", *storeKind, "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(st, log, api.WithAuth(auth)).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// Nothing this API does should legitimately take long -- without
		// these, a slow or stalled client holds a connection (and its
		// goroutine) open indefinitely.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", *addr, "store", *storeKind)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
		os.Exit(1)
	}
}

// openStore returns the configured backend. "memory" (the default) needs no
// DSN and nothing external, so a clone still runs with `make build &&
// ./bin/runledger` and no setup. "duckdb" is durable and queryable but
// requires cgo at build time -- see
// docs/adr/0006-duckdb-store-backend-and-the-cgo-cost.md.
func openStore(kind, dsn string) (store.Store, error) {
	switch kind {
	case "", "memory":
		return store.NewMemory(), nil
	case "duckdb":
		return store.NewDuckDB(dsn)
	default:
		return nil, fmt.Errorf("unknown store %q: want memory or duckdb", kind)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadToken returns the token from file if set, otherwise from the named
// environment variable. A file's contents are trimmed of surrounding
// whitespace so a trailing newline from an editor or `echo` does not become
// part of the token.
func loadToken(file, envKey string) (string, error) {
	if file == "" {
		return os.Getenv(envKey), nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", file, err)
	}
	return strings.TrimSpace(string(b)), nil
}
