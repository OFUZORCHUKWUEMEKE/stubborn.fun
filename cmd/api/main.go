// Command api is the Stubborn.fun HTTP entrypoint. It currently boots the
// datastore, ensures indexes, and runs the market auto-close scheduler;
// route handlers land in Phase 5.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/ledger"
	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/market"
	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/platform"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := platform.LoadConfig()

	client, db, err := platform.ConnectMongo(ctx, cfg.Mongo)
	if err != nil {
		log.Fatalf("mongo: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(shutdownCtx)
	}()

	// Indexes are a correctness dependency, not just a performance one:
	// the ledger's idempotency rests on a unique index, and the
	// scheduler's poll would otherwise be a collection scan.
	if err := ledger.EnsureIndexes(ctx, db); err != nil {
		log.Fatalf("ledger indexes: %v", err)
	}
	if err := market.EnsureIndexes(ctx, db); err != nil {
		log.Fatalf("market indexes: %v", err)
	}

	_ = ledger.New(client, db)

	clock := platform.SystemClock{}
	markets := market.NewStore(db, clock)

	// In-process auto-close scheduler. Safe to run on several instances:
	// each transition is a compare-and-swap, so only one closer wins.
	scheduler := market.NewScheduler(markets, clock, market.DefaultTickInterval, nil)
	go scheduler.Run(ctx)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("stubborn api listening on %s", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
}
