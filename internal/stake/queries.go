package stake

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// PositionsByUser returns a user's positions, newest first. Backs the
// wallet/bet history view.
func (s *Service) PositionsByUser(ctx context.Context, userID string, limit int64) ([]Position, error) {
	return s.find(ctx, bson.M{"user_id": userID}, limit)
}

// PositionsByMarket returns a market's positions, newest first. Backs the
// "who's in" participant list.
func (s *Service) PositionsByMarket(ctx context.Context, marketID bson.ObjectID, limit int64) ([]Position, error) {
	return s.find(ctx, bson.M{"market_id": marketID}, limit)
}

// PositionsByUserInMarket returns one user's positions in one market.
func (s *Service) PositionsByUserInMarket(ctx context.Context, marketID bson.ObjectID, userID string) ([]Position, error) {
	return s.find(ctx, bson.M{"market_id": marketID, "user_id": userID}, 0)
}

func (s *Service) find(ctx context.Context, filter bson.M, limit int64) ([]Position, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	if limit > 0 {
		opts.SetLimit(limit)
	}
	cur, err := s.positions.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("stake: find positions: %w", err)
	}
	defer cur.Close(ctx)

	var out []Position
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("stake: decode positions: %w", err)
	}
	return out, nil
}

type aggRow struct {
	ID  string `bson:"_id"`
	Sum int64  `bson:"sum"`
}

// UserStakeByOutcome returns how much a user has staked on each outcome of
// a market, summed across their (additive) positions. Outcomes the user
// has not staked on are absent from the map.
func (s *Service) UserStakeByOutcome(ctx context.Context, marketID bson.ObjectID, userID string) (map[string]int64, error) {
	return s.sumBy(ctx, bson.M{"market_id": marketID, "user_id": userID}, "$outcome_id")
}

// StakeByOutcome returns the total staked on each outcome of a market
// across all users, computed from positions. This is the independent
// cross-check against the market document's cached pools.
func (s *Service) StakeByOutcome(ctx context.Context, marketID bson.ObjectID) (map[string]int64, error) {
	return s.sumBy(ctx, bson.M{"market_id": marketID}, "$outcome_id")
}

func (s *Service) sumBy(ctx context.Context, match bson.M, groupBy string) (map[string]int64, error) {
	cur, err := s.positions.Aggregate(ctx, mongo.Pipeline{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$group", Value: bson.M{"_id": groupBy, "sum": bson.M{"$sum": "$amount"}}}},
	})
	if err != nil {
		return nil, fmt.Errorf("stake: aggregate: %w", err)
	}
	defer cur.Close(ctx)

	var rows []aggRow
	if err := cur.All(ctx, &rows); err != nil {
		return nil, fmt.Errorf("stake: decode aggregate: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Sum
	}
	return out, nil
}

// ParticipantCount returns how many distinct users hold a position in a
// market. Backs the participant counter on market cards.
func (s *Service) ParticipantCount(ctx context.Context, marketID bson.ObjectID) (int64, error) {
	cur, err := s.positions.Aggregate(ctx, mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"market_id": marketID}}},
		bson.D{{Key: "$group", Value: bson.M{"_id": "$user_id"}}},
		bson.D{{Key: "$count", Value: "n"}},
	})
	if err != nil {
		return 0, fmt.Errorf("stake: participant count: %w", err)
	}
	defer cur.Close(ctx)

	var rows []struct {
		N int64 `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, fmt.Errorf("stake: decode participant count: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].N, nil
}

// MarketDiscrepancy reports a market whose three independent records of
// staked money disagree.
type MarketDiscrepancy struct {
	MarketID       bson.ObjectID
	PositionsTotal int64            // summed from position documents
	MarketTotal    int64            // the market document's cached total_pool
	EscrowBalance  int64            // the ledger escrow account balance
	OutcomeDrift   map[string]int64 // per outcome: positions sum minus cached pool
}

// Balanced reports whether every record agrees.
func (d MarketDiscrepancy) Balanced() bool {
	return d.PositionsTotal == d.MarketTotal &&
		d.MarketTotal == d.EscrowBalance &&
		len(d.OutcomeDrift) == 0
}

// ReconcileMarket cross-checks a market's staked money three ways: the sum
// of position documents, the market document's cached pools, and the
// ledger escrow balance. PlaceStake writes all three in one transaction,
// so under normal operation they cannot diverge — a discrepancy means
// something wrote outside PlaceStake, or a prior bug left state behind.
//
// This is the invariant the concurrency gate tests assert, and it is the
// analogue of ledger.Reconcile: cheap enough to schedule, too expensive
// for a request path.
//
// Note it holds only while a market is taking stakes. Once settlement pays
// out, escrow drains while positions remain, and the caller should stop
// expecting EscrowBalance to match.
func (s *Service) ReconcileMarket(ctx context.Context, marketID bson.ObjectID) (MarketDiscrepancy, error) {
	m, err := s.markets.Get(ctx, marketID)
	if err != nil {
		return MarketDiscrepancy{}, err
	}

	byOutcome, err := s.StakeByOutcome(ctx, marketID)
	if err != nil {
		return MarketDiscrepancy{}, err
	}

	escrow, err := s.ledger.Balance(ctx, escrowParty(marketID), m.Currency)
	if err != nil {
		return MarketDiscrepancy{}, err
	}

	d := MarketDiscrepancy{
		MarketID:      marketID,
		MarketTotal:   m.TotalPool,
		EscrowBalance: escrow,
		OutcomeDrift:  map[string]int64{},
	}
	for _, sum := range byOutcome {
		d.PositionsTotal += sum
	}
	for _, o := range m.Outcomes {
		if diff := byOutcome[o.ID] - o.Pool; diff != 0 {
			d.OutcomeDrift[o.ID] = diff
		}
	}
	// An outcome present in positions but absent from the market document
	// would otherwise go unnoticed.
	for id, sum := range byOutcome {
		if !m.HasOutcome(id) {
			d.OutcomeDrift[id] = sum
		}
	}
	return d, nil
}
