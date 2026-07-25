package ledger

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Discrepancy is one account whose cached Balance disagrees with the
// signed sum of its entries.
type Discrepancy struct {
	AccountID       bson.ObjectID `bson:"account_id"`
	OwnerType       OwnerType     `bson:"owner_type"`
	OwnerID         string        `bson:"owner_id"`
	Currency        string        `bson:"currency"`
	CachedBalance   int64         `bson:"cached_balance"`
	ComputedBalance int64         `bson:"computed_balance"`
}

// Reconcile scans every account and reports any whose cached Balance does
// not equal the signed sum of its entries. Because Transfer only ever
// mutates an account's balance and appends its entries inside the same
// Mongo transaction, the two cannot drift under normal operation — a
// non-empty result here means something wrote to accounts or entries
// outside of Transfer/Grant, or a prior bug left the collections
// inconsistent. Intended to be run on a schedule (cron/health check), not
// on any request path — it's a full collection scan.
func (l *Ledger) Reconcile(ctx context.Context) ([]Discrepancy, error) {
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$lookup", Value: bson.M{
			"from": EntriesCollection,
			"let":  bson.M{"acctID": "$_id"},
			"pipeline": mongo.Pipeline{
				bson.D{{Key: "$match", Value: bson.M{
					"$expr": bson.M{"$eq": bson.A{"$account_id", "$$acctID"}},
				}}},
				bson.D{{Key: "$group", Value: bson.M{
					"_id": nil,
					"sum": bson.M{"$sum": bson.M{"$cond": bson.A{
						bson.M{"$eq": bson.A{"$direction", string(Credit)}},
						"$amount",
						bson.M{"$multiply": bson.A{"$amount", -1}},
					}}},
				}}},
			},
			"as": "entry_sum",
		}}},
		bson.D{{Key: "$addFields", Value: bson.M{
			"computed_balance": bson.M{"$ifNull": bson.A{
				bson.M{"$arrayElemAt": bson.A{"$entry_sum.sum", 0}}, int64(0),
			}},
		}}},
		bson.D{{Key: "$match", Value: bson.M{
			"$expr": bson.M{"$ne": bson.A{"$balance", "$computed_balance"}},
		}}},
		bson.D{{Key: "$project", Value: bson.M{
			"_id":              0,
			"account_id":       "$_id",
			"owner_type":       1,
			"owner_id":         1,
			"currency":         1,
			"cached_balance":   "$balance",
			"computed_balance": 1,
		}}},
	}

	cur, err := l.accounts.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("ledger: reconcile: %w", err)
	}
	defer cur.Close(ctx)

	var out []Discrepancy
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("ledger: reconcile: %w", err)
	}
	return out, nil
}
