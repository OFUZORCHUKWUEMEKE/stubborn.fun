package market

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/platform"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// openStakedMarket creates a market, publishes it, and stakes the given
// pools — the normal precondition for an auto-close.
func openStakedMarket(t *testing.T, s *Store, closeAt time.Time, pools map[string]int64) Market {
	t.Helper()
	ctx := context.Background()

	n := validNewMarket()
	n.CloseAt = closeAt
	m, err := s.Create(ctx, n)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Publish(ctx, m.ID, "user-creator"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(pools) > 0 {
		setPools(t, s, m.ID, pools)
	}
	return mustGet(t, s, m.ID)
}

// TestScheduler_ClosesExactlyOnTime walks a fake clock up to and past
// close_at, asserting the market is not closed early and closes on the
// first tick at or after its deadline.
func TestScheduler_ClosesExactlyOnTime(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()
	sched := NewScheduler(s, clock, time.Second, quietLogger())

	closeAt := baseTime.Add(time.Hour)
	m := openStakedMarket(t, s, closeAt, map[string]int64{"yes": 1000, "no": 500})

	// Well before the deadline: nothing happens.
	res, err := sched.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Closed != 0 || res.Cancelled != 0 {
		t.Fatalf("tick before close_at acted: %+v", res)
	}

	// One nanosecond before the deadline: still nothing. This is the
	// "must not close early" boundary.
	clock.Set(closeAt.Add(-time.Nanosecond))
	res, err = sched.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Closed != 0 {
		t.Fatalf("closed 1ns early: %+v", res)
	}
	if got := mustGet(t, s, m.ID); got.State != StateOpen {
		t.Fatalf("state = %s, want still OPEN", got.State)
	}

	// Exactly at the deadline: closes (the poll is close_at <= now).
	clock.Set(closeAt)
	res, err = sched.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Closed != 1 || res.Cancelled != 0 {
		t.Fatalf("tick at close_at = %+v, want exactly 1 closed", res)
	}

	after := mustGet(t, s, m.ID)
	if after.State != StateClosed {
		t.Fatalf("state = %s, want CLOSED", after.State)
	}
	last := after.Transitions[len(after.Transitions)-1]
	if last.Actor != ActorScheduler {
		t.Fatalf("closing actor = %q, want %q", last.Actor, ActorScheduler)
	}
	if !last.At.Equal(closeAt) {
		t.Fatalf("transition timestamp = %s, want fake clock time %s", last.At, closeAt)
	}
}

// TestScheduler_CancelsUnderFilled covers the CANCELLED branch and, most
// importantly, the boundary: exactly two staked outcomes must CLOSE, not
// cancel.
func TestScheduler_CancelsUnderFilled(t *testing.T) {
	cases := []struct {
		name      string
		pools     map[string]int64
		wantState State
	}{
		{"no stakes", nil, StateCancelled},
		{"only one outcome staked", map[string]int64{"yes": 5000}, StateCancelled},
		{"exactly two outcomes staked closes", map[string]int64{"yes": 1, "no": 1}, StateClosed},
		{"both outcomes heavily staked closes", map[string]int64{"yes": 900000, "no": 100000}, StateClosed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, clock := newTestStore(t)
			ctx := context.Background()
			sched := NewScheduler(s, clock, time.Second, quietLogger())

			closeAt := baseTime.Add(time.Hour)
			m := openStakedMarket(t, s, closeAt, tc.pools)

			clock.Set(closeAt)
			res, err := sched.Tick(ctx)
			if err != nil {
				t.Fatalf("tick: %v", err)
			}

			after := mustGet(t, s, m.ID)
			if after.State != tc.wantState {
				t.Fatalf("state = %s, want %s (result %+v)", after.State, tc.wantState, res)
			}
			if tc.wantState == StateCancelled {
				if res.Cancelled != 1 {
					t.Fatalf("result = %+v, want 1 cancelled", res)
				}
				last := after.Transitions[len(after.Transitions)-1]
				if last.Reason == "" {
					t.Fatal("cancellation must record a reason for support/debugging")
				}
			} else if res.Closed != 1 {
				t.Fatalf("result = %+v, want 1 closed", res)
			}
		})
	}
}

