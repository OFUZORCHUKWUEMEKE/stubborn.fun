package stake

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/ledger"
	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/market"
)

func TestPlaceStake_HappyPath(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 10_000)

	res, err := h.svc.PlaceStake(ctx, Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 3_000,
	})
	if err != nil {
		t.Fatalf("place stake: %v", err)
	}

	if res.Replayed {
		t.Fatal("first stake reported as replay")
	}
	if res.Position.Amount != 3_000 || res.Position.OutcomeID != "yes" {
		t.Fatalf("position = %+v", res.Position)
	}
	if res.Position.Currency != "NGN_PLAY" {
		t.Fatalf("position currency = %q, want the market's NGN_PLAY", res.Position.Currency)
	}
	if res.TotalPool != 3_000 {
		t.Fatalf("total pool = %d, want 3000", res.TotalPool)
	}

	if got := h.balance(t, "alice"); got != 7_000 {
		t.Fatalf("wallet = %d, want 7000", got)
	}
	if got := h.escrowBalance(t, m.ID); got != 3_000 {
		t.Fatalf("escrow = %d, want 3000", got)
	}

	after := h.mustMarket(t, m.ID)
	for _, o := range after.Outcomes {
		want := int64(0)
		if o.ID == "yes" {
			want = 3_000
		}
		if o.Pool != want {
			t.Fatalf("outcome %s pool = %d, want %d", o.ID, o.Pool, want)
		}
	}

	// The ledger entry must be traceable back to the position.
	entries, err := h.ledger.EntriesForTxn(ctx, res.Position.TxnID())
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (debit + credit)", len(entries))
	}

	h.assertBalanced(t, m.ID)
}

func TestPlaceStake_AdditivePositions(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 10_000)

	// Same outcome twice, plus the other outcome: all additive, all
	// separate documents.
	for _, req := range []Request{
		{UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000},
		{UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 2_000},
		{UserID: "alice", MarketID: m.ID, OutcomeID: "no", Amount: 500},
	} {
		if _, err := h.svc.PlaceStake(ctx, req); err != nil {
			t.Fatalf("stake %+v: %v", req, err)
		}
	}

	if n := h.countPositions(t, m.ID); n != 3 {
		t.Fatalf("positions = %d, want 3", n)
	}

	byOutcome, err := h.svc.UserStakeByOutcome(ctx, m.ID, "alice")
	if err != nil {
		t.Fatalf("user stake by outcome: %v", err)
	}
	if byOutcome["yes"] != 3_000 || byOutcome["no"] != 500 {
		t.Fatalf("aggregate = %+v, want yes:3000 no:500", byOutcome)
	}

	after := h.mustMarket(t, m.ID)
	if after.TotalPool != 3_500 {
		t.Fatalf("total pool = %d, want 3500", after.TotalPool)
	}
	if got := h.balance(t, "alice"); got != 6_500 {
		t.Fatalf("wallet = %d, want 6500", got)
	}
	h.assertBalanced(t, m.ID)
}

// TestPlaceStake_ConcurrentKeepsPoolsConsistent is the primary gate: 200
// concurrent stakes across two outcomes, after which the position sum, the
// market's cached pools, and the ledger escrow balance must all agree
// exactly, and every wallet must be debited exactly once per stake.
func TestPlaceStake_ConcurrentKeepsPoolsConsistent(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)

	const stakers = 20
	const perStaker = 10 // 200 stakes total
	const amount = 100
	const funded = perStaker * amount

	for i := 0; i < stakers; i++ {
		h.fund(t, fmt.Sprintf("user-%d", i), funded)
	}

	var wg sync.WaitGroup
	var succeeded, failed int64
	for i := 0; i < stakers; i++ {
		for j := 0; j < perStaker; j++ {
			wg.Add(1)
			go func(i, j int) {
				defer wg.Done()
				outcome := "yes"
				if j%2 == 1 {
					outcome = "no"
				}
				_, err := h.svc.PlaceStake(ctx, Request{
					UserID: fmt.Sprintf("user-%d", i), MarketID: m.ID,
					OutcomeID: outcome, Amount: amount,
				})
				if err != nil {
					atomic.AddInt64(&failed, 1)
					t.Errorf("user-%d stake %d: %v", i, j, err)
					return
				}
				atomic.AddInt64(&succeeded, 1)
			}(i, j)
		}
	}
	wg.Wait()

	if failed != 0 {
		t.Fatalf("%d stakes failed, want 0", failed)
	}
	if succeeded != stakers*perStaker {
		t.Fatalf("succeeded = %d, want %d", succeeded, stakers*perStaker)
	}

	d := h.assertBalanced(t, m.ID)
	wantTotal := int64(stakers * perStaker * amount)
	if d.MarketTotal != wantTotal {
		t.Fatalf("total pool = %d, want %d", d.MarketTotal, wantTotal)
	}

	// Every wallet fully drained, exactly once per stake.
	for i := 0; i < stakers; i++ {
		if got := h.balance(t, fmt.Sprintf("user-%d", i)); got != 0 {
			t.Fatalf("user-%d wallet = %d, want 0 (exactly one debit per stake)", i, got)
		}
	}

	if n := h.countPositions(t, m.ID); n != stakers*perStaker {
		t.Fatalf("positions = %d, want %d", n, stakers*perStaker)
	}
	// Two entries (debit + credit) per stake, no more.
	if n := h.countLedgerEntries(t, m.ID); n != 2*stakers*perStaker {
		t.Fatalf("ledger entries = %d, want %d", n, 2*stakers*perStaker)
	}
}

