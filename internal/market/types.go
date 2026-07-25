// Package market owns the market document, its guarded state machine, and
// the in-process auto-close scheduler. It is pure domain plus persistence:
// it never imports ledger or stake, so money movement cannot be triggered
// from inside a state transition. Callers that need both (stake, settle)
// orchestrate the two packages themselves.
package market

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// State is a market's lifecycle position. Payouts execute only on
// StateSettled; refunds only on StateRefunded.
type State string

const (
	StateDraft         State = "DRAFT"
	StateOpen          State = "OPEN"
	StateClosed        State = "CLOSED"
	StateSettling      State = "SETTLING"
	StateSettlePending State = "SETTLE_PENDING"
	StateSettled       State = "SETTLED"
	StateDisputed      State = "DISPUTED"
	StateVoting        State = "VOTING"
	StateCancelled     State = "CANCELLED"
	StateRefunding     State = "REFUNDING"
	StateRefunded      State = "REFUNDED"
)

// AllStates is every valid state, used by the transition-matrix tests and
// by validation.
func AllStates() []State {
	return []State{
		StateDraft, StateOpen, StateClosed, StateSettling, StateSettlePending,
		StateSettled, StateDisputed, StateVoting, StateCancelled,
		StateRefunding, StateRefunded,
	}
}

type VisibilityKind string

const (
	VisibilityPublic VisibilityKind = "public"
	VisibilityCircle VisibilityKind = "circle"
)

// Visibility controls who can see and stake on a market. Enforcement of
// circle membership lives in the circle/stake packages (market has no
// notion of who is in a circle); market only carries the pointer.
type Visibility struct {
	Kind     VisibilityKind `bson:"kind"`
	CircleID string         `bson:"circle_id,omitempty"` // set iff Kind == VisibilityCircle
}

type SettlementMode string

const (
	SettlementCreator   SettlementMode = "creator"
	SettlementFeed      SettlementMode = "feed"
	SettlementGroupVote SettlementMode = "group_vote"
)

// Outcome is one side of a market. ID is a caller-supplied slug ("yes",
// "team_a") rather than an ObjectID: it is an enum option scoped to this
// market, not an independent document. Pool is in the market's Currency
// minor units (kobo for NGN_PLAY), matching the ledger's convention.
//
// Pool is a cache of the staked total, in the same sense that
// ledger.Account.Balance caches the sum of its entries: the authoritative
// record is the ledger's escrow entries for this market. Phase 3's stake
// write must $inc this and TotalPool in the same update that posts to the
// ledger.
type Outcome struct {
	ID    string `bson:"id"`
	Label string `bson:"label"`
	Pool  int64  `bson:"pool"`
}

// Transition is one entry in a market's append-only transition log. It is
// pushed in the same atomic update that changes State, so the log can
// never disagree with the current state.
type Transition struct {
	From   State     `bson:"from"`
	To     State     `bson:"to"`
	Actor  string    `bson:"actor"`
	At     time.Time `bson:"at"`
	Reason string    `bson:"reason,omitempty"`
}

// Well-known non-user actors for the transition log.
const (
	ActorScheduler = "system:scheduler"
	ActorAdmin     = "system:admin"
)

type Market struct {
	ID       bson.ObjectID `bson:"_id,omitempty"`
	Question string        `bson:"question"`
	// Category is a free string (Sports, Soccer, Politics, Friend Bets,
	// ...) rather than an enum: the set will grow and should not require
	// a code change per addition.
	Category  string    `bson:"category"`
	Outcomes  []Outcome `bson:"outcomes"`
	TotalPool int64     `bson:"total_pool"`
	// Currency tags the pool amounts so the market document is
	// self-describing and stake/settle can build ledger.Amount values
	// without hardcoding "NGN_PLAY" in several places. V1 is always
	// NGN_PLAY; real rails later reuse the same field.
	Currency       string         `bson:"currency"`
	Visibility     Visibility     `bson:"visibility"`
	SettlementMode SettlementMode `bson:"settlement_mode"`
	CreatorID      string         `bson:"creator_id"`
	CloseAt        time.Time      `bson:"close_at"`
	State          State          `bson:"state"`
	// WinningOutcomeID is set by the transition into StateSettled (whether
	// via Settle or ResolveVoting) and is what the payout engine, market
	// detail view, and streaks read-model read. Empty until settled.
	WinningOutcomeID string       `bson:"winning_outcome_id,omitempty"`
	Transitions      []Transition `bson:"transitions"`
	CreatedAt        time.Time    `bson:"created_at"`
	UpdatedAt        time.Time    `bson:"updated_at"`
}

// HasOutcome reports whether outcomeID is one of this market's outcomes.
func (m Market) HasOutcome(outcomeID string) bool {
	for _, o := range m.Outcomes {
		if o.ID == outcomeID {
			return true
		}
	}
	return false
}

// MinStakedOutcomes is how many distinct outcomes must have stakes for a
// market to be settleable. With fewer, there is no losing pool to
// distribute (or no winning side at all), so the market is refunded
// instead of settled.
const MinStakedOutcomes = 2

// IsUnderFilled reports whether fewer than MinStakedOutcomes outcomes have
// any stake. This is the canonical under-fill rule: the scheduler uses it
// to choose CANCELLED over CLOSED at close time, and any future
// admin/creator force-close path must use it too rather than
// reimplementing the count.
func IsUnderFilled(outcomes []Outcome) bool {
	staked := 0
	for _, o := range outcomes {
		if o.Pool > 0 {
			staked++
		}
	}
	return staked < MinStakedOutcomes
}
