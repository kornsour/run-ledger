// Command runledger serves the experiment run ledger over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kornsour/run-ledger/internal/api"
	"github.com/kornsour/run-ledger/internal/store"
)

func main() {
	addr := flag.String("addr", envOr("RUNLEDGER_ADDR", ":8080"), "listen address")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// The in-memory store is the only backend today, so a clone runs with no
	// external dependency. A durable backend is issue #1.
	st := store.NewMemory()
	defer func() { _ = st.Close() }()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(st, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", *addr, "store", "memory")
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