// TestPlaceStake_RacingCloseTransition is the atomicity gate against a
// close landing mid-flight. The close is an admin/creator force-close
// while the clock is still before close_at, because that is the only way
// to produce a genuine race: staking and the scheduler read the same
// clock, so at the deadline the close_at check rejects every stake before
// the state transition is even reached (pinned separately below).
//
// Each stake must either land completely or fail with ErrMarketClosed.
// There must never be a debited user without a position, never a position
// without its escrow credit, and the three-way invariant must hold no
// matter where the close fell among the in-flight stakes.
func TestPlaceStake_RacingCloseTransition(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)

	const warmup = 5
	const racers = 55
	const total = warmup + racers
	const amount = 100
	for i := 0; i < total; i++ {
		h.fund(t, fmt.Sprintf("racer-%d", i), amount)
	}

	stake := func(i int) error {
		_, err := h.svc.PlaceStake(ctx, Request{
			UserID: fmt.Sprintf("racer-%d", i), MarketID: m.ID,
			OutcomeID: "yes", Amount: amount,
		})
		return err
	}

	// Warm-up stakes land before the close is even attempted, so the
	// invariant is always checked against a non-empty set — otherwise a
	// close that wins every race would make this test vacuous.
	var landed, rejected int64
	for i := 0; i < warmup; i++ {
		if err := stake(i); err != nil {
			t.Fatalf("warmup stake %d: %v", i, err)
		}
		landed++
	}

	// Now fire the remaining stakes, and close once some of them have
	// already committed. Firing the close at the same instant as the
	// stakes is not enough: a single findOneAndUpdate always beats a
	// multi-document transaction, so the close would win every race and
	// no stake would ever commit concurrently with it. Waiting for a
	// threshold guarantees stakes commit on both sides of the close.
	const closeAfter = warmup + 10

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			n, err := h.svc.positions.CountDocuments(ctx, bson.M{"market_id": m.ID})
			if err != nil {
				t.Errorf("count during race: %v", err)
				return
			}
			if n >= closeAfter {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := h.markets.Close(ctx, m.ID, market.ActorAdmin); err != nil {
			t.Errorf("admin close: %v", err)
		}
	}()

	for i := warmup; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch err := stake(i); {
			case err == nil:
				atomic.AddInt64(&landed, 1)
			case errors.Is(err, market.ErrMarketClosed):
				atomic.AddInt64(&rejected, 1)
			default:
				t.Errorf("racer-%d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if landed+rejected != total {
		t.Fatalf("landed(%d) + rejected(%d) != %d", landed, rejected, total)
	}
	// Both of these must be non-zero, or the test is not exercising a
	// concurrent close at all: stakes must have committed on both sides
	// of the close transition.
	if landed <= warmup {
		t.Fatalf("landed = %d: no racing stake committed before the close, so nothing raced", landed)
	}
	if rejected == 0 {
		t.Fatalf("rejected = 0: the close never landed mid-flight, so nothing raced")
	}
	t.Logf("landed=%d (%d of them racing) rejected=%d", landed, landed-warmup, rejected)

	if after := h.mustMarket(t, m.ID); after.State != market.StateClosed {
		t.Fatalf("state = %s, want CLOSED", after.State)
	}

	// The invariant: whatever landed agrees across positions, cached
	// pools, and escrow. A half-committed stake breaks exactly this.
	d := h.assertBalanced(t, m.ID)
	if d.PositionsTotal != landed*amount {
		t.Fatalf("positions total = %d, want %d (landed stakes)", d.PositionsTotal, landed*amount)
	}
	if n := h.countPositions(t, m.ID); n != landed {
		t.Fatalf("positions = %d, want %d", n, landed)
	}
	if n := h.countLedgerEntries(t, m.ID); n != 2*landed {
		t.Fatalf("ledger entries = %d, want %d (two per landed stake)", n, 2*landed)
	}

	// No debit without a position, and no position without a debit.
	debited := int64(0)
	for i := 0; i < total; i++ {
		switch bal := h.balance(t, fmt.Sprintf("racer-%d", i)); bal {
		case 0:
			debited++
		case amount:
			// untouched, as a rejected stake must be
		default:
			t.Fatalf("racer-%d has partial balance %d: a stake half-committed", i, bal)
		}
	}
	if debited != landed {
		t.Fatalf("%d users debited but %d positions exist", debited, landed)
	}
}