// TestScheduler_IgnoresNonOpenMarkets makes sure the poll is state-scoped:
// a DRAFT market whose close_at has passed must not be touched (it was
// never published), and neither must an already-CLOSED one.
func TestScheduler_IgnoresNonOpenMarkets(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()
	sched := NewScheduler(s, clock, time.Second, quietLogger())

	closeAt := baseTime.Add(time.Hour)

	// Never published.
	draft := mustCreate(t, s)

	// Published then already closed by someone else.
	closed := openStakedMarket(t, s, closeAt, map[string]int64{"yes": 100, "no": 100})
	if _, err := s.Close(ctx, closed.ID, ActorAdmin); err != nil {
		t.Fatalf("pre-close: %v", err)
	}

	// Already settled, far in the past.
	settled := openStakedMarket(t, s, closeAt, map[string]int64{"yes": 100, "no": 100})
	forceState(t, s, settled.ID, StateSettled)

	clock.Set(closeAt.Add(24 * time.Hour))
	res, err := sched.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Closed != 0 || res.Cancelled != 0 || res.Skipped != 0 {
		t.Fatalf("tick acted on non-OPEN markets: %+v", res)
	}

	if got := mustGet(t, s, draft.ID); got.State != StateDraft {
		t.Fatalf("draft state = %s, want untouched DRAFT", got.State)
	}
	if got := mustGet(t, s, closed.ID); got.State != StateClosed {
		t.Fatalf("closed state = %s, want untouched CLOSED", got.State)
	}
	if got := mustGet(t, s, settled.ID); got.State != StateSettled {
		t.Fatalf("settled state = %s, want untouched SETTLED", got.State)
	}
}

// TestScheduler_RestartRecovery is the crash-safety gate: a market goes
// overdue while no scheduler is ticking (simulating a process that died
// before the deadline), then a brand-new Scheduler instance — as a
// restarted process would construct — closes it on its very first tick.
// Nothing is missed because Tick re-derives the overdue set from the
// database rather than from in-memory timers.
func TestScheduler_RestartRecovery(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()

	closeAt := baseTime.Add(time.Hour)

	overdueStaked := openStakedMarket(t, s, closeAt, map[string]int64{"yes": 700, "no": 300})
	overdueUnderFilled := openStakedMarket(t, s, closeAt.Add(time.Minute), map[string]int64{"yes": 700})

	// "Instance 1" exists but never gets to tick — the process dies here.
	dead := NewScheduler(s, clock, time.Second, quietLogger())
	_ = dead

	// Time passes with nothing running.
	clock.Set(closeAt.Add(6 * time.Hour))

	// "Instance 2": a fresh process, fresh Scheduler, same database.
	revived := NewScheduler(s, clock, time.Second, quietLogger())
	res, err := revived.Tick(ctx)
	if err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	if res.Closed != 1 || res.Cancelled != 1 {
		t.Fatalf("recovery tick = %+v, want 1 closed + 1 cancelled", res)
	}

	if got := mustGet(t, s, overdueStaked.ID); got.State != StateClosed {
		t.Fatalf("staked market state = %s, want CLOSED after recovery", got.State)
	}
	if got := mustGet(t, s, overdueUnderFilled.ID); got.State != StateCancelled {
		t.Fatalf("under-filled market state = %s, want CANCELLED after recovery", got.State)
	}
}

