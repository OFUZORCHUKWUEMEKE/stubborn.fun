package market

import "errors"

// Typed domain errors. The HTTP layer maps these to status codes; market
// never imports net/http.
var (
	// ErrMarketNotFound means no market document has that ID.
	ErrMarketNotFound = errors.New("market: not found")

	// ErrIllegalTransition means the market exists but is not in the
	// state the requested transition requires. This is also what losing a
	// compare-and-swap race looks like (two closers, one winner), which is
	// why the scheduler treats it as a benign no-op rather than a failure.
	ErrIllegalTransition = errors.New("market: illegal state transition")

	// ErrUnknownOutcome means the supplied outcome ID is not one of the
	// market's outcomes.
	ErrUnknownOutcome = errors.New("market: unknown outcome")
)

// Validation errors for Create.
var (
	ErrEmptyQuestion      = errors.New("market: question must not be empty")
	ErrEmptyCategory      = errors.New("market: category must not be empty")
	ErrTooFewOutcomes     = errors.New("market: at least two outcomes required")
	ErrDuplicateOutcomeID = errors.New("market: duplicate outcome id")
	ErrEmptyOutcomeField  = errors.New("market: outcome id and label must not be empty")
	ErrNonZeroInitialPool = errors.New("market: outcomes must start with a zero pool")
	ErrInvalidVisibility  = errors.New("market: invalid visibility")
	ErrInvalidSettlement  = errors.New("market: invalid settlement mode")
	ErrEmptyCurrency      = errors.New("market: currency must not be empty")
	ErrEmptyCreator       = errors.New("market: creator id must not be empty")
	ErrCloseAtNotInFuture = errors.New("market: close_at must be in the future")
)
