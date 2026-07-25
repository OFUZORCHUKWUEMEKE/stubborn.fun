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
)

// errIdempotentReplay is an internal sentinel used to unwind a Mongo
// transaction when a txnID replay is detected via the unique (txn_id,
// account_id) index. It never escapes Transfer/Grant as an error — a
// replay is reported to the caller as a nil-error no-op.
var errIdempotentReplay = errors.New("ledger: idempotent replay")
