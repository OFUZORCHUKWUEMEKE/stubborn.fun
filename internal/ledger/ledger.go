package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Ledger is the single choke point for all money movement. Every caller in
// every other package (stake, settle, user) must go through Transfer/Grant
// rather than writing to the accounts/entries collections directly.
type Ledger struct {
	client   *mongo.Client
	accounts *mongo.Collection
	entries  *mongo.Collection
}

func New(client *mongo.Client, db *mongo.Database) *Ledger {
	return &Ledger{
		client:   client,
		accounts: db.Collection(AccountsCollection),
		entries:  db.Collection(EntriesCollection),
	}
}

// isUnlimited reports whether an owner is a play-money issuance source that
// is allowed to carry a negative balance. Only promo_pool qualifies today;
// house_rake must never go negative.
func isUnlimited(p Party) bool {
	return p.OwnerType == OwnerSystem && p.OwnerID == SystemPromoPool
}

// getOrCreateAccount upserts and returns the account for (owner, currency).
// Must be called inside the Transfer transaction so the upsert participates
// in the same snapshot/retry semantics as the balance check and entry
// writes.
func (l *Ledger) getOrCreateAccount(ctx context.Context, p Party, currency string) (Account, error) {
	filter := bson.M{"owner_type": p.OwnerType, "owner_id": p.OwnerID, "currency": currency}
	now := time.Now().UTC()
	update := bson.M{
		"$setOnInsert": bson.M{
			"owner_type": p.OwnerType,
			"owner_id":   p.OwnerID,
			"currency":   currency,
			"balance":    int64(0),
			"unlimited":  isUnlimited(p),
			"created_at": now,
			"updated_at": now,
		},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var acct Account
	if err := l.accounts.FindOneAndUpdate(ctx, filter, update, opts).Decode(&acct); err != nil {
		return Account{}, fmt.Errorf("ledger: get or create account: %w", err)
	}
	return acct, nil
}

// Transfer moves Amount from one account to another as a single atomic,
// idempotent posting. It:
//   - rejects non-positive amounts and empty currency (ErrInvalidAmount)
//   - rejects from == to (ErrSameAccount)
//   - rejects overdrawing a non-Unlimited account (ErrInsufficientFunds)
//   - is a no-op (nil error, no new entries) if txnID was already applied
//
// Both legs of the posting share txnID; the unique (txn_id, account_id)
// index on entries is what makes replay detection safe under concurrent
// retries of the same logical operation (e.g. an HTTP client retrying
// after a timeout).
func (l *Ledger) Transfer(ctx context.Context, txnID string, from, to Party, amount Amount, entryType EntryType, ref TransferRef) error {
	if txnID == "" {
		return fmt.Errorf("ledger: txnID must not be empty")
	}
	if amount.Amount <= 0 || amount.Currency == "" {
		return ErrInvalidAmount
	}
	if from == to {
		return ErrSameAccount
	}

	session, err := l.client.StartSession()
	if err != nil {
		return fmt.Errorf("ledger: start session: %w", err)
	}
	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(txCtx context.Context) (any, error) {
		fromAcct, err := l.getOrCreateAccount(txCtx, from, amount.Currency)
		if err != nil {
			return nil, err
		}
		toAcct, err := l.getOrCreateAccount(txCtx, to, amount.Currency)
		if err != nil {
			return nil, err
		}

		if !fromAcct.Unlimited && fromAcct.Balance < amount.Amount {
			return nil, ErrInsufficientFunds
		}

		now := time.Now().UTC()
		debit := Entry{
			ID:        bson.NewObjectID(),
			TxnID:     txnID,
			AccountID: fromAcct.ID,
			Amount:    amount.Amount,
			Currency:  amount.Currency,
			Direction: Debit,
			Type:      entryType,
			MarketID:  ref.MarketID,
			Ref:       ref.Note,
			CreatedAt: now,
		}
		credit := Entry{
			ID:        bson.NewObjectID(),
			TxnID:     txnID,
			AccountID: toAcct.ID,
			Amount:    amount.Amount,
			Currency:  amount.Currency,
			Direction: Credit,
			Type:      entryType,
			MarketID:  ref.MarketID,
			Ref:       ref.Note,
			CreatedAt: now,
		}

		if _, err := l.entries.InsertMany(txCtx, []any{debit, credit}); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return nil, errIdempotentReplay
			}
			return nil, err
		}

		if _, err := l.accounts.UpdateByID(txCtx, fromAcct.ID, bson.M{
			"$inc": bson.M{"balance": -amount.Amount},
			"$set": bson.M{"updated_at": now},
		}); err != nil {
			return nil, err
		}
		if _, err := l.accounts.UpdateByID(txCtx, toAcct.ID, bson.M{
			"$inc": bson.M{"balance": amount.Amount},
			"$set": bson.M{"updated_at": now},
		}); err != nil {
			return nil, err
		}

		return nil, nil
	})

	if errors.Is(err, errIdempotentReplay) {
		return nil
	}
	return err
}

