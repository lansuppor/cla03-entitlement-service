package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lansolocoder/cla03-entitlement-service/internal/httpapi"
	"github.com/lansolocoder/cla03-entitlement-service/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatal("invalid database configuration")
	}
	defer pool.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	if err := pool.Ping(pingCtx); err != nil {
		cancelPing()
		log.Fatal("cannot reach database: ", err)
	}
	cancelPing()

	entitlementStore := store.New(pool)
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 30*time.Second)
	if err := entitlementStore.Migrate(migrateCtx); err != nil {
		cancelMigrate()
		log.Fatal("database migration failed: ", err)
	}
	cancelMigrate()

	address := os.Getenv("LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	server := &http.Server{
		Addr: address, Handler: httpapi.New(pool.Ping, entitlementStore),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.ListenAndServe() }()
	select {
	case err := <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("HTTP listener failed: ", err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}
}
