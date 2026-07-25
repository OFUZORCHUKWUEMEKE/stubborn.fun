package ledger

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// These tests never touch the network: mongo.Connect only starts background
// topology monitoring, it does not dial synchronously, and Transfer's
// input validation runs before the first Mongo operation. That lets us
// exercise the validation branches (and isUnlimited) in every environment,
// including ones without a reachable Mongo replica set.
func unconnectedLedger(t *testing.T) *Ledger {
	t.Helper()
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return New(client, client.Database("unused"))
}

func TestTransfer_ValidatesInputsWithoutTouchingNetwork(t *testing.T) {
	l := unconnectedLedger(t)
	ctx := context.Background()

	alice := Party{OwnerType: OwnerUser, OwnerID: "alice"}
	escrow := Party{OwnerType: OwnerMarketEscrow, OwnerID: "m1"}

	cases := []struct {
		name   string
		txnID  string
		from   Party
		to     Party
		amount Amount
		want   error
	}{
		{"empty txnID", "", alice, escrow, Amount{Amount: 100, Currency: "NGN_PLAY"}, nil},
		{"zero amount", "t1", alice, escrow, Amount{Amount: 0, Currency: "NGN_PLAY"}, ErrInvalidAmount},
		{"negative amount", "t2", alice, escrow, Amount{Amount: -1, Currency: "NGN_PLAY"}, ErrInvalidAmount},
		{"empty currency", "t3", alice, escrow, Amount{Amount: 100, Currency: ""}, ErrInvalidAmount},
		{"same account", "t4", alice, alice, Amount{Amount: 100, Currency: "NGN_PLAY"}, ErrSameAccount},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := l.Transfer(ctx, tc.txnID, tc.from, tc.to, tc.amount, EntryStake, TransferRef{})
			if tc.want == nil {
				if err == nil {
					t.Fatalf("err = nil, want a non-nil error for empty txnID")
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestIsUnlimited(t *testing.T) {
	cases := []struct {
		name string
		p    Party
		want bool
	}{
		{"promo pool is unlimited", Party{OwnerType: OwnerSystem, OwnerID: SystemPromoPool}, true},
		{"house rake is not unlimited", Party{OwnerType: OwnerSystem, OwnerID: SystemHouseRake}, false},
		{"user is not unlimited", Party{OwnerType: OwnerUser, OwnerID: SystemPromoPool}, false}, // owner_type must also match
		{"market escrow is not unlimited", Party{OwnerType: OwnerMarketEscrow, OwnerID: "m1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnlimited(tc.p); got != tc.want {
				t.Fatalf("isUnlimited(%+v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}