// Grant credits a user's starter play-money balance from promo_pool. It is
// idempotent per (userID, currency): calling it twice for the same user
// (e.g. a retried signup request) only credits once.
func (l *Ledger) Grant(ctx context.Context, userID string, amount Amount) error {
	txnID := fmt.Sprintf("starter-credit:%s:%s", userID, amount.Currency)
	return l.Transfer(ctx, txnID,
		Party{OwnerType: OwnerSystem, OwnerID: SystemPromoPool},
		Party{OwnerType: OwnerUser, OwnerID: userID},
		amount, EntryStarterCredit, TransferRef{Note: "starter balance grant"},
	)
}

// Account returns the account document for (owner, currency), or
// ErrAccountNotFound if no money has ever moved through it.
func (l *Ledger) Account(ctx context.Context, p Party, currency string) (Account, error) {
	var acct Account
	err := l.accounts.FindOne(ctx, bson.M{"owner_type": p.OwnerType, "owner_id": p.OwnerID, "currency": currency}).Decode(&acct)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, err
	}
	return acct, nil
}

// Balance returns an account's cached balance, or 0 if the account has
// never been touched.
func (l *Ledger) Balance(ctx context.Context, p Party, currency string) (int64, error) {
	acct, err := l.Account(ctx, p, currency)
	if errors.Is(err, ErrAccountNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return acct.Balance, nil
}

type entrySumRow struct {
	Sum int64 `bson:"sum"`
}

func signedSumPipeline(match bson.M) mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id": nil,
			"sum": bson.M{"$sum": bson.M{"$cond": bson.A{
				bson.M{"$eq": bson.A{"$direction", string(Credit)}},
				"$amount",
				bson.M{"$multiply": bson.A{"$amount", -1}},
			}}},
		}}},
	}
}

// SumEntriesForAccount returns the signed sum (credits positive, debits
// negative) of every entry ever posted to an account. Used to verify the
// invariant that Account.Balance always equals the sum of its entries; not
// on any request hot path.
func (l *Ledger) SumEntriesForAccount(ctx context.Context, accountID bson.ObjectID) (int64, error) {
	return l.signedSum(ctx, bson.M{"account_id": accountID})
}

// SumEntriesForTxn returns the signed sum of every entry posted under a
// txnID. For any Transfer this must be exactly zero.
func (l *Ledger) SumEntriesForTxn(ctx context.Context, txnID string) (int64, error) {
	return l.signedSum(ctx, bson.M{"txn_id": txnID})
}

func (l *Ledger) signedSum(ctx context.Context, match bson.M) (int64, error) {
	cur, err := l.entries.Aggregate(ctx, signedSumPipeline(match))
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)

	var rows []entrySumRow
	if err := cur.All(ctx, &rows); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Sum, nil
}

// EntriesForTxn returns both legs of a posting, ordered by insertion. Test
// and audit helper.
func (l *Ledger) EntriesForTxn(ctx context.Context, txnID string) ([]Entry, error) {
	cur, err := l.entries.Find(ctx, bson.M{"txn_id": txnID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []Entry
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
