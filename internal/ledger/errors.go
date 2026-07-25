package ledger

import "errors"

// Typed domain errors. The HTTP layer maps these to status codes; ledger
// itself never imports net/http.
var (
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
	ErrInvalidAmount     = errors.New("ledger: amount must be positive")
	ErrCurrencyMismatch  = errors.New("ledger: currency mismatch")
	ErrSameAccount       = errors.New("ledger: from and to accounts must differ")
	ErrAccountNotFound   = errors.New("ledger: account not found")

	// ErrDuplicateTxn means this txnID was already posted, detected via
	// the unique (txn_id, account_id) index on entries.
	//
	// Transfer never surfaces it: a replay there is a successful no-op.
	// TransferTx does surface it, because the caller owns the enclosing
	// transaction — which Mongo has already doomed by the time the
	// duplicate write fails — and only the caller knows what a replay
	// means for their operation.
	ErrDuplicateTxn = errors.New("ledger: duplicate transaction id")
)