// TestPlaceStake_SchedulerCloseAtDeadlineRejectsAll pins the property the
// race test above relies on: because PlaceStake and the scheduler read the
// same clock, once close_at is reached every stake is rejected on the
// deadline check regardless of whether the scheduler has ticked yet. The
// scheduler's state change and the stake deadline check cannot disagree.
func TestPlaceStake_SchedulerCloseAtDeadlineRejectsAll(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	const stakers = 30
	const amount = 100
	for i := 0; i < stakers; i++ {
		h.fund(t, fmt.Sprintf("late-%d", i), amount)
	}

	sched := market.NewScheduler(h.markets, h.clock, time.Second, nil)
	h.clock.Set(m.CloseAt)

	var wg sync.WaitGroup
	var landed, rejected int64

	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := sched.Tick(ctx); err != nil {
			t.Errorf("scheduler tick: %v", err)
		}
	}()
	for i := 0; i < stakers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.svc.PlaceStake(ctx, Request{
				UserID: fmt.Sprintf("late-%d", i), MarketID: m.ID,
				OutcomeID: "yes", Amount: amount,
			})
			switch {
			case err == nil:
				atomic.AddInt64(&landed, 1)
			case errors.Is(err, market.ErrMarketClosed):
				atomic.AddInt64(&rejected, 1)
			default:
				t.Errorf("late-%d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if landed != 0 {
		t.Fatalf("landed = %d, want 0: no stake may land at or after close_at", landed)
	}
	if rejected != stakers {
		t.Fatalf("rejected = %d, want %d", rejected, stakers)
	}
	if n := h.countPositions(t, m.ID); n != 0 {
		t.Fatalf("positions = %d, want 0", n)
	}
	for i := 0; i < stakers; i++ {
		if bal := h.balance(t, fmt.Sprintf("late-%d", i)); bal != amount {
			t.Fatalf("late-%d balance = %d, want untouched %d", i, bal, amount)
		}
	}
}

// TestPlaceStake_InsufficientFundsRollsBack proves the failure unwinds
// every write: no position, no pool drift, no ledger entry.
func TestPlaceStake_InsufficientFundsRollsBack(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 5_000)
	h.fund(t, "bob", 1_000)

	// A successful stake first, so we can prove the failed one does not
	// disturb existing state.
	if _, err := h.svc.PlaceStake(ctx, Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 2_000,
	}); err != nil {
		t.Fatalf("seed stake: %v", err)
	}
	before := h.mustMarket(t, m.ID)

	_, err := h.svc.PlaceStake(ctx, Request{
		UserID: "bob", MarketID: m.ID, OutcomeID: "no", Amount: 5_000,
	})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}

	after := h.mustMarket(t, m.ID)
	if after.TotalPool != before.TotalPool {
		t.Fatalf("total pool drifted: %d -> %d", before.TotalPool, after.TotalPool)
	}
	for i, o := range after.Outcomes {
		if o.Pool != before.Outcomes[i].Pool {
			t.Fatalf("outcome %s pool drifted: %d -> %d", o.ID, before.Outcomes[i].Pool, o.Pool)
		}
	}
	if got := h.balance(t, "bob"); got != 1_000 {
		t.Fatalf("bob wallet = %d, want untouched 1000", got)
	}
	if n := h.countPositions(t, m.ID); n != 1 {
		t.Fatalf("positions = %d, want 1 (the failed stake must leave none)", n)
	}
	if n := h.countLedgerEntries(t, m.ID); n != 2 {
		t.Fatalf("ledger entries = %d, want 2 (only the successful stake)", n)
	}
	h.assertBalanced(t, m.ID)
}

