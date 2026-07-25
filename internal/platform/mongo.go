// Package platform holds cross-cutting infra: the Mongo client, config
// loading, the WS hub, and logging. Domain packages (ledger, market,
// stake, settle, circle, user) depend on platform; platform depends on
// nothing under internal/.
package platform

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// MongoConfig configures the connection to the local single-node replica
// set. Multi-document transactions (required by ledger.Transfer) only work
// against a replica set / mongos, never a standalone mongod.
type MongoConfig struct {
	URI      string
	Database string
}

// ConnectMongo dials Mongo and confirms it is reachable and running as a
// replica set member (Transfer will fail at call time, not connect time,
// against a standalone node, so we fail fast here instead).
func ConnectMongo(ctx context.Context, cfg MongoConfig) (*mongo.Client, *mongo.Database, error) {
	clientOpts := options.Client().
		ApplyURI(cfg.URI).
		SetReadConcern(readconcern.Majority()).
		SetWriteConcern(writeconcern.Majority())

	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("platform: mongo connect: %w", err)
	}
	if err := client.Ping(connectCtx, nil); err != nil {
		return nil, nil, fmt.Errorf("platform: mongo ping: %w", err)
	}

	db := client.Database(cfg.Database)
	return client, db, nil
}
