package stake

import (
	"math"
	"testing"

	"github.com/ofuzorchukwuemeke/stubborn.fun/internal/market"
)

func mkMarket(pools ...int64) market.Market {
	m := market.Market{}
	var total int64
	for i, p := range pools {
		m.Outcomes = append(m.Outcomes, market.Outcome{
			ID:   string(rune('a' + i)),
			Pool: p,
		})
		total += p
	}
	m.TotalPool = total
	return m
}

func TestPoolPrices(t *testing.T) {
	cases := []struct {
		name  string
		pools []int64
		want  []int
	}{
		{"no stakes yet", []int64{0, 0}, []int{0, 0}},
		{"even split", []int64{5000, 5000}, []int{5000, 5000}},
		{"one sided", []int64{1000, 0}, []int{10000, 0}},
		{"three way even", []int64{100, 100, 100}, []int{3333, 3333, 3333}},
		{"uneven", []int64{7500, 2500}, []int{7500, 2500}},
		{"lopsided", []int64{9999, 1}, []int{9999, 1}},
		{"single kobo each", []int64{1, 1}, []int{5000, 5000}},
		{"truncation", []int64{1, 2}, []int{3333, 6666}},
		{"seven way", []int64{1, 1, 1, 1, 1, 1, 1}, []int{1428, 1428, 1428, 1428, 1428, 1428, 1428}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PoolPrices(mkMarket(tc.pools...))
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			sum := 0
			for i, p := range got {
				if p.PriceBPS != tc.want[i] {
					t.Errorf("outcome %d price = %d bps, want %d", i, p.PriceBPS, tc.want[i])
				}
				if p.PriceBPS < 0 {
					t.Errorf("outcome %d price is negative: %d", i, p.PriceBPS)
				}
				sum += p.PriceBPS
			}
			if sum > BPSDenominator {
				t.Fatalf("prices sum to %d bps, must never exceed %d", sum, BPSDenominator)
			}
		})
	}
}

// TestPoolPrices_NeverOverflows is why the arithmetic uses big.Int:
// pool * 10000 exceeds int64 above roughly 9.2e14 kobo, which unbounded
// play money can reach. With plain int64 math these cases wrap around and
// produce negative or absurd prices.
func TestPoolPrices_NeverOverflows(t *testing.T) {
	cases := []struct {
		name  string
		pools []int64
	}{
		{"beyond int64/10000", []int64{math.MaxInt64 / 4, math.MaxInt64 / 4}},
		{"max int64 single side", []int64{math.MaxInt64, 0}},
		{"huge and tiny", []int64{math.MaxInt64 - 1, 1}},
		{"max spread over three", []int64{math.MaxInt64 / 3, math.MaxInt64 / 3, math.MaxInt64 / 3}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PoolPrices(mkMarket(tc.pools...))
			sum := 0
			for i, p := range got {
				if p.PriceBPS < 0 || p.PriceBPS > BPSDenominator {
					t.Fatalf("outcome %d price = %d bps, outside [0, %d] — overflow", i, p.PriceBPS, BPSDenominator)
				}
				sum += p.PriceBPS
			}
			if sum > BPSDenominator {
				t.Fatalf("prices sum to %d bps, must never exceed %d", sum, BPSDenominator)
			}
		})
	}
}

func TestPoolPrices_PreservesOutcomeOrderAndIDs(t *testing.T) {
	m := market.Market{
		TotalPool: 300,
		Outcomes: []market.Outcome{
			{ID: "team_a", Pool: 100},
			{ID: "draw", Pool: 100},
			{ID: "team_b", Pool: 100},
		},
	}
	got := PoolPrices(m)
	wantIDs := []string{"team_a", "draw", "team_b"}
	if len(got) != len(wantIDs) {
		t.Fatalf("len = %d, want %d", len(got), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].OutcomeID != id {
			t.Fatalf("position %d = %q, want %q", i, got[i].OutcomeID, id)
		}
	}
}

func TestPoolPrices_EmptyMarket(t *testing.T) {
	if got := PoolPrices(market.Market{}); len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}

// TestPoolPrices_NegativeTotalIsSafe guards against a corrupted document
// producing nonsense rather than a panic or a division by zero.
func TestPoolPrices_NegativeTotalIsSafe(t *testing.T) {
	m := market.Market{
		TotalPool: -100,
		Outcomes:  []market.Outcome{{ID: "a", Pool: 50}, {ID: "b", Pool: 50}},
	}
	for _, p := range PoolPrices(m) {
		if p.PriceBPS != 0 {
			t.Fatalf("price = %d, want 0 for a nonsensical total", p.PriceBPS)
		}
	}
}
