package ledger

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	AccountsCollection = "accounts"
	EntriesCollection  = "entries"
)

// EnsureIndexes creates the indexes the ledger package relies on for
// correctness (not just performance):
//   - accounts: unique (owner_type, owner_id, currency) — one account per
//     owner per currency.
//   - entries: unique (txn_id, account_id) — this is what makes Transfer
//     idempotent on txnID replay, and what makes entries append-only safe
//     under concurrent retries.
//   - entries: (market_id) — non-unique, for settle/stake queries in later
//     phases.
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	accounts := db.Collection(AccountsCollection)
	_, err := accounts.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "owner_type", Value: 1}, {Key: "owner_id", Value: 1}, {Key: "currency", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uniq_owner_currency"),
	})
	if err != nil {
		return err
	}

	entries := db.Collection(EntriesCollection)
	_, err = entries.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "txn_id", Value: 1}, {Key: "account_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uniq_txn_account"),
	})
	if err != nil {
		return err
	}

	_, err = entries.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "market_id", Value: 1}},
		Options: options.Index().SetName("by_market").SetSparse(true),
	})
	return err
}
