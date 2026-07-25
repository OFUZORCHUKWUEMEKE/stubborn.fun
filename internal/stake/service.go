package stake

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/ledger"
	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/market"
	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/platform"
)

// Service places stakes and answers position queries.
type Service struct {
	client    *mongo.Client
	positions *mongo.Collection
	ledger    *ledger.Ledger
	markets   *market.Store
	members   MembershipChecker
	clock     platform.Clock
	cfg       Config
}

func NewService(
	client *mongo.Client,
	db *mongo.Database,
	led *ledger.Ledger,
	markets *market.Store,
	members MembershipChecker,
	clock platform.Clock,
	cfg Config,
) *Service {
	if members == nil {
		// Fail closed: a nil checker must not mean "allow everyone".
		members = DenyAllMembership{}
	}
	return &Service{
		client:    client,
		positions: db.Collection(PositionsCollection),
		ledger:    led,
		markets:   markets,
		members:   members,
		clock:     clock,
		cfg:       cfg,
	}
}

// EnsureIndexes creates the indexes stake relies on.
//
// The unique partial index on (user_id, idempotency_key) is a correctness
// dependency, not a performance one: it is what makes a replayed request
// fail its insert instead of double-staking. It is partial so the many
// positions without a key do not collide on a null value.
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	positions := db.Collection(PositionsCollection)

	if _, err := positions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "idempotency_key", Value: 1}},
		Options: options.Index().
			SetName("uniq_user_idempotency_key").
			SetUnique(true).
			SetPartialFilterExpression(bson.M{"idempotency_key": bson.M{"$type": "string"}}),
	}); err != nil {
		return err
	}

	if _, err := positions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "market_id", Value: 1}, {Key: "created_at", Value: -1}},
		Options: options.Index().SetName("by_market_created"),
	}); err != nil {
		return err
	}

	if _, err := positions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}},
		Options: options.Index().SetName("by_user_created"),
	}); err != nil {
		return err
	}

	_, err := positions.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "market_id", Value: 1}, {Key: "user_id", Value: 1}, {Key: "outcome_id", Value: 1}},
		Options: options.Index().SetName("by_market_user_outcome"),
	})
	return err
}

// escrowParty is the ledger account holding a market's staked funds.
func escrowParty(marketID bson.ObjectID) ledger.Party {
	return ledger.Party{OwnerType: ledger.OwnerMarketEscrow, OwnerID: marketID.Hex()}
}

func userParty(userID string) ledger.Party {
	return ledger.Party{OwnerType: ledger.OwnerUser, OwnerID: userID}
}