// TestScheduler_SecondTickIsIdempotent proves a repeated tick over the
// same window does not double-transition: the second pass finds nothing
// because the markets are no longer OPEN.
func TestScheduler_SecondTickIsIdempotent(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()
	sched := NewScheduler(s, clock, time.Second, quietLogger())

	closeAt := baseTime.Add(time.Hour)
	m := openStakedMarket(t, s, closeAt, map[string]int64{"yes": 100, "no": 100})

	clock.Set(closeAt)
	if _, err := sched.Tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	res, err := sched.Tick(ctx)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if res.Closed != 0 || res.Cancelled != 0 || res.Skipped != 0 {
		t.Fatalf("second tick = %+v, want no-op", res)
	}

	after := mustGet(t, s, m.ID)
	closes := 0
	for _, tr := range after.Transitions {
		if tr.To == StateClosed {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("logged %d CLOSED transitions across two ticks, want 1", closes)
	}
}

// TestScheduler_ConcurrentSchedulersEachMarketClosedOnce simulates two
// instances (or an overlapping slow tick) racing the same backlog: every
// market must end up transitioned exactly once, with the losers reported
// as Skipped rather than as errors.
func TestScheduler_ConcurrentSchedulersEachMarketClosedOnce(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()

	closeAt := baseTime.Add(time.Hour)
	const numMarkets = 10
	ids := make([]bson.ObjectID, 0, numMarkets)
	for i := 0; i < numMarkets; i++ {
		m := openStakedMarket(t, s, closeAt, map[string]int64{"yes": 100, "no": 100})
		ids = append(ids, m.ID)
	}
	clock.Set(closeAt)

	const instances = 4
	type outcome struct {
		res TickResult
		err error
	}
	results := make(chan outcome, instances)
	for i := 0; i < instances; i++ {
		go func() {
			sched := NewScheduler(s, clock, time.Second, quietLogger())
			res, err := sched.Tick(ctx)
			results <- outcome{res, err}
		}()
	}

	totalClosed, totalSkipped := 0, 0
	for i := 0; i < instances; i++ {
		o := <-results
		if o.err != nil {
			t.Fatalf("instance tick: %v", o.err)
		}
		totalClosed += o.res.Closed
		totalSkipped += o.res.Skipped
	}

	if totalClosed != numMarkets {
		t.Fatalf("total closed = %d, want %d", totalClosed, numMarkets)
	}

	for _, id := range ids {
		after := mustGet(t, s, id)
		if after.State != StateClosed {
			t.Fatalf("market %s state = %s, want CLOSED", id.Hex(), after.State)
		}
		closes := 0
		for _, tr := range after.Transitions {
			if tr.To == StateClosed {
				closes++
			}
		}
		if closes != 1 {
			t.Fatalf("market %s logged %d closes, want 1", id.Hex(), closes)
		}
	}
	t.Logf("closed=%d skipped=%d across %d instances", totalClosed, totalSkipped, instances)
}

// TestScheduler_HandlesBacklogInOneTick covers post-downtime drain: many
// markets overdue at once are all handled.
func TestScheduler_HandlesBacklogInOneTick(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()
	sched := NewScheduler(s, clock, time.Second, quietLogger())

	closeAt := baseTime.Add(time.Hour)
	const staked = 7
	const underFilled = 3
	for i := 0; i < staked; i++ {
		openStakedMarket(t, s, closeAt.Add(time.Duration(i)*time.Second), map[string]int64{"yes": 10, "no": 10})
	}
	for i := 0; i < underFilled; i++ {
		openStakedMarket(t, s, closeAt.Add(time.Duration(i)*time.Second), map[string]int64{"yes": 10})
	}

	clock.Set(closeAt.Add(time.Hour))
	res, err := sched.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Closed != staked {
		t.Fatalf("closed = %d, want %d", res.Closed, staked)
	}
	if res.Cancelled != underFilled {
		t.Fatalf("cancelled = %d, want %d", res.Cancelled, underFilled)
	}
}

// TestScheduler_RunTicksImmediatelyAndStopsOnContext exercises the real
// goroutine loop (not just Tick): Run must act without waiting a full
// interval — which is what makes a restart drain its backlog promptly —
// and must return when its context is cancelled.
func TestScheduler_RunTicksImmediatelyAndStopsOnContext(t *testing.T) {
	s, clock := newTestStore(t)

	closeAt := baseTime.Add(time.Hour)
	m := openStakedMarket(t, s, closeAt, map[string]int64{"yes": 100, "no": 100})
	clock.Set(closeAt)

	// A deliberately long interval: if Run waited for the first tick, this
	// test would time out instead of passing.
	sched := NewScheduler(s, clock, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	var lastState State
	for time.Now().Before(deadline) {
		got := mustGet(t, s, m.ID)
		lastState = got.State
		if got.State == StateClosed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastState != StateClosed {
		cancel()
		<-done
		t.Fatalf("Run did not close the market on its immediate first tick (state %s)", lastState)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestScheduler_UsesInjectedClockNotWallClock guards the "no time.Now() in
// domain logic" rule from the outside: with a fake clock pinned far in the
// past, a market whose close_at is in the real past but the fake future
// must NOT be closed. If any code path reached for the wall clock, this
// fails.
func TestScheduler_UsesInjectedClockNotWallClock(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()
	sched := NewScheduler(s, clock, time.Second, quietLogger())

	// baseTime (2026-01-01) is the fake "now"; close 1h after that. Both
	// are unrelated to the real wall clock at test time.
	closeAt := baseTime.Add(time.Hour)
	m := openStakedMarket(t, s, closeAt, map[string]int64{"yes": 100, "no": 100})

	// Rewind the fake clock well before the market even existed.
	clock.Set(baseTime.Add(-365 * 24 * time.Hour))
	res, err := sched.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Closed != 0 || res.Cancelled != 0 {
		t.Fatalf("tick acted using something other than the injected clock: %+v", res)
	}
	if got := mustGet(t, s, m.ID); got.State != StateOpen {
		t.Fatalf("state = %s, want OPEN", got.State)
	}
}

// TestScheduler_ZeroIntervalFallsBackToDefault is a small constructor
// guard: a zero interval would spin a ticker panic, so it must be
// normalized.
func TestScheduler_ZeroIntervalFallsBackToDefault(t *testing.T) {
	s, clock := newTestStore(t)
	sched := NewScheduler(s, clock, 0, nil)
	if sched.interval != DefaultTickInterval {
		t.Fatalf("interval = %s, want %s", sched.interval, DefaultTickInterval)
	}
	if sched.log == nil {
		t.Fatal("nil logger must be replaced with a default")
	}
}

// Compile-time guard: FakeClock must remain a valid Clock so domain code
// can be driven deterministically.
var _ platform.Clock = (*platform.FakeClock)(nil)
