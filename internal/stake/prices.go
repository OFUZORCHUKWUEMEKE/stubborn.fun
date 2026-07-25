package stake

import (
	"math/big"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/market"
)

// BPSDenominator is 100% expressed in basis points.
const BPSDenominator = 10000

// PoolPrices returns each outcome's implied price in integer basis points:
// outcome_pool / total_pool. Prices are derived, never stored, so they
// cannot drift from the pools.
//
// Division truncates, so the returned prices sum to at most
// BPSDenominator (never over) — the lost remainder is display dust and is
// deliberately not redistributed, since inventing a rule for who gets the
// extra basis point would make prices disagree with the pools.
//
// A market with no stakes yet returns zero for every outcome rather than
// an even split: with an empty pool there is no implied probability, and
// showing "50/50" would be asserting information the market does not have.
//
// The arithmetic uses big.Int because pool * 10000 overflows int64 above
// roughly ₦92 billion in kobo, which unbounded play money can reach. This
// is a read/broadcast path, not a hot loop, so the allocation is fine.
func PoolPrices(m market.Market) []OutcomePrice {
	prices := make([]OutcomePrice, 0, len(m.Outcomes))

	if m.TotalPool <= 0 {
		for _, o := range m.Outcomes {
			prices = append(prices, OutcomePrice{OutcomeID: o.ID, PriceBPS: 0})
		}
		return prices
	}

	total := big.NewInt(m.TotalPool)
	scale := big.NewInt(BPSDenominator)
	for _, o := range m.Outcomes {
		bps := 0
		if o.Pool > 0 {
			v := new(big.Int).Mul(big.NewInt(o.Pool), scale)
			v.Quo(v, total)
			bps = int(v.Int64()) // bounded by BPSDenominator, so this is safe
		}
		prices = append(prices, OutcomePrice{OutcomeID: o.ID, PriceBPS: bps})
	}
	return prices
}
