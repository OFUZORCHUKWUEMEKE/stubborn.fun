package market

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/platform"
)

const MarketsCollection = "markets"

// Store is the persistence boundary for markets. All state changes go
// through its transition methods; nothing else may write the state field.
type Store struct {
	markets *mongo.Collection
	clock   platform.Clock
}

func NewStore(db *mongo.Database, clock platform.Clock) *Store {
	return &Store{
		markets: db.Collection(MarketsCollection),
		clock:   clock,
	}
}

// EnsureIndexes creates the index the scheduler's poll depends on. Without
// it, every tick is a full collection scan.
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	_, err := db.Collection(MarketsCollection).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "state", Value: 1}, {Key: "close_at", Value: 1}},
		Options: options.Index().SetName("by_state_close_at"),
	})
	return err
}

// NewMarket is the caller-supplied input to Create. Pools are not
// accepted: a new market always starts with empty pools in DRAFT.
type NewMarket struct {
	Question       string
	Category       string
	Outcomes       []Outcome
	Currency       string
	Visibility     Visibility
	SettlementMode SettlementMode
	CreatorID      string
	CloseAt        time.Time
}

func (n NewMarket) validate(now time.Time) error {
	if n.Question == "" {
		return ErrEmptyQuestion
	}
	if n.Category == "" {
		return ErrEmptyCategory
	}
	if n.Currency == "" {
		return ErrEmptyCurrency
	}
	if n.CreatorID == "" {
		return ErrEmptyCreator
	}
	if len(n.Outcomes) < MinStakedOutcomes {
		return ErrTooFewOutcomes
	}

	seen := make(map[string]struct{}, len(n.Outcomes))
	for _, o := range n.Outcomes {
		if o.ID == "" || o.Label == "" {
			return ErrEmptyOutcomeField
		}
		if o.Pool != 0 {
			return ErrNonZeroInitialPool
		}
		if _, dup := seen[o.ID]; dup {
			return ErrDuplicateOutcomeID
		}
		seen[o.ID] = struct{}{}
	}

	switch n.Visibility.Kind {
	case VisibilityPublic:
		if n.Visibility.CircleID != "" {
			return ErrInvalidVisibility
		}
	case VisibilityCircle:
		if n.Visibility.CircleID == "" {
			return ErrInvalidVisibility
		}
	default:
		return ErrInvalidVisibility
	}

	switch n.SettlementMode {
	case SettlementCreator, SettlementFeed, SettlementGroupVote:
	default:
		return ErrInvalidSettlement
	}

	if !n.CloseAt.After(now) {
		return ErrCloseAtNotInFuture
	}
	return nil
}

// Create validates and inserts a market in StateDraft with empty pools and
// an empty transition log. Publish moves it to OPEN.
func (s *Store) Create(ctx context.Context, n NewMarket) (Market, error) {
	now := s.clock.Now()
	if err := n.validate(now); err != nil {
		return Market{}, err
	}

	m := Market{
		ID:             bson.NewObjectID(),
		Question:       n.Question,
		Category:       n.Category,
		Outcomes:       n.Outcomes,
		TotalPool:      0,
		Currency:       n.Currency,
		Visibility:     n.Visibility,
		SettlementMode: n.SettlementMode,
		CreatorID:      n.CreatorID,
		CloseAt:        n.CloseAt.UTC(),
		State:          StateDraft,
		Transitions:    []Transition{},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if _, err := s.markets.InsertOne(ctx, m); err != nil {
		return Market{}, fmt.Errorf("market: create: %w", err)
	}
	return m, nil
}

// Get returns a market by ID, or ErrMarketNotFound.
func (s *Store) Get(ctx context.Context, id bson.ObjectID) (Market, error) {
	var m Market
	err := s.markets.FindOne(ctx, bson.M{"_id": id}).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Market{}, ErrMarketNotFound
	}
	if err != nil {
		return Market{}, fmt.Errorf("market: get: %w", err)
	}
	return m, nil
}

// FindOverdueOpen returns up to limit OPEN markets whose close_at is at or
// before now, oldest deadline first. The scheduler re-derives this on every
// tick, which is what makes it crash-safe: there is no in-memory timer
// state to lose.
func (s *Store) FindOverdueOpen(ctx context.Context, now time.Time, limit int64) ([]Market, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "close_at", Value: 1}}).
		SetLimit(limit)
	cur, err := s.markets.Find(ctx, bson.M{
		"state":    StateOpen,
		"close_at": bson.M{"$lte": now},
	}, opts)
	if err != nil {
		return nil, fmt.Errorf("market: find overdue open: %w", err)
	}
	defer cur.Close(ctx)

	var out []Market
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("market: find overdue open: %w", err)
	}
	return out, nil
}

