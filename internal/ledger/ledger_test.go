package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Gate tests need a real Mongo replica set (multi-document transactions do
// not work against a standalone mongod). Point STUBBORN_TEST_MONGO_URI at
// one (see `make mongo-rs`); tests skip cleanly if it's unset so `go test
// ./...` still passes in environments without Mongo available.
func testDB(t *testing.T) (*mongo.Client, *mongo.Database) {
	t.Helper()
	uri := os.Getenv("STUBBORN_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("STUBBORN_TEST_MONGO_URI not set; skipping ledger integration tests (need a local Mongo replica set, see `make mongo-rs`)")
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

	dbName := fmt.Sprintf("stubborn_test_%d", time.Now().UnixNano())
	db := client.Database(dbName)

	if err := EnsureIndexes(ctx, db); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}

	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(dropCtx)
		_ = client.Disconnect(dropCtx)
	})

	return client, db
}

func newTestLedger(t *testing.T) *Ledger {
	client, db := testDB(t)
	return New(client, db)
}

func mustGrant(t *testing.T, l *Ledger, userID string, kobo int64) {
	t.Helper()
	if err := l.Grant(context.Background(), userID, Amount{Amount: kobo, Currency: "NGN_PLAY"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
}

func TestTransfer_MovesBalanceAndWritesBalancedEntries(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	mustGrant(t, l, "alice", 10_000)

	from := Party{OwnerType: OwnerUser, OwnerID: "alice"}
	to := Party{OwnerType: OwnerMarketEscrow, OwnerID: "market-1"}
	amount := Amount{Amount: 3_000, Currency: "NGN_PLAY"}

	if err := l.Transfer(ctx, "stake:1", from, to, amount, EntryStake, TransferRef{MarketID: "market-1"}); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	aliceBal, err := l.Balance(ctx, from, "NGN_PLAY")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if aliceBal != 7_000 {
		t.Fatalf("alice balance = %d, want 7000", aliceBal)
	}

	escrowBal, err := l.Balance(ctx, to, "NGN_PLAY")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if escrowBal != 3_000 {
		t.Fatalf("escrow balance = %d, want 3000", escrowBal)
	}

	sum, err := l.SumEntriesForTxn(ctx, "stake:1")
	if err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	if sum != 0 {
		t.Fatalf("entries for txn sum to %d, want 0", sum)
	}
}

func TestTransfer_RejectsOverdraft(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	mustGrant(t, l, "bob", 1_000)

	from := Party{OwnerType: OwnerUser, OwnerID: "bob"}
	to := Party{OwnerType: OwnerMarketEscrow, OwnerID: "market-2"}

	err := l.Transfer(ctx, "stake:overdraft", from, to, Amount{Amount: 1_001, Currency: "NGN_PLAY"}, EntryStake, TransferRef{})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}

	bal, err := l.Balance(ctx, from, "NGN_PLAY")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != 1_000 {
		t.Fatalf("balance = %d, want unchanged 1000", bal)
	}
}

func TestTransfer_RejectsInvalidInputs(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	from := Party{OwnerType: OwnerUser, OwnerID: "carol"}
	to := Party{OwnerType: OwnerMarketEscrow, OwnerID: "market-3"}

	cases := []struct {
		name   string
		txnID  string
		from   Party
		to     Party
		amount Amount
		want   error
	}{
		{"zero amount", "t1", from, to, Amount{Amount: 0, Currency: "NGN_PLAY"}, ErrInvalidAmount},
		{"negative amount", "t2", from, to, Amount{Amount: -1, Currency: "NGN_PLAY"}, ErrInvalidAmount},
		{"empty currency", "t3", from, to, Amount{Amount: 100, Currency: ""}, ErrInvalidAmount},
		{"same account", "t4", from, from, Amount{Amount: 100, Currency: "NGN_PLAY"}, ErrSameAccount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := l.Transfer(ctx, tc.txnID, tc.from, tc.to, tc.amount, EntryStake, TransferRef{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTransfer_ReplayIsNoOp(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	mustGrant(t, l, "dave", 5_000)

	from := Party{OwnerType: OwnerUser, OwnerID: "dave"}
	to := Party{OwnerType: OwnerMarketEscrow, OwnerID: "market-4"}
	amount := Amount{Amount: 2_000, Currency: "NGN_PLAY"}

	for i := 0; i < 3; i++ {
		if err := l.Transfer(ctx, "stake:replay", from, to, amount, EntryStake, TransferRef{}); err != nil {
			t.Fatalf("transfer attempt %d: %v", i, err)
		}
	}

	bal, err := l.Balance(ctx, from, "NGN_PLAY")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != 3_000 {
		t.Fatalf("balance = %d, want 3000 (only first transfer should apply)", bal)
	}

	entries, err := l.EntriesForTxn(ctx, "stake:replay")
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want exactly 2 (debit+credit), replay must not duplicate", len(entries))
	}
}

func TestTransfer_GrantIsIdempotentPerUser(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	amount := Amount{Amount: 5_000, Currency: "NGN_PLAY"}
	for i := 0; i < 5; i++ {
		if err := l.Grant(ctx, "erin", amount); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}

	bal, err := l.Balance(ctx, Party{OwnerType: OwnerUser, OwnerID: "erin"}, "NGN_PLAY")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != 5_000 {
		t.Fatalf("balance = %d, want 5000 (grant must be idempotent)", bal)
	}
}

// TestTransfer_ConcurrentCannotOverdraw is the money-safety gate test: 50
// goroutines race to debit the same account. Demand (50 * 100 = 5000)
// exceeds the funded balance (1000), so exactly 10 must succeed and 40
// must fail with ErrInsufficientFunds — and the account must never go
// negative regardless of goroutine interleaving.
func TestTransfer_ConcurrentCannotOverdraw(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	mustGrant(t, l, "frank", 1_000)
	from := Party{OwnerType: OwnerUser, OwnerID: "frank"}

	const goroutines = 50
	const perTransfer = 100

	var wg sync.WaitGroup
	var succeeded, failed int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			to := Party{OwnerType: OwnerMarketEscrow, OwnerID: fmt.Sprintf("market-race-%d", i)}
			txnID := fmt.Sprintf("stake:race:%d", i)
			err := l.Transfer(ctx, txnID, from, to, Amount{Amount: perTransfer, Currency: "NGN_PLAY"}, EntryStake, TransferRef{})
			switch {
			case err == nil:
				atomic.AddInt64(&succeeded, 1)
			case errors.Is(err, ErrInsufficientFunds):
				atomic.AddInt64(&failed, 1)
			default:
				t.Errorf("goroutine %d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if succeeded != 10 {
		t.Fatalf("succeeded = %d, want 10", succeeded)
	}
	if failed != 40 {
		t.Fatalf("failed = %d, want 40", failed)
	}

	bal, err := l.Balance(ctx, from, "NGN_PLAY")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance = %d, want exactly 0 (no overdraw, no leftover)", bal)
	}
	if bal < 0 {
		t.Fatalf("balance went negative: %d", bal)
	}

	acct, err := l.Account(ctx, from, "NGN_PLAY")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	sumFromEntries, err := l.SumEntriesForAccount(ctx, acct.ID)
	if err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	if sumFromEntries != acct.Balance {
		t.Fatalf("balance cache invariant violated: cached balance %d != sum of entries %d", acct.Balance, sumFromEntries)
	}
}

// TestTransfer_ConcurrentMultiAccountCannotOverdraw is the escalated version
// of the single-account race test: 500 goroutines hammer 4 distinct funded
// accounts concurrently (spreading contention instead of concentrating it
// on one document), each racing to overdraw its own account. Every account
// is funded for exactly 63 successful transfers out of 125 attempts, so
// the outcome is fully deterministic regardless of goroutine interleaving.
func TestTransfer_ConcurrentMultiAccountCannotOverdraw(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	const numAccounts = 4
	const goroutinesPerAccount = 125
	const perTransfer = 100
	const fundedBalance = 6_300                                             // = 63 * perTransfer
	const wantSuccessPerAccount = fundedBalance / perTransfer               // 63
	const wantFailPerAccount = goroutinesPerAccount - wantSuccessPerAccount // 62

	users := make([]string, numAccounts)
	for a := 0; a < numAccounts; a++ {
		users[a] = fmt.Sprintf("racer-%d", a)
		mustGrant(t, l, users[a], fundedBalance)
	}

	var wg sync.WaitGroup
	succeeded := make([]int64, numAccounts)
	failed := make([]int64, numAccounts)

	for a := 0; a < numAccounts; a++ {
		from := Party{OwnerType: OwnerUser, OwnerID: users[a]}
		for g := 0; g < goroutinesPerAccount; g++ {
			wg.Add(1)
			go func(acctIdx, g int) {
				defer wg.Done()
				to := Party{OwnerType: OwnerMarketEscrow, OwnerID: fmt.Sprintf("market-race-%d-%d", acctIdx, g)}
				txnID := fmt.Sprintf("stake:multirace:%d:%d", acctIdx, g)
				err := l.Transfer(ctx, txnID, from, to, Amount{Amount: perTransfer, Currency: "NGN_PLAY"}, EntryStake, TransferRef{})
				switch {
				case err == nil:
					atomic.AddInt64(&succeeded[acctIdx], 1)
				case errors.Is(err, ErrInsufficientFunds):
					atomic.AddInt64(&failed[acctIdx], 1)
				default:
					t.Errorf("account %d goroutine %d: unexpected error: %v", acctIdx, g, err)
				}
			}(a, g)
		}
	}
	wg.Wait()

	for a := 0; a < numAccounts; a++ {
		if succeeded[a] != wantSuccessPerAccount {
			t.Errorf("account %d: succeeded = %d, want %d", a, succeeded[a], wantSuccessPerAccount)
		}
		if failed[a] != wantFailPerAccount {
			t.Errorf("account %d: failed = %d, want %d", a, failed[a], wantFailPerAccount)
		}

		from := Party{OwnerType: OwnerUser, OwnerID: users[a]}
		acct, err := l.Account(ctx, from, "NGN_PLAY")
		if err != nil {
			t.Fatalf("account %d: %v", a, err)
		}
		if acct.Balance != 0 {
			t.Errorf("account %d: balance = %d, want exactly 0 (no overdraw, no leftover)", a, acct.Balance)
		}
		if acct.Balance < 0 {
			t.Errorf("account %d: balance went negative: %d", a, acct.Balance)
		}

		sum, err := l.SumEntriesForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("account %d sum entries: %v", a, err)
		}
		if sum != acct.Balance {
			t.Errorf("account %d: balance cache invariant violated: cached %d != sum of entries %d", a, acct.Balance, sum)
		}
	}
}

// TestBalanceCache_AlwaysEqualsSumOfEntries is a property check run after a
// mixed sequence of grants, transfers, and rejected overdrafts: every
// touched account's cached balance must equal the signed sum of its
// entries.
func TestBalanceCache_AlwaysEqualsSumOfEntries(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	users := []string{"grace", "heidi", "ivan"}
	for _, u := range users {
		mustGrant(t, l, u, 10_000)
	}

	ops := []struct {
		from, to Party
		amount   int64
		txnID    string
	}{
		{Party{OwnerType: OwnerUser, OwnerID: "grace"}, Party{OwnerType: OwnerMarketEscrow, OwnerID: "m1"}, 4_000, "op1"},
		{Party{OwnerType: OwnerUser, OwnerID: "heidi"}, Party{OwnerType: OwnerMarketEscrow, OwnerID: "m1"}, 6_000, "op2"},
		{Party{OwnerType: OwnerUser, OwnerID: "ivan"}, Party{OwnerType: OwnerMarketEscrow, OwnerID: "m1"}, 999_999, "op3"}, // will fail: overdraft
		{Party{OwnerType: OwnerMarketEscrow, OwnerID: "m1"}, Party{OwnerType: OwnerUser, OwnerID: "grace"}, 10_000, "op4"}, // payout
		{Party{OwnerType: OwnerMarketEscrow, OwnerID: "m1"}, Party{OwnerType: OwnerSystem, OwnerID: SystemHouseRake}, 1, "op5"},
	}

	for _, op := range ops {
		_ = l.Transfer(ctx, op.txnID, op.from, op.to, Amount{Amount: op.amount, Currency: "NGN_PLAY"}, EntryStake, TransferRef{})
	}

	checkParties := []Party{
		{OwnerType: OwnerMarketEscrow, OwnerID: "m1"},
		{OwnerType: OwnerSystem, OwnerID: SystemHouseRake},
		{OwnerType: OwnerSystem, OwnerID: SystemPromoPool},
	}
	for _, u := range users {
		checkParties = append(checkParties, Party{OwnerType: OwnerUser, OwnerID: u})
	}

	for _, p := range checkParties {
		acct, err := l.Account(ctx, p, "NGN_PLAY")
		if errors.Is(err, ErrAccountNotFound) {
			continue
		}
		if err != nil {
			t.Fatalf("account %+v: %v", p, err)
		}
		sum, err := l.SumEntriesForAccount(ctx, acct.ID)
		if err != nil {
			t.Fatalf("sum entries %+v: %v", p, err)
		}
		if sum != acct.Balance {
			t.Fatalf("account %+v: cached balance %d != sum of entries %d", p, acct.Balance, sum)
		}
	}
}