// --- Idempotency ---

func TestPlaceStake_IdempotencyKeyReplay(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 10_000)

	req := Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 2_500,
		IdempotencyKey: "client-req-1",
	}

	first, err := h.svc.PlaceStake(ctx, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Replayed {
		t.Fatal("first call reported as replay")
	}

	for i := 0; i < 3; i++ {
		again, err := h.svc.PlaceStake(ctx, req)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !again.Replayed {
			t.Fatalf("replay %d not flagged as replayed", i)
		}
		if again.Position.ID != first.Position.ID {
			t.Fatalf("replay %d returned a different position: %s vs %s",
				i, again.Position.ID.Hex(), first.Position.ID.Hex())
		}
	}

	if got := h.balance(t, "alice"); got != 7_500 {
		t.Fatalf("wallet = %d, want 7500 (exactly one debit)", got)
	}
	if n := h.countPositions(t, m.ID); n != 1 {
		t.Fatalf("positions = %d, want 1", n)
	}
	after := h.mustMarket(t, m.ID)
	if after.TotalPool != 2_500 {
		t.Fatalf("total pool = %d, want 2500 (incremented once)", after.TotalPool)
	}
	h.assertBalanced(t, m.ID)
}

// TestPlaceStake_ConcurrentIdempotencyKeyReplay is the harsher version:
// the same key fired simultaneously, where a pre-check cannot help and
// only the unique index can prevent a double stake.
func TestPlaceStake_ConcurrentIdempotencyKeyReplay(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 10_000)

	req := Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
		IdempotencyKey: "client-req-concurrent",
	}

	const goroutines = 25
	var wg sync.WaitGroup
	ids := make([]bson.ObjectID, goroutines)
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := h.svc.PlaceStake(ctx, req)
			errs[i] = err
			ids[i] = res.Position.ID
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	// Every caller must have been handed the same position.
	for i := 1; i < goroutines; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("goroutine %d got position %s, goroutine 0 got %s", i, ids[i].Hex(), ids[0].Hex())
		}
	}

	if n := h.countPositions(t, m.ID); n != 1 {
		t.Fatalf("positions = %d, want exactly 1", n)
	}
	if got := h.balance(t, "alice"); got != 9_000 {
		t.Fatalf("wallet = %d, want 9000 (exactly one debit)", got)
	}
	if n := h.countLedgerEntries(t, m.ID); n != 2 {
		t.Fatalf("ledger entries = %d, want 2", n)
	}
	h.assertBalanced(t, m.ID)
}

func TestPlaceStake_DistinctKeysAreAdditive(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 10_000)

	for _, key := range []string{"k1", "k2", "k3"} {
		if _, err := h.svc.PlaceStake(ctx, Request{
			UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
			IdempotencyKey: key,
		}); err != nil {
			t.Fatalf("stake %s: %v", key, err)
		}
	}

	if n := h.countPositions(t, m.ID); n != 3 {
		t.Fatalf("positions = %d, want 3", n)
	}
	if got := h.balance(t, "alice"); got != 7_000 {
		t.Fatalf("wallet = %d, want 7000", got)
	}
	h.assertBalanced(t, m.ID)
}

// TestPlaceStake_IdempotencyKeyIsPerUser confirms the unique index is
// scoped to the user: two users may legitimately send the same key.
func TestPlaceStake_IdempotencyKeyIsPerUser(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 5_000)
	h.fund(t, "bob", 5_000)

	for _, user := range []string{"alice", "bob"} {
		if _, err := h.svc.PlaceStake(ctx, Request{
			UserID: user, MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
			IdempotencyKey: "same-key",
		}); err != nil {
			t.Fatalf("%s: %v", user, err)
		}
	}

	if n := h.countPositions(t, m.ID); n != 2 {
		t.Fatalf("positions = %d, want 2 (key is scoped per user)", n)
	}
	h.assertBalanced(t, m.ID)
}

// --- Rejections ---

