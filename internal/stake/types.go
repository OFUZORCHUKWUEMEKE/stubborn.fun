// Package stake orchestrates market and ledger: it places stakes, which is
// the one operation that must move money and grow a market's pools as a
// single atomic unit. It imports ledger and market; neither imports stake.
package stake

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const PositionsCollection = "positions"

// Position is one user's stake on one outcome. Positions are additive: a
// user may stake repeatedly, and on more than one outcome of the same
// market, and each attempt is its own document. The authoritative record
// of the money is the ledger entry sharing this position's derived txn id.
type Position struct {
	ID        bson.ObjectID `bson:"_id,omitempty"`
	UserID    string        `bson:"user_id"`
	MarketID  bson.ObjectID `bson:"market_id"`
	OutcomeID string        `bson:"outcome_id"`
	Amount    int64         `bson:"amount"`
	Currency  string        `bson:"currency"`
	// IdempotencyKey is the caller-supplied key that makes a retried
	// request a no-op. Empty when the caller did not supply one, in which
	// case repeated calls create separate, additive positions.
	IdempotencyKey string    `bson:"idempotency_key,omitempty"`
	CreatedAt      time.Time `bson:"created_at"`
}

// TxnID is the ledger transaction id for this position's money movement.
// Deriving it from the position id keeps every ledger entry traceable back
// to the position that caused it, and guarantees uniqueness per position.
func (p Position) TxnID() string {
	return "stake:" + p.ID.Hex()
}

// Request is the input to PlaceStake.
type Request struct {
	UserID    string
	MarketID  bson.ObjectID
	OutcomeID string
	Amount    int64
	// IdempotencyKey is optional. When set, a repeat request with the same
	// (user, key) returns the original position instead of staking again.
	IdempotencyKey string
}

// OutcomePrice is an outcome's implied price in integer basis points
// (10000 bps = 100%). Prices are derived from pool sizes on read, never
// stored: the pools are the authority.
type OutcomePrice struct {
	OutcomeID string `json:"outcome_id"`
	PriceBPS  int    `json:"price_bps"`
}

// Result is what a caller needs after a successful stake, including the
// post-stake pool snapshot for broadcasting to market subscribers.
type Result struct {
	Position  Position
	MarketID  bson.ObjectID
	TotalPool int64
	Prices    []OutcomePrice
	// Replayed is true when an idempotency key matched an existing
	// position, so no new money moved.
	Replayed bool
}

// MembershipChecker answers whether a user belongs to a circle. It is
// declared here, where it is consumed, so stake does not depend on the
// circle package (which arrives in Phase 6); circle will satisfy this
// interface.
type MembershipChecker interface {
	IsMember(ctx context.Context, circleID, userID string) (bool, error)
}

// DenyAllMembership refuses every circle membership check. It is the
// deliberate default until the circle package exists: an unimplemented
// visibility check must fail closed, never silently admit everyone.
type DenyAllMembership struct{}

func (DenyAllMembership) IsMember(context.Context, string, string) (bool, error) {
	return false, nil
}

// Config bounds a single stake. Amounts are in the market's currency minor
// units (kobo for NGN_PLAY).
type Config struct {
	MinStake int64
	MaxStake int64
}

// DefaultConfig allows ₦1 to ₦1,000,000 of play money per stake.
func DefaultConfig() Config {
	return Config{
		MinStake: 100,
		MaxStake: 100_000_000,
	}
}
