package market

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/platform"
)

// baseTime is the fixed instant every fake clock starts at, so failures
// report comparable timestamps.
var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Integration tests need a real Mongo (transitions use findOneAndUpdate,
// which works on a standalone too, but the rest of the stack assumes the
// replica set from `make mongo-rs`). Skips cleanly when unset.
func newTestStore(t *testing.T) (*Store, *platform.FakeClock) {
	t.Helper()
	uri := os.Getenv("STUBBORN_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("STUBBORN_TEST_MONGO_URI not set; skipping market integration tests (see `make mongo-rs`)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}

	db := client.Database(fmt.Sprintf("stubborn_market_test_%d", time.Now().UnixNano()))
	if err := EnsureIndexes(ctx, db); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(dropCtx)
		_ = client.Disconnect(dropCtx)
	})

	clock := platform.NewFakeClock(baseTime)
	return NewStore(db, clock), clock
}

func validNewMarket() NewMarket {
	return NewMarket{
		Question: "Will Super Eagles beat Ghana?",
		Category: "Soccer",
		Outcomes: []Outcome{
			{ID: "yes", Label: "Yes"},
			{ID: "no", Label: "No"},
		},
		Currency:       "NGN_PLAY",
		Visibility:     Visibility{Kind: VisibilityPublic},
		SettlementMode: SettlementCreator,
		CreatorID:      "user-creator",
		CloseAt:        baseTime.Add(time.Hour),
	}
}

func mustCreate(t *testing.T, s *Store) Market {
	t.Helper()
	m, err := s.Create(context.Background(), validNewMarket())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return m
}

// forceState writes the state field directly, bypassing the guarded
// transitions, so the matrix test can put a market into any state without
// walking a legal path to it. It deliberately does not touch the
// transition log, so illegal-transition assertions can check the log was
// not appended to.
func forceState(t *testing.T, s *Store, id bson.ObjectID, state State) {
	t.Helper()
	if _, err := s.markets.UpdateByID(context.Background(), id, bson.M{"$set": bson.M{"state": state}}); err != nil {
		t.Fatalf("force state %s: %v", state, err)
	}
}

// setPools simulates staking without importing the stake package (which
// does not exist yet): it overwrites the outcome pools and total_pool
// directly. Phase 3 will do this via a real transactional $inc alongside
// the ledger posting.
func setPools(t *testing.T, s *Store, id bson.ObjectID, pools map[string]int64) {
	t.Helper()
	ctx := context.Background()
	m, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get for setPools: %v", err)
	}
	var total int64
	for i := range m.Outcomes {
		if p, ok := pools[m.Outcomes[i].ID]; ok {
			m.Outcomes[i].Pool = p
		}
		total += m.Outcomes[i].Pool
	}
	if _, err := s.markets.UpdateByID(ctx, id, bson.M{
		"$set": bson.M{"outcomes": m.Outcomes, "total_pool": total},
	}); err != nil {
		t.Fatalf("set pools: %v", err)
	}
}

func mustGet(t *testing.T, s *Store, id bson.ObjectID) Market {
	t.Helper()
	m, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return m
}

// --- Transition matrix ---

// transitionOp is one guarded transition under test, with the single
// (from -> to) edge it is allowed to traverse.
type transitionOp struct {
	name string
	from State
	to   State
	call func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error)
}