func TestPlaceStake_RejectsNonOpenStates(t *testing.T) {
	for _, state := range []market.State{
		market.StateDraft, market.StateClosed, market.StateSettling,
		market.StateSettlePending, market.StateSettled, market.StateDisputed,
		market.StateVoting, market.StateCancelled, market.StateRefunding,
		market.StateRefunded,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t, allowMembership{})
			ctx := context.Background()

			m := h.openMarket(t)
			h.fund(t, "alice", 5_000)
			if _, err := h.db.Collection(market.MarketsCollection).
				UpdateByID(ctx, m.ID, bson.M{"$set": bson.M{"state": state}}); err != nil {
				t.Fatalf("force state: %v", err)
			}

			_, err := h.svc.PlaceStake(ctx, Request{
				UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
			})
			if !errors.Is(err, market.ErrMarketClosed) {
				t.Fatalf("err = %v, want ErrMarketClosed", err)
			}
			if got := h.balance(t, "alice"); got != 5_000 {
				t.Fatalf("wallet = %d, want untouched 5000", got)
			}
			if n := h.countPositions(t, m.ID); n != 0 {
				t.Fatalf("positions = %d, want 0", n)
			}
		})
	}
}

// TestPlaceStake_RejectsPastDeadlineWhileStillOpen covers the scheduler
// gap: close_at has passed but the scheduler has not ticked yet, so the
// state is still OPEN. Checking state alone would wrongly accept this.
func TestPlaceStake_RejectsPastDeadlineWhileStillOpen(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 5_000)

	// Past the deadline, but nothing has closed the market.
	h.clock.Set(m.CloseAt.Add(time.Second))
	if got := h.mustMarket(t, m.ID); got.State != market.StateOpen {
		t.Fatalf("precondition: state = %s, want still OPEN", got.State)
	}

	_, err := h.svc.PlaceStake(ctx, Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	})
	if !errors.Is(err, market.ErrMarketClosed) {
		t.Fatalf("err = %v, want ErrMarketClosed", err)
	}
	if got := h.balance(t, "alice"); got != 5_000 {
		t.Fatalf("wallet = %d, want untouched", got)
	}
	if n := h.countPositions(t, m.ID); n != 0 {
		t.Fatalf("positions = %d, want 0", n)
	}
}

// TestPlaceStake_ExactlyAtDeadlineIsRejected pins the boundary: close_at
// is exclusive for staking (the market closes at that instant), matching
// the scheduler's close_at <= now.
func TestPlaceStake_ExactlyAtDeadlineIsRejected(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 5_000)

	h.clock.Set(m.CloseAt.Add(-time.Nanosecond))
	if _, err := h.svc.PlaceStake(ctx, Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	}); err != nil {
		t.Fatalf("1ns before close_at should succeed, got %v", err)
	}

	h.clock.Set(m.CloseAt)
	_, err := h.svc.PlaceStake(ctx, Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	})
	if !errors.Is(err, market.ErrMarketClosed) {
		t.Fatalf("exactly at close_at: err = %v, want ErrMarketClosed", err)
	}
	h.assertBalanced(t, m.ID)
}

func TestPlaceStake_RejectsUnknownOutcome(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 5_000)

	_, err := h.svc.PlaceStake(ctx, Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "maybe", Amount: 1_000,
	})
	if !errors.Is(err, market.ErrUnknownOutcome) {
		t.Fatalf("err = %v, want ErrUnknownOutcome", err)
	}
	if got := h.balance(t, "alice"); got != 5_000 {
		t.Fatalf("wallet = %d, want untouched", got)
	}
}

func TestPlaceStake_RejectsMissingMarket(t *testing.T) {
	h := newHarness(t, allowMembership{})

	h.fund(t, "alice", 5_000)
	_, err := h.svc.PlaceStake(context.Background(), Request{
		UserID: "alice", MarketID: bson.NewObjectID(), OutcomeID: "yes", Amount: 1_000,
	})
	if !errors.Is(err, market.ErrMarketNotFound) {
		t.Fatalf("err = %v, want ErrMarketNotFound", err)
	}
}

