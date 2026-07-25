package ledger

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestReconcile_CleanLedgerHasNoDiscrepancies(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	mustGrant(t, l, "jack", 10_000)
	from := Party{OwnerType: OwnerUser, OwnerID: "jack"}
	to := Party{OwnerType: OwnerMarketEscrow, OwnerID: "market-recon"}
	if err := l.Transfer(ctx, "recon:1", from, to, Amount{Amount: 2_500, Currency: "NGN_PLAY"}, EntryStake, TransferRef{}); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	discrepancies, err := l.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(discrepancies) != 0 {
		t.Fatalf("discrepancies = %+v, want none after only using Transfer/Grant", discrepancies)
	}
}

// TestReconcile_DetectsCorruptedBalance proves Reconcile actually catches
// drift rather than trivially returning empty: it bypasses Transfer to
// directly corrupt an account's cached balance (simulating a bug or an
// out-of-band write) and asserts Reconcile flags exactly that account with
// the correct cached vs. computed values.
func TestReconcile_DetectsCorruptedBalance(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	mustGrant(t, l, "kate", 4_000)
	acct, err := l.Account(ctx, Party{OwnerType: OwnerUser, OwnerID: "kate"}, "NGN_PLAY")
	if err != nil {
		t.Fatalf("account: %v", err)
	}

	// Bypass Transfer entirely: this is exactly the kind of drift
	// Reconcile exists to catch.
	if _, err := l.accounts.UpdateByID(ctx, acct.ID, bson.M{"$set": bson.M{"balance": int64(999_999)}}); err != nil {
		t.Fatalf("corrupt balance: %v", err)
	}

	discrepancies, err := l.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(discrepancies) != 1 {
		t.Fatalf("discrepancies = %+v, want exactly 1", discrepancies)
	}
	d := discrepancies[0]
	if d.OwnerID != "kate" || d.CachedBalance != 999_999 || d.ComputedBalance != 4_000 {
		t.Fatalf("discrepancy = %+v, want owner_id=kate cached=999999 computed=4000", d)
	}
}
