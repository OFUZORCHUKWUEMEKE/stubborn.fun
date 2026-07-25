// Package ledger implements the append-only double-entry money layer for
// Stubborn.fun. It is play-money only (currency "NGN_PLAY") in V1 but the
// schema is currency-tagged and uses integer minor units throughout so a
// later real-rails phase (NGN/USDC) can slot into the same collections.
package ledger

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// OwnerType identifies what kind of entity an account belongs to.
type OwnerType string

const (
	OwnerUser         OwnerType = "user"
	OwnerSystem       OwnerType = "system"
	OwnerMarketEscrow OwnerType = "market_escrow"
)

// Well-known system account owner IDs (OwnerType == OwnerSystem).
const (
	SystemPromoPool = "promo_pool" // source of starter-balance grants; allowed to go negative
	SystemHouseRake = "house_rake" // accumulates rake + rounding dust
)

// EntryType classifies why money moved, for reporting/audit.
type EntryType string

const (
	EntryStake         EntryType = "stake"
	EntryPayout        EntryType = "payout"
	EntryRefund        EntryType = "refund"
	EntryStarterCredit EntryType = "starter_credit"
	EntryRake          EntryType = "rake"
)

// Direction is the sign of an entry: Debit decreases an account's balance,
// Credit increases it.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Amount is a currency-tagged integer minor-unit quantity. Never a float.
// For NGN_PLAY, Amount is kobo (1 NGN_PLAY = 100 minor units).
type Amount struct {
	Amount   int64  `bson:"amount"`
	Currency string `bson:"currency"`
}

// Party identifies one side of a Transfer.
type Party struct {
	OwnerType OwnerType
	OwnerID   string
}

// Account is the current-balance projection for one (owner, currency) pair.
// Balance is a cache: it MUST always equal the signed sum of this account's
// entries. It is only ever mutated inside the same transaction that appends
// the entries that justify the change.
type Account struct {
	ID        bson.ObjectID `bson:"_id,omitempty"`
	OwnerType OwnerType     `bson:"owner_type"`
	OwnerID   string        `bson:"owner_id"`
	Currency  string        `bson:"currency"`
	Balance   int64         `bson:"balance"`
	// Unlimited marks accounts (e.g. promo_pool) that are allowed to go
	// negative because they are the play-money issuance source, not a
	// real balance. Never set for user or market_escrow accounts.
	Unlimited bool      `bson:"unlimited"`
	CreatedAt time.Time `bson:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"`
}

// Entry is one leg of a double-entry posting. Entries are immutable and
// append-only; corrections happen via new offsetting entries, never edits.
// Every Transfer writes exactly two entries (one Debit, one Credit) sharing
// a TxnID, so entries for a txn always sum to zero.
type Entry struct {
	ID        bson.ObjectID `bson:"_id,omitempty"`
	TxnID     string        `bson:"txn_id"`
	AccountID bson.ObjectID `bson:"account_id"`
	Amount    int64         `bson:"amount"` // always > 0; Direction carries the sign
	Currency  string        `bson:"currency"`
	Direction Direction     `bson:"direction"`
	Type      EntryType     `bson:"type"`
	MarketID  string        `bson:"market_id,omitempty"`
	Ref       string        `bson:"ref,omitempty"`
	CreatedAt time.Time     `bson:"created_at"`
}

// TransferRef carries the free-form context for a Transfer: a human-readable
// note plus an optional market_id so entries can later be queried per
// market (needed by stake/settle in later phases) without parsing Ref
// strings.
type TransferRef struct {
	MarketID string
	Note     string
}