func allOps() []transitionOp {
	const actor = "user-actor"
	return []transitionOp{
		{"Publish", StateDraft, StateOpen, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.Publish(ctx, id, actor)
		}},
		{"Close", StateOpen, StateClosed, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.Close(ctx, id, actor)
		}},
		{"Cancel", StateOpen, StateCancelled, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.Cancel(ctx, id, actor, "test cancel")
		}},
		{"StartSettling", StateClosed, StateSettling, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.StartSettling(ctx, id, actor)
		}},
		{"MoveToSettlePending", StateSettling, StateSettlePending, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.MoveToSettlePending(ctx, id, actor)
		}},
		{"Dispute", StateSettlePending, StateDisputed, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.Dispute(ctx, id, actor, "wrong result")
		}},
		{"Settle", StateSettlePending, StateSettled, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.Settle(ctx, id, actor, "yes")
		}},
		{"StartVoting", StateDisputed, StateVoting, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.StartVoting(ctx, id, actor)
		}},
		{"ResolveVoting", StateVoting, StateSettled, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.ResolveVoting(ctx, id, actor, "yes")
		}},
		{"StartRefunding", StateCancelled, StateRefunding, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.StartRefunding(ctx, id, actor)
		}},
		{"CompleteRefund", StateRefunding, StateRefunded, func(ctx context.Context, s *Store, id bson.ObjectID) (Market, error) {
			return s.CompleteRefund(ctx, id, actor)
		}},
	}
}

// TestTransitionMatrix is the full gate: every guarded transition is
// attempted from every state. Exactly one (op, state) pair per op may
// succeed; all 110 others must fail with ErrIllegalTransition and leave
// the document — state and transition log alike — untouched.
func TestTransitionMatrix(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	for _, op := range allOps() {
		for _, state := range AllStates() {
			t.Run(fmt.Sprintf("%s/from_%s", op.name, state), func(t *testing.T) {
				m := mustCreate(t, s)
				forceState(t, s, m.ID, state)

				got, err := op.call(ctx, s, m.ID)

				if state == op.from {
					if err != nil {
						t.Fatalf("legal transition %s from %s: unexpected error %v", op.name, state, err)
					}
					if got.State != op.to {
						t.Fatalf("returned state = %s, want %s", got.State, op.to)
					}

					after := mustGet(t, s, m.ID)
					if after.State != op.to {
						t.Fatalf("persisted state = %s, want %s", after.State, op.to)
					}
					if len(after.Transitions) != 1 {
						t.Fatalf("len(transitions) = %d, want 1", len(after.Transitions))
					}
					tr := after.Transitions[0]
					if tr.From != op.from || tr.To != op.to {
						t.Fatalf("logged transition = %s->%s, want %s->%s", tr.From, tr.To, op.from, op.to)
					}
					if tr.At.IsZero() {
						t.Fatal("logged transition has zero timestamp")
					}
					return
				}

				if !errors.Is(err, ErrIllegalTransition) {
					t.Fatalf("illegal transition %s from %s: err = %v, want ErrIllegalTransition", op.name, state, err)
				}
				after := mustGet(t, s, m.ID)
				if after.State != state {
					t.Fatalf("state changed on illegal transition: %s -> %s", state, after.State)
				}
				if len(after.Transitions) != 0 {
					t.Fatalf("transition log appended on illegal transition: %+v", after.Transitions)
				}
			})
		}
	}
}

func TestTransition_MarketNotFoundIsDistinctError(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	missing := bson.NewObjectID()
	for _, op := range allOps() {
		t.Run(op.name, func(t *testing.T) {
			_, err := op.call(ctx, s, missing)
			if !errors.Is(err, ErrMarketNotFound) {
				t.Fatalf("err = %v, want ErrMarketNotFound", err)
			}
			if errors.Is(err, ErrIllegalTransition) {
				t.Fatal("a missing market must not be reported as an illegal transition")
			}
		})
	}
}

