// Package database provides the shared Mongo handle for yeshara-backend.
package database

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"yeshara.network/marketplace/internal/config"
)

var (
	client   *mongo.Client
	db       *mongo.Database
	initOnce sync.Once
	initErr  error
)

// Init connects to MongoDB once per process.
func Init(cfg *config.Config) error {
	initOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		c, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.MongoURI))
		if err != nil {
			initErr = fmt.Errorf("mongo connect: %w", err)
			return
		}
		if err := c.Ping(ctx, nil); err != nil {
			initErr = fmt.Errorf("mongo ping: %w", err)
			return
		}
		client = c
		db = c.Database(cfg.MongoDB)
	})
	return initErr
}

// GetDB returns the connected database, or nil when not initialised.
func GetDB() *mongo.Database {
	return db
}

// Close disconnects cleanly on shutdown.
func Close(ctx context.Context) error {
	if client == nil {
		return nil
	}
	return client.Disconnect(ctx)
}
