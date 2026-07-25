package stake

import "errors"

// Typed domain errors. The HTTP layer maps these to status codes; stake
// never imports net/http. Errors originating in ledger or market
// (ErrInsufficientFunds, ErrMarketClosed, ErrUnknownOutcome...) are
// wrapped and pass through, so callers match on those directly with
// errors.Is.
var (
	ErrEmptyUserID      = errors.New("stake: user id must not be empty")
	ErrStakeTooSmall    = errors.New("stake: amount below minimum")
	ErrStakeTooLarge    = errors.New("stake: amount above maximum")
	ErrNotCircleMember  = errors.New("stake: user is not a member of this circle")
	ErrPositionNotFound = errors.New("stake: position not found")
)
