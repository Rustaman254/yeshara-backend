// yeshara-marketplace: the primary-market backend for Yeshara. A tenant of
// comet-engine, not a blockchain integration in its own right — every
// mint/transfer/balance call routes through comet-engine's receipt
// tokenization API.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"yeshara.network/marketplace/internal/api"
	"yeshara.network/marketplace/internal/config"
	"yeshara.network/marketplace/internal/database"
	"yeshara.network/marketplace/internal/distributions"
	"yeshara.network/marketplace/internal/investors"
	"yeshara.network/marketplace/internal/offerings"
	"yeshara.network/marketplace/internal/owners"
	"yeshara.network/marketplace/internal/redemptions"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := database.Init(cfg); err != nil {
		log.Fatalf("mongo: %v", err)
	}
	log.Println("mongo connected")

	if err := investors.EnsureIndexes(ctx); err != nil {
		log.Fatalf("investor indexes: %v", err)
	}
	if err := offerings.EnsureIndexes(ctx); err != nil {
		log.Fatalf("offering indexes: %v", err)
	}
	if err := distributions.EnsureIndexes(ctx); err != nil {
		log.Fatalf("distribution indexes: %v", err)
	}
	if err := redemptions.EnsureIndexes(ctx); err != nil {
		log.Fatalf("redemption indexes: %v", err)
	}
	if err := owners.EnsureIndexes(ctx); err != nil {
		log.Fatalf("owner indexes: %v", err)
	}

	srv := api.NewServer(cfg)
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: srv.Router()}

	go func() {
		log.Printf("yeshara-marketplace listening on :%s", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stopCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-stopCtx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = database.Close(shutdownCtx)
}
