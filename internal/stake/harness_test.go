package stake

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/ledger"
	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/market"
	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/platform"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// allowMembership is the permissive MembershipChecker used where circle
// membership is not what a test is exercising.
type allowMembership struct{}

func (allowMembership) IsMember(context.Context, string, string) (bool, error) { return true, nil }

// memberSet allows exactly the listed users, for circle visibility tests.
type memberSet map[string]bool

func (m memberSet) IsMember(_ context.Context, _ string, userID string) (bool, error) {
	return m[userID], nil
}

// failingMembership simulates the circle service being unavailable.
type failingMembership struct{ err error }

func (f failingMembership) IsMember(context.Context, string, string) (bool, error) {
	return false, f.err
}

type harness struct {
	svc     *Service
	ledger  *ledger.Ledger
	markets *market.Store
	clock   *platform.FakeClock
	db      *mongo.Database
}

func newHarness(t *testing.T, members MembershipChecker) *harness {
	t.Helper()
	uri := os.Getenv("STUBBORN_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("STUBBORN_TEST_MONGO_URI not set; skipping stake integration tests (see `make mongo-rs`)")
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

	db := client.Database(fmt.Sprintf("stubborn_stake_test_%d", time.Now().UnixNano()))

	// Indexes must exist before any transaction runs: they also create the
	// collections, and creating a collection inside a transaction is a
	// restriction we simply avoid rather than rely on.
	if err := ledger.EnsureIndexes(ctx, db); err != nil {
		t.Fatalf("ledger indexes: %v", err)
	}
	if err := market.EnsureIndexes(ctx, db); err != nil {
		t.Fatalf("market indexes: %v", err)
	}
	if err := EnsureIndexes(ctx, db); err != nil {
		t.Fatalf("stake indexes: %v", err)
	}

	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dropCancel()
		_ = db.Drop(dropCtx)
		_ = client.Disconnect(dropCtx)
	})

	clock := platform.NewFakeClock(baseTime)
	led := ledger.New(client, db)
	markets := market.NewStore(db, clock)
	svc := NewService(client, db, led, markets, members, clock, DefaultConfig())

	return &harness{svc: svc, ledger: led, markets: markets, clock: clock, db: db}
}

// openMarket creates and publishes a public market that closes an hour
// after the fake clock's start.
func (h *harness) openMarket(t *testing.T) market.Market {
	t.Helper()
	return h.openMarketWithVisibility(t, market.Visibility{Kind: market.VisibilityPublic})
}

func (h *harness) openMarketWithVisibility(t *testing.T, v market.Visibility) market.Market {
	t.Helper()
	ctx := context.Background()
	m, err := h.markets.Create(ctx, market.NewMarket{
		Question: "Will Super Eagles beat Ghana?",
		Category: "Soccer",
		Outcomes: []market.Outcome{
			{ID: "yes", Label: "Yes"},
			{ID: "no", Label: "No"},
		},
		Currency:       "NGN_PLAY",
		Visibility:     v,
		SettlementMode: market.SettlementCreator,
		CreatorID:      "user-creator",
		CloseAt:        h.clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create market: %v", err)
	}
	published, err := h.markets.Publish(ctx, m.ID, "user-creator")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return published
}

// fund grants a user starter play money.
func (h *harness) fund(t *testing.T, userID string, kobo int64) {
	t.Helper()
	if err := h.ledger.Grant(context.Background(), userID, ledger.Amount{Amount: kobo, Currency: "NGN_PLAY"}); err != nil {
		t.Fatalf("fund %s: %v", userID, err)
	}
}

func (h *harness) balance(t *testing.T, userID string) int64 {
	t.Helper()
	b, err := h.ledger.Balance(context.Background(), userParty(userID), "NGN_PLAY")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func (h *harness) escrowBalance(t *testing.T, marketID bson.ObjectID) int64 {
	t.Helper()
	b, err := h.ledger.Balance(context.Background(), escrowParty(marketID), "NGN_PLAY")
	if err != nil {
		t.Fatalf("escrow balance: %v", err)
	}
	return b
}

func (h *harness) mustMarket(t *testing.T, id bson.ObjectID) market.Market {
	t.Helper()
	m, err := h.markets.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get market: %v", err)
	}
	return m
}

// assertBalanced runs the three-way invariant: positions vs cached pools
// vs ledger escrow. Every concurrency test ends with this.
func (h *harness) assertBalanced(t *testing.T, marketID bson.ObjectID) MarketDiscrepancy {
	t.Helper()
	d, err := h.svc.ReconcileMarket(context.Background(), marketID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !d.Balanced() {
		t.Fatalf("market not balanced: positions=%d cached_total=%d escrow=%d outcome_drift=%+v",
			d.PositionsTotal, d.MarketTotal, d.EscrowBalance, d.OutcomeDrift)
	}
	return d
}

// countPositions returns how many position documents exist for a market.
func (h *harness) countPositions(t *testing.T, marketID bson.ObjectID) int64 {
	t.Helper()
	n, err := h.svc.positions.CountDocuments(context.Background(), bson.M{"market_id": marketID})
	if err != nil {
		t.Fatalf("count positions: %v", err)
	}
	return n
}

// countLedgerEntries returns how many ledger entries reference a market.
func (h *harness) countLedgerEntries(t *testing.T, marketID bson.ObjectID) int64 {
	t.Helper()
	n, err := h.db.Collection(ledger.EntriesCollection).
		CountDocuments(context.Background(), bson.M{"market_id": marketID.Hex()})
	if err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return n
}