// PlaceStake moves amount from the user's wallet into the market's escrow,
// records a position, and grows the outcome's pool — atomically, or not at
// all.
//
// Validation happens twice by design. The pre-transaction pass is a cheap
// fast-fail that avoids taking write locks for obviously bad requests; the
// authority is inside the transaction, where market.ApplyStake re-checks
// state, deadline, and outcome as filter preconditions. Only the second
// pass can be trusted, because the first races the close scheduler.
func (s *Service) PlaceStake(ctx context.Context, req Request) (Result, error) {
	if req.UserID == "" {
		return Result{}, ErrEmptyUserID
	}
	if req.Amount < s.cfg.MinStake {
		return Result{}, fmt.Errorf("%w: %d < %d", ErrStakeTooSmall, req.Amount, s.cfg.MinStake)
	}
	if req.Amount > s.cfg.MaxStake {
		return Result{}, fmt.Errorf("%w: %d > %d", ErrStakeTooLarge, req.Amount, s.cfg.MaxStake)
	}

	// Fast-fail on an already-known replay before doing any work.
	if req.IdempotencyKey != "" {
		if existing, err := s.positionByKey(ctx, req.UserID, req.IdempotencyKey); err == nil {
			return s.replayResult(ctx, existing)
		} else if !errors.Is(err, ErrPositionNotFound) {
			return Result{}, err
		}
	}

	m, err := s.markets.Get(ctx, req.MarketID)
	if err != nil {
		return Result{}, err
	}
	if err := s.checkStakeable(ctx, m, req); err != nil {
		return Result{}, err
	}

	position := Position{
		ID:             bson.NewObjectID(),
		UserID:         req.UserID,
		MarketID:       req.MarketID,
		OutcomeID:      req.OutcomeID,
		Amount:         req.Amount,
		Currency:       m.Currency,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      s.clock.Now(),
	}

	session, err := s.client.StartSession()
	if err != nil {
		return Result{}, fmt.Errorf("stake: start session: %w", err)
	}
	defer session.EndSession(ctx)

	var updated market.Market
	_, err = session.WithTransaction(ctx, func(txCtx context.Context) (any, error) {
		// 1. Position first, so a replayed idempotency key is rejected by
		//    the unique index before any money moves.
		if _, err := s.positions.InsertOne(txCtx, position); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return nil, errReplayDetected
			}
			return nil, err
		}

		// 2. Money. The ledger is the authority on funds; an overdraft
		//    fails here and unwinds the position insert above.
		if err := s.ledger.TransferTx(txCtx, position.TxnID(),
			userParty(req.UserID), escrowParty(req.MarketID),
			ledger.Amount{Amount: req.Amount, Currency: m.Currency},
			ledger.EntryStake,
			ledger.TransferRef{MarketID: req.MarketID.Hex(), Note: "stake"},
		); err != nil {
			return nil, err
		}

		// 3. Pools. This is the authoritative state/deadline/outcome gate:
		//    its filter preconditions cannot be raced by the close
		//    scheduler. If it fails, both writes above roll back.
		u, err := s.markets.ApplyStake(txCtx, req.MarketID, req.OutcomeID, req.Amount)
		if err != nil {
			return nil, err
		}
		updated = u
		return nil, nil
	})

	if err != nil {
		// A concurrent request with the same idempotency key won the race.
		// Its position is committed, so return that rather than an error.
		if errors.Is(err, errReplayDetected) || errors.Is(err, ledger.ErrDuplicateTxn) {
			existing, lookupErr := s.positionByKey(ctx, req.UserID, req.IdempotencyKey)
			if lookupErr != nil {
				return Result{}, fmt.Errorf("stake: replay detected but original position not found: %w", lookupErr)
			}
			return s.replayResult(ctx, existing)
		}
		return Result{}, err
	}

	return Result{
		Position:  position,
		MarketID:  req.MarketID,
		TotalPool: updated.TotalPool,
		Prices:    PoolPrices(updated),
	}, nil
}

// errReplayDetected unwinds the transaction when the idempotency index
// rejects a duplicate insert. It never escapes PlaceStake.
var errReplayDetected = errors.New("stake: idempotency key replay")

// checkStakeable runs the pre-transaction validation: everything that can
// be judged from a snapshot read. The transaction re-checks whatever can
// change underneath it.
func (s *Service) checkStakeable(ctx context.Context, m market.Market, req Request) error {
	if m.State != market.StateOpen {
		return fmt.Errorf("%w: %s is %s", market.ErrMarketClosed, m.ID.Hex(), m.State)
	}
	if !m.CloseAt.After(s.clock.Now()) {
		return fmt.Errorf("%w: %s closed at %s", market.ErrMarketClosed, m.ID.Hex(), m.CloseAt)
	}
	if !m.HasOutcome(req.OutcomeID) {
		return fmt.Errorf("%w: %q", market.ErrUnknownOutcome, req.OutcomeID)
	}
	if m.Visibility.Kind == market.VisibilityCircle {
		ok, err := s.members.IsMember(ctx, m.Visibility.CircleID, req.UserID)
		if err != nil {
			return fmt.Errorf("stake: membership check: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: circle %s", ErrNotCircleMember, m.Visibility.CircleID)
		}
	}
	return nil
}

func (s *Service) positionByKey(ctx context.Context, userID, key string) (Position, error) {
	if key == "" {
		return Position{}, ErrPositionNotFound
	}
	var p Position
	err := s.positions.FindOne(ctx, bson.M{"user_id": userID, "idempotency_key": key}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Position{}, ErrPositionNotFound
	}
	if err != nil {
		return Position{}, fmt.Errorf("stake: lookup by idempotency key: %w", err)
	}
	return p, nil
}

// replayResult rebuilds a Result for an already-committed position, with
// the market's current pools rather than those at original stake time.
func (s *Service) replayResult(ctx context.Context, p Position) (Result, error) {
	m, err := s.markets.Get(ctx, p.MarketID)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Position:  p,
		MarketID:  p.MarketID,
		TotalPool: m.TotalPool,
		Prices:    PoolPrices(m),
		Replayed:  true,
	}, nil
}