// TestClose_ConcurrentOnlyOneWins proves the compare-and-swap: many
// closers race on one OPEN market, exactly one succeeds, and the
// transition log records exactly one close rather than one per caller.
func TestClose_ConcurrentOnlyOneWins(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, s)
	if _, err := s.Publish(ctx, m.ID, "user-creator"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	const goroutines = 50
	var wg sync.WaitGroup
	var won, lost int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Close(ctx, m.ID, fmt.Sprintf("closer-%d", i))
			switch {
			case err == nil:
				atomic.AddInt64(&won, 1)
			case errors.Is(err, ErrIllegalTransition):
				atomic.AddInt64(&lost, 1)
			default:
				t.Errorf("closer %d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if won != 1 {
		t.Fatalf("won = %d, want exactly 1", won)
	}
	if lost != goroutines-1 {
		t.Fatalf("lost = %d, want %d", lost, goroutines-1)
	}

	after := mustGet(t, s, m.ID)
	if after.State != StateClosed {
		t.Fatalf("state = %s, want CLOSED", after.State)
	}
	// One publish + exactly one close.
	if len(after.Transitions) != 2 {
		t.Fatalf("len(transitions) = %d, want 2 (publish + one close)", len(after.Transitions))
	}
	closes := 0
	for _, tr := range after.Transitions {
		if tr.To == StateClosed {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("logged %d CLOSED transitions, want 1", closes)
	}
}

// --- Full lifecycle paths ---

func TestLifecycle_HappyPathLogsEveryStep(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, s)
	if _, err := s.Publish(ctx, m.ID, "user-creator"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := s.Close(ctx, m.ID, ActorScheduler); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := s.StartSettling(ctx, m.ID, "user-creator"); err != nil {
		t.Fatalf("start settling: %v", err)
	}
	if _, err := s.MoveToSettlePending(ctx, m.ID, "user-creator"); err != nil {
		t.Fatalf("move to settle pending: %v", err)
	}
	settled, err := s.Settle(ctx, m.ID, "user-creator", "yes")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}

	if settled.WinningOutcomeID != "yes" {
		t.Fatalf("winning outcome = %q, want \"yes\"", settled.WinningOutcomeID)
	}

	want := []struct {
		from, to State
		actor    string
	}{
		{StateDraft, StateOpen, "user-creator"},
		{StateOpen, StateClosed, ActorScheduler},
		{StateClosed, StateSettling, "user-creator"},
		{StateSettling, StateSettlePending, "user-creator"},
		{StateSettlePending, StateSettled, "user-creator"},
	}
	after := mustGet(t, s, m.ID)
	if len(after.Transitions) != len(want) {
		t.Fatalf("len(transitions) = %d, want %d", len(after.Transitions), len(want))
	}
	for i, w := range want {
		tr := after.Transitions[i]
		if tr.From != w.from || tr.To != w.to || tr.Actor != w.actor {
			t.Errorf("transition[%d] = %s->%s by %s, want %s->%s by %s",
				i, tr.From, tr.To, tr.Actor, w.from, w.to, w.actor)
		}
	}
}

func TestLifecycle_DisputePathReachesSettled(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, s)
	forceState(t, s, m.ID, StateSettlePending)

	if _, err := s.Dispute(ctx, m.ID, "user-friend", "that's not what happened"); err != nil {
		t.Fatalf("dispute: %v", err)
	}
	if _, err := s.StartVoting(ctx, m.ID, ActorAdmin); err != nil {
		t.Fatalf("start voting: %v", err)
	}
	resolved, err := s.ResolveVoting(ctx, m.ID, ActorAdmin, "no")
	if err != nil {
		t.Fatalf("resolve voting: %v", err)
	}
	if resolved.State != StateSettled {
		t.Fatalf("state = %s, want SETTLED", resolved.State)
	}
	if resolved.WinningOutcomeID != "no" {
		t.Fatalf("winning outcome = %q, want \"no\"", resolved.WinningOutcomeID)
	}
}

func TestLifecycle_RefundPathReachesRefunded(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, s)
	if _, err := s.Publish(ctx, m.ID, "user-creator"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := s.Cancel(ctx, m.ID, ActorScheduler, "under-filled"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.StartRefunding(ctx, m.ID, ActorAdmin); err != nil {
		t.Fatalf("start refunding: %v", err)
	}
	refunded, err := s.CompleteRefund(ctx, m.ID, ActorAdmin)
	if err != nil {
		t.Fatalf("complete refund: %v", err)
	}
	if refunded.State != StateRefunded {
		t.Fatalf("state = %s, want REFUNDED", refunded.State)
	}
	// The cancel reason must survive on the log for support/debugging.
	found := false
	for _, tr := range refunded.Transitions {
		if tr.To == StateCancelled && tr.Reason == "under-filled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancel reason not recorded in log: %+v", refunded.Transitions)
	}
}