// transition performs a guarded state change as a single atomic
// compare-and-swap: the `state: from` clause in the filter is the
// precondition, so two concurrent callers attempting the same transition
// cannot both succeed — the loser matches zero documents and gets
// ErrIllegalTransition.
//
// extraSet adds fields to the same $set (used to record the winning
// outcome atomically with the move to SETTLED). extraFilter adds further
// preconditions; residualErr is returned when the document exists and its
// state does match `from`, meaning it was an extraFilter clause that
// failed.
func (s *Store) transition(
	ctx context.Context,
	id bson.ObjectID,
	from, to State,
	actor, reason string,
	extraSet bson.M,
	extraFilter bson.M,
	residualErr error,
) (Market, error) {
	now := s.clock.Now()

	set := bson.M{"state": to, "updated_at": now}
	for k, v := range extraSet {
		set[k] = v
	}

	filter := bson.M{"_id": id, "state": from}
	for k, v := range extraFilter {
		filter[k] = v
	}

	update := bson.M{
		"$set": set,
		"$push": bson.M{"transitions": Transition{
			From:   from,
			To:     to,
			Actor:  actor,
			At:     now,
			Reason: reason,
		}},
	}

	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var m Market
	err := s.markets.FindOneAndUpdate(ctx, filter, update, opts).Decode(&m)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return Market{}, fmt.Errorf("market: transition %s->%s: %w", from, to, err)
	}

	// Nothing matched: distinguish missing document from failed
	// precondition so callers get an actionable error.
	current, getErr := s.Get(ctx, id)
	if getErr != nil {
		return Market{}, getErr // ErrMarketNotFound, or a real read error
	}
	if current.State != from {
		return Market{}, fmt.Errorf("%w: %s is in state %s, requires %s", ErrIllegalTransition, id.Hex(), current.State, from)
	}
	if residualErr != nil {
		return Market{}, residualErr
	}
	return Market{}, fmt.Errorf("%w: %s precondition failed", ErrIllegalTransition, id.Hex())
}

// --- Guarded transitions. Each hardcodes exactly one legal edge; every
// other current state yields ErrIllegalTransition. ---

// Publish moves DRAFT -> OPEN, making the market stakeable.
func (s *Store) Publish(ctx context.Context, id bson.ObjectID, actor string) (Market, error) {
	return s.transition(ctx, id, StateDraft, StateOpen, actor, "", nil, nil, nil)
}

// Close moves OPEN -> CLOSED at close time. Callers deciding between Close
// and Cancel must consult IsUnderFilled rather than reimplementing the
// rule; Close itself does not check pools, so a caller that skips that
// check can close an under-filled market.
func (s *Store) Close(ctx context.Context, id bson.ObjectID, actor string) (Market, error) {
	return s.transition(ctx, id, StateOpen, StateClosed, actor, "", nil, nil, nil)
}

// Cancel moves OPEN -> CANCELLED, the entry point to the refund path.
func (s *Store) Cancel(ctx context.Context, id bson.ObjectID, actor, reason string) (Market, error) {
	return s.transition(ctx, id, StateOpen, StateCancelled, actor, reason, nil, nil, nil)
}

// StartSettling moves CLOSED -> SETTLING.
func (s *Store) StartSettling(ctx context.Context, id bson.ObjectID, actor string) (Market, error) {
	return s.transition(ctx, id, StateClosed, StateSettling, actor, "", nil, nil, nil)
}

// MoveToSettlePending moves SETTLING -> SETTLE_PENDING, where friend bets
// wait out their dispute window.
func (s *Store) MoveToSettlePending(ctx context.Context, id bson.ObjectID, actor string) (Market, error) {
	return s.transition(ctx, id, StateSettling, StateSettlePending, actor, "", nil, nil, nil)
}

// Dispute moves SETTLE_PENDING -> DISPUTED. Whether a market is eligible
// to be disputed (friend bets only) is an authorization concern for the
// settle package, not a state-machine concern.
func (s *Store) Dispute(ctx context.Context, id bson.ObjectID, actor, reason string) (Market, error) {
	return s.transition(ctx, id, StateSettlePending, StateDisputed, actor, reason, nil, nil, nil)
}

// Settle moves SETTLE_PENDING -> SETTLED, recording the winning outcome in
// the same atomic update. winningOutcomeID must be one of the market's
// outcomes, enforced as a filter precondition so it cannot race.
func (s *Store) Settle(ctx context.Context, id bson.ObjectID, actor, winningOutcomeID string) (Market, error) {
	return s.transition(ctx, id, StateSettlePending, StateSettled, actor, "",
		bson.M{"winning_outcome_id": winningOutcomeID},
		bson.M{"outcomes.id": winningOutcomeID},
		ErrUnknownOutcome,
	)
}

// StartVoting moves DISPUTED -> VOTING.
func (s *Store) StartVoting(ctx context.Context, id bson.ObjectID, actor string) (Market, error) {
	return s.transition(ctx, id, StateDisputed, StateVoting, actor, "", nil, nil, nil)
}

// ResolveVoting moves VOTING -> SETTLED with the vote's winning outcome.
func (s *Store) ResolveVoting(ctx context.Context, id bson.ObjectID, actor, winningOutcomeID string) (Market, error) {
	return s.transition(ctx, id, StateVoting, StateSettled, actor, "",
		bson.M{"winning_outcome_id": winningOutcomeID},
		bson.M{"outcomes.id": winningOutcomeID},
		ErrUnknownOutcome,
	)
}

// StartRefunding moves CANCELLED -> REFUNDING.
func (s *Store) StartRefunding(ctx context.Context, id bson.ObjectID, actor string) (Market, error) {
	return s.transition(ctx, id, StateCancelled, StateRefunding, actor, "", nil, nil, nil)
}

// CompleteRefund moves REFUNDING -> REFUNDED.
func (s *Store) CompleteRefund(ctx context.Context, id bson.ObjectID, actor string) (Market, error) {
	return s.transition(ctx, id, StateRefunding, StateRefunded, actor, "", nil, nil, nil)
}