func TestPlaceStake_AmountBounds(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()
	cfg := DefaultConfig()

	m := h.openMarket(t)
	h.fund(t, "alice", cfg.MaxStake*2)

	cases := []struct {
		name   string
		amount int64
		want   error
	}{
		{"zero", 0, ErrStakeTooSmall},
		{"negative", -100, ErrStakeTooSmall},
		{"one below min", cfg.MinStake - 1, ErrStakeTooSmall},
		{"exactly min", cfg.MinStake, nil},
		{"exactly max", cfg.MaxStake, nil},
		{"one above max", cfg.MaxStake + 1, ErrStakeTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.PlaceStake(ctx, Request{
				UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: tc.amount,
			})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPlaceStake_RejectsEmptyUserID(t *testing.T) {
	h := newHarness(t, allowMembership{})
	m := h.openMarket(t)
	_, err := h.svc.PlaceStake(context.Background(), Request{
		UserID: "", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	})
	if !errors.Is(err, ErrEmptyUserID) {
		t.Fatalf("err = %v, want ErrEmptyUserID", err)
	}
}

// --- Circle visibility ---

func TestPlaceStake_CircleMarketRejectsNonMember(t *testing.T) {
	h := newHarness(t, memberSet{"insider": true})
	ctx := context.Background()

	m := h.openMarketWithVisibility(t, market.Visibility{
		Kind: market.VisibilityCircle, CircleID: "circle-1",
	})
	h.fund(t, "outsider", 5_000)
	h.fund(t, "insider", 5_000)

	_, err := h.svc.PlaceStake(ctx, Request{
		UserID: "outsider", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	})
	if !errors.Is(err, ErrNotCircleMember) {
		t.Fatalf("outsider: err = %v, want ErrNotCircleMember", err)
	}
	if got := h.balance(t, "outsider"); got != 5_000 {
		t.Fatalf("outsider wallet = %d, want untouched", got)
	}
	if n := h.countPositions(t, m.ID); n != 0 {
		t.Fatalf("positions = %d, want 0", n)
	}

	if _, err := h.svc.PlaceStake(ctx, Request{
		UserID: "insider", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	}); err != nil {
		t.Fatalf("insider: %v", err)
	}
	h.assertBalanced(t, m.ID)
}

// TestPlaceStake_DefaultMembershipFailsClosed is the security-relevant
// default: until the circle package exists, circle markets must reject
// everyone rather than admit everyone.
func TestPlaceStake_DefaultMembershipFailsClosed(t *testing.T) {
	h := newHarness(t, DenyAllMembership{})
	ctx := context.Background()

	m := h.openMarketWithVisibility(t, market.Visibility{
		Kind: market.VisibilityCircle, CircleID: "circle-1",
	})
	h.fund(t, "anyone", 5_000)

	if _, err := h.svc.PlaceStake(ctx, Request{
		UserID: "anyone", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	}); !errors.Is(err, ErrNotCircleMember) {
		t.Fatalf("err = %v, want ErrNotCircleMember", err)
	}
}

// TestPlaceStake_NilMembershipCheckerFailsClosed guards the constructor:
// forgetting to wire a checker must not silently allow all circle stakes.
func TestPlaceStake_NilMembershipCheckerFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	m := h.openMarketWithVisibility(t, market.Visibility{
		Kind: market.VisibilityCircle, CircleID: "circle-1",
	})
	h.fund(t, "anyone", 5_000)

	if _, err := h.svc.PlaceStake(ctx, Request{
		UserID: "anyone", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	}); !errors.Is(err, ErrNotCircleMember) {
		t.Fatalf("err = %v, want ErrNotCircleMember", err)
	}
}

// TestPlaceStake_MembershipErrorDoesNotStake makes sure a failing
// membership service rejects the stake rather than falling open.
func TestPlaceStake_MembershipErrorDoesNotStake(t *testing.T) {
	sentinel := errors.New("circle service unavailable")
	h := newHarness(t, failingMembership{err: sentinel})
	ctx := context.Background()

	m := h.openMarketWithVisibility(t, market.Visibility{
		Kind: market.VisibilityCircle, CircleID: "circle-1",
	})
	h.fund(t, "alice", 5_000)

	_, err := h.svc.PlaceStake(ctx, Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the membership error", err)
	}
	if got := h.balance(t, "alice"); got != 5_000 {
		t.Fatalf("wallet = %d, want untouched", got)
	}
}

func TestPlaceStake_PublicMarketNeedsNoMembership(t *testing.T) {
	h := newHarness(t, DenyAllMembership{})
	ctx := context.Background()

	m := h.openMarket(t) // public
	h.fund(t, "anyone", 5_000)

	if _, err := h.svc.PlaceStake(ctx, Request{
		UserID: "anyone", MarketID: m.ID, OutcomeID: "yes", Amount: 1_000,
	}); err != nil {
		t.Fatalf("public market stake rejected: %v", err)
	}
	h.assertBalanced(t, m.ID)
}

// --- Reconciliation ---

func TestReconcileMarket_DetectsPoolCorruption(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m := h.openMarket(t)
	h.fund(t, "alice", 5_000)
	if _, err := h.svc.PlaceStake(ctx, Request{
		UserID: "alice", MarketID: m.ID, OutcomeID: "yes", Amount: 2_000,
	}); err != nil {
		t.Fatalf("stake: %v", err)
	}
	h.assertBalanced(t, m.ID)

	// Corrupt the cached pool behind PlaceStake's back.
	if _, err := h.db.Collection(market.MarketsCollection).UpdateByID(ctx, m.ID, bson.M{
		"$set": bson.M{"total_pool": int64(999_999), "outcomes.0.pool": int64(999_999)},
	}); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	d, err := h.svc.ReconcileMarket(ctx, m.ID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if d.Balanced() {
		t.Fatal("reconcile reported balanced despite corrupted pools")
	}
	if d.PositionsTotal != 2_000 || d.EscrowBalance != 2_000 || d.MarketTotal != 999_999 {
		t.Fatalf("discrepancy = %+v", d)
	}
	if d.OutcomeDrift["yes"] != 2_000-999_999 {
		t.Fatalf("outcome drift = %+v", d.OutcomeDrift)
	}
}

// --- Queries ---

func TestPositionQueries(t *testing.T) {
	h := newHarness(t, allowMembership{})
	ctx := context.Background()

	m1 := h.openMarket(t)
	m2 := h.openMarket(t)
	h.fund(t, "alice", 10_000)
	h.fund(t, "bob", 10_000)

	stakes := []Request{
		{UserID: "alice", MarketID: m1.ID, OutcomeID: "yes", Amount: 1_000},
		{UserID: "alice", MarketID: m1.ID, OutcomeID: "no", Amount: 500},
		{UserID: "alice", MarketID: m2.ID, OutcomeID: "yes", Amount: 300},
		{UserID: "bob", MarketID: m1.ID, OutcomeID: "yes", Amount: 2_000},
	}
	for _, r := range stakes {
		if _, err := h.svc.PlaceStake(ctx, r); err != nil {
			t.Fatalf("stake %+v: %v", r, err)
		}
	}

	alice, err := h.svc.PositionsByUser(ctx, "alice", 0)
	if err != nil {
		t.Fatalf("positions by user: %v", err)
	}
	if len(alice) != 3 {
		t.Fatalf("alice positions = %d, want 3", len(alice))
	}

	inM1, err := h.svc.PositionsByMarket(ctx, m1.ID, 0)
	if err != nil {
		t.Fatalf("positions by market: %v", err)
	}
	if len(inM1) != 3 {
		t.Fatalf("m1 positions = %d, want 3", len(inM1))
	}

	aliceM1, err := h.svc.PositionsByUserInMarket(ctx, m1.ID, "alice")
	if err != nil {
		t.Fatalf("positions by user in market: %v", err)
	}
	if len(aliceM1) != 2 {
		t.Fatalf("alice m1 positions = %d, want 2", len(aliceM1))
	}

	byOutcome, err := h.svc.StakeByOutcome(ctx, m1.ID)
	if err != nil {
		t.Fatalf("stake by outcome: %v", err)
	}
	if byOutcome["yes"] != 3_000 || byOutcome["no"] != 500 {
		t.Fatalf("m1 by outcome = %+v, want yes:3000 no:500", byOutcome)
	}

	participants, err := h.svc.ParticipantCount(ctx, m1.ID)
	if err != nil {
		t.Fatalf("participant count: %v", err)
	}
	if participants != 2 {
		t.Fatalf("participants = %d, want 2 distinct users", participants)
	}

	empty, err := h.svc.ParticipantCount(ctx, bson.NewObjectID())
	if err != nil {
		t.Fatalf("participant count on empty market: %v", err)
	}
	if empty != 0 {
		t.Fatalf("empty participants = %d, want 0", empty)
	}

	// Limit is honoured.
	limited, err := h.svc.PositionsByUser(ctx, "alice", 2)
	if err != nil {
		t.Fatalf("limited: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limited = %d, want 2", len(limited))
	}
}
