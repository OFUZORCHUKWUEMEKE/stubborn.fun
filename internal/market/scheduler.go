package market

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/platform"
)

// DefaultTickInterval is how often the scheduler polls for overdue
// markets. Seconds-granularity is fine: close_at precision users care
// about is "about when I was told", and the poll is an indexed query.
const DefaultTickInterval = 5 * time.Second

// DefaultTickBatchSize caps how many overdue markets one tick will handle,
// so a large backlog (e.g. after downtime) is drained over several ticks
// instead of one unbounded pass.
const DefaultTickBatchSize = 500

// Scheduler auto-closes markets whose close_at has passed. It is
// deliberately stateless: every Tick re-derives the overdue set from the
// database and the injected Clock, so a process restart loses nothing and
// overdue markets are picked up on the next tick. Running two schedulers
// concurrently is safe but wasteful — the state precondition on each
// transition means only one can win each close.
type Scheduler struct {
	store    *Store
	clock    platform.Clock
	interval time.Duration
	batch    int64
	log      *slog.Logger
}

func NewScheduler(store *Store, clock platform.Clock, interval time.Duration, log *slog.Logger) *Scheduler {
	if interval <= 0 {
		interval = DefaultTickInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		store:    store,
		clock:    clock,
		interval: interval,
		batch:    DefaultTickBatchSize,
		log:      log,
	}
}

// TickResult reports what one pass did. Skipped counts markets another
// tick or process transitioned first (a lost compare-and-swap), which is
// expected and benign.
type TickResult struct {
	Closed    int
	Cancelled int
	Skipped   int
}

// Run polls until ctx is cancelled. It ticks once immediately rather than
// waiting a full interval, so a restart drains any backlog of overdue
// markets straight away.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		if res, err := s.Tick(ctx); err != nil {
			// Errors are logged and dropped: the next tick retries from
			// current database state, so a transient failure self-heals.
			s.log.ErrorContext(ctx, "market scheduler tick failed", "error", err,
				"closed", res.Closed, "cancelled", res.Cancelled, "skipped", res.Skipped)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick performs one poll-and-act pass. It is exported so tests can drive
// it deterministically under a fake clock without waiting on a ticker.
//
// A market past close_at with fewer than MinStakedOutcomes staked outcomes
// is CANCELLED (there is no meaningful pari-mutuel settlement to run);
// otherwise it is CLOSED. ErrIllegalTransition is swallowed as Skipped: it
// means someone else already moved this market.
func (s *Scheduler) Tick(ctx context.Context) (TickResult, error) {
	var res TickResult

	overdue, err := s.store.FindOverdueOpen(ctx, s.clock.Now(), s.batch)
	if err != nil {
		return res, err
	}

	var errs []error
	for _, m := range overdue {
		if IsUnderFilled(m.Outcomes) {
			_, err = s.store.Cancel(ctx, m.ID, ActorScheduler, "under-filled at close: fewer than two outcomes staked")
		} else {
			_, err = s.store.Close(ctx, m.ID, ActorScheduler)
		}

		switch {
		case err == nil:
			if IsUnderFilled(m.Outcomes) {
				res.Cancelled++
			} else {
				res.Closed++
			}
		case errors.Is(err, ErrIllegalTransition), errors.Is(err, ErrMarketNotFound):
			// Another tick, another instance, or an admin got there first.
			res.Skipped++
		default:
			errs = append(errs, err)
		}
	}

	return res, errors.Join(errs...)
}