// --- Winning outcome validation ---

func TestSettle_RejectsUnknownOutcomeWithoutMutating(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, s)
	forceState(t, s, m.ID, StateSettlePending)

	_, err := s.Settle(ctx, m.ID, "user-creator", "maybe")
	if !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("err = %v, want ErrUnknownOutcome", err)
	}

	after := mustGet(t, s, m.ID)
	if after.State != StateSettlePending {
		t.Fatalf("state = %s, want unchanged SETTLE_PENDING", after.State)
	}
	if after.WinningOutcomeID != "" {
		t.Fatalf("winning outcome = %q, want empty", after.WinningOutcomeID)
	}
	if len(after.Transitions) != 0 {
		t.Fatalf("transition log appended: %+v", after.Transitions)
	}
}

func TestResolveVoting_RejectsUnknownOutcome(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, s)
	forceState(t, s, m.ID, StateVoting)

	if _, err := s.ResolveVoting(ctx, m.ID, ActorAdmin, "nope"); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("err = %v, want ErrUnknownOutcome", err)
	}
}

// TestSettle_DoubleSettleIsIllegal covers the money-safety-adjacent case:
// settlement must not be re-runnable, since Phase 4 keys payouts off this
// transition.
func TestSettle_DoubleSettleIsIllegal(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, s)
	forceState(t, s, m.ID, StateSettlePending)

	if _, err := s.Settle(ctx, m.ID, "user-creator", "yes"); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if _, err := s.Settle(ctx, m.ID, "user-creator", "no"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second settle: err = %v, want ErrIllegalTransition", err)
	}

	after := mustGet(t, s, m.ID)
	if after.WinningOutcomeID != "yes" {
		t.Fatalf("winning outcome = %q, want \"yes\" (second settle must not overwrite)", after.WinningOutcomeID)
	}
	if len(after.Transitions) != 1 {
		t.Fatalf("len(transitions) = %d, want 1", len(after.Transitions))
	}
}

// --- Create validation ---

func TestCreate_Validation(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()

	twoOutcomes := []Outcome{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}}

	cases := []struct {
		name   string
		mutate func(*NewMarket)
		want   error
	}{
		{"valid", func(n *NewMarket) {}, nil},
		{"empty question", func(n *NewMarket) { n.Question = "" }, ErrEmptyQuestion},
		{"empty category", func(n *NewMarket) { n.Category = "" }, ErrEmptyCategory},
		{"empty currency", func(n *NewMarket) { n.Currency = "" }, ErrEmptyCurrency},
		{"empty creator", func(n *NewMarket) { n.CreatorID = "" }, ErrEmptyCreator},
		{"one outcome", func(n *NewMarket) { n.Outcomes = twoOutcomes[:1] }, ErrTooFewOutcomes},
		{"no outcomes", func(n *NewMarket) { n.Outcomes = nil }, ErrTooFewOutcomes},
		{"duplicate outcome id", func(n *NewMarket) {
			n.Outcomes = []Outcome{{ID: "yes", Label: "Yes"}, {ID: "yes", Label: "Yes again"}}
		}, ErrDuplicateOutcomeID},
		{"empty outcome id", func(n *NewMarket) {
			n.Outcomes = []Outcome{{ID: "", Label: "Yes"}, {ID: "no", Label: "No"}}
		}, ErrEmptyOutcomeField},
		{"empty outcome label", func(n *NewMarket) {
			n.Outcomes = []Outcome{{ID: "yes", Label: ""}, {ID: "no", Label: "No"}}
		}, ErrEmptyOutcomeField},
		{"pre-seeded pool", func(n *NewMarket) {
			n.Outcomes = []Outcome{{ID: "yes", Label: "Yes", Pool: 500}, {ID: "no", Label: "No"}}
		}, ErrNonZeroInitialPool},
		{"public with circle id", func(n *NewMarket) {
			n.Visibility = Visibility{Kind: VisibilityPublic, CircleID: "c1"}
		}, ErrInvalidVisibility},
		{"circle without circle id", func(n *NewMarket) {
			n.Visibility = Visibility{Kind: VisibilityCircle}
		}, ErrInvalidVisibility},
		{"unknown visibility", func(n *NewMarket) {
			n.Visibility = Visibility{Kind: "friends-of-friends"}
		}, ErrInvalidVisibility},
		{"unknown settlement mode", func(n *NewMarket) { n.SettlementMode = "vibes" }, ErrInvalidSettlement},
		{"close_at in past", func(n *NewMarket) { n.CloseAt = clock.Now().Add(-time.Minute) }, ErrCloseAtNotInFuture},
		{"close_at exactly now", func(n *NewMarket) { n.CloseAt = clock.Now() }, ErrCloseAtNotInFuture},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := validNewMarket()
			tc.mutate(&n)

			m, err := s.Create(ctx, n)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if m.State != StateDraft {
					t.Fatalf("state = %s, want DRAFT", m.State)
				}
				if m.TotalPool != 0 {
					t.Fatalf("total pool = %d, want 0", m.TotalPool)
				}
				if len(m.Transitions) != 0 {
					t.Fatalf("new market has transitions: %+v", m.Transitions)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCreate_CircleVisibilityRoundTrips(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	n := validNewMarket()
	n.Visibility = Visibility{Kind: VisibilityCircle, CircleID: "circle-123"}
	n.SettlementMode = SettlementGroupVote

	created, err := s.Create(ctx, n)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	after := mustGet(t, s, created.ID)
	if after.Visibility.Kind != VisibilityCircle || after.Visibility.CircleID != "circle-123" {
		t.Fatalf("visibility = %+v, want circle/circle-123", after.Visibility)
	}
	if after.SettlementMode != SettlementGroupVote {
		t.Fatalf("settlement mode = %s, want group_vote", after.SettlementMode)
	}
	if after.Currency != "NGN_PLAY" {
		t.Fatalf("currency = %q, want NGN_PLAY", after.Currency)
	}
}

// --- Pure unit tests (no Mongo) ---

func TestIsUnderFilled(t *testing.T) {
	cases := []struct {
		name     string
		outcomes []Outcome
		want     bool
	}{
		{"no stakes at all", []Outcome{{ID: "a"}, {ID: "b"}}, true},
		{"one outcome staked", []Outcome{{ID: "a", Pool: 100}, {ID: "b"}}, true},
		{"exactly two staked is not under-filled", []Outcome{{ID: "a", Pool: 1}, {ID: "b", Pool: 1}}, false},
		{"three of three staked", []Outcome{{ID: "a", Pool: 5}, {ID: "b", Pool: 5}, {ID: "c", Pool: 5}}, false},
		{"two of three staked", []Outcome{{ID: "a", Pool: 5}, {ID: "b", Pool: 5}, {ID: "c"}}, false},
		{"one of three staked", []Outcome{{ID: "a", Pool: 5}, {ID: "b"}, {ID: "c"}}, true},
		{"single kobo counts as staked", []Outcome{{ID: "a", Pool: 1}, {ID: "b", Pool: 1}}, false},
		{"empty outcomes", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnderFilled(tc.outcomes); got != tc.want {
				t.Fatalf("IsUnderFilled(%+v) = %v, want %v", tc.outcomes, got, tc.want)
			}
		})
	}
}

func TestHasOutcome(t *testing.T) {
	m := Market{Outcomes: []Outcome{{ID: "yes"}, {ID: "no"}}}
	if !m.HasOutcome("yes") || !m.HasOutcome("no") {
		t.Fatal("HasOutcome missed a present outcome")
	}
	if m.HasOutcome("maybe") || m.HasOutcome("") {
		t.Fatal("HasOutcome matched an absent outcome")
	}
}
