// Package box holds the domain math for SPX short box spreads.
//
// Unit convention: all prices are integer index points with 4 decimal
// places of precision (1 Price unit = 0.0001 point). SPX option
// multipliers are $100 per point, so USD = points × 100; dollars are
// derived for display only. Rates are exact rationals and are converted
// to float64 only for presentation.
package box

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// PriceScale is the number of Price units in one index point.
const PriceScale = 10_000

// Price is an index-point price stored as an integer number of
// 0.0001-point units. Never use float64 for money math.
type Price int64

// ParsePrice parses a decimal point value such as "12.35" into Price.
func ParsePrice(s string) (Price, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty price")
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > 4 {
		return 0, fmt.Errorf("price %q has more than 4 decimal places", s)
	}
	fracPart += strings.Repeat("0", 4-len(fracPart))
	ip, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("price %q: %w", s, err)
	}
	fp, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("price %q: %w", s, err)
	}
	p := Price(ip*PriceScale + fp)
	if neg {
		p = -p
	}
	return p, nil
}

// MustParsePrice is ParsePrice that panics on error; for tests and
// package-level constants.
func MustParsePrice(s string) Price {
	p, err := ParsePrice(s)
	if err != nil {
		panic(err)
	}
	return p
}

// PriceFromPoints converts a decimal point value (e.g. a config float)
// to Price, rounding to the nearest 1/10000 point. Use only at config
// boundaries — money math stays in integer Price.
func PriceFromPoints(f float64) Price { return Price(math.Round(f * PriceScale)) }

// Float64 returns the price in points for display only.
func (p Price) Float64() float64 { return float64(p) / PriceScale }

// String returns the price in points with 4 decimals.
func (p Price) String() string {
	neg := p < 0
	v := int64(p)
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d.%04d", v/PriceScale, v%PriceScale)
	if neg {
		return "-" + s
	}
	return s
}

// Mul returns p×n in Price units, truncated.
func (p Price) Mul(n int64) Price { return Price(int64(p) * n) }

// Div returns p÷n truncated toward zero.
func (p Price) Div(n int64) Price { return Price(int64(p) / n) }

// Strike is an option strike in whole index points (SPX strikes are
// integral; step is configured separately).
type Strike int64

// Price converts the strike to Price units.
func (k Strike) Price() Price { return Price(k) * PriceScale }

// Combo identifies one short box candidate: same expiry, K1 < K2.
type Combo struct {
	Expiry time.Time // expiry date (UTC midnight)
	K1     Strike
	K2     Strike
	D      int // calendar days from scan date to expiry
}

// Width is the payoff W = K2 − K1 in points.
func (c Combo) Width() Strike { return c.K2 - c.K1 }

// Quote is a bid/ask pair in Price units.
type Quote struct {
	Bid, Ask Price
}

// Mid returns the bid/ask midpoint; trades at half a tick are truncated
// toward zero (documented convention, consistent for all four legs).
func (q Quote) Mid() Price { return (q.Bid + q.Ask) / 2 }

// LegQuotes holds the four box legs:
//
//	Sell K1 Call, Buy K2 Call, Buy K1 Put, Sell K2 Put.
type LegQuotes struct {
	SellCallK1 Quote
	BuyCallK2  Quote
	BuyPutK1   Quote
	SellPutK2  Quote
}

// Credits is the premium received for opening the short box.
type Credits struct {
	// Natural: sell legs at bid, buy legs at ask — the conservative
	// fill-now borrowing basis.
	Natural Price
	// Mid: all four legs at midpoint — the theoretical center.
	Mid Price
}

// Evaluate computes the opening credit of the short box.
func Evaluate(l LegQuotes) Credits {
	return Credits{
		Natural: l.SellCallK1.Bid - l.BuyCallK2.Ask - l.BuyPutK1.Ask + l.SellPutK2.Bid,
		Mid:     l.SellCallK1.Mid() - l.BuyCallK2.Mid() - l.BuyPutK1.Mid() + l.SellPutK2.Mid(),
	}
}

// Rate is an exact rational annualized interest rate on a 365-day
// basis: the represented value is Num/Den. Keeping it as a rational
// makes ordering and the r_natural ≥ r_mid invariant exact; convert to
// float64 only for display.
type Rate struct {
	Num, Den int64
}

// AnnualRate computes the implied annualized borrowing rate of a short
// box with the given opening credit, payoff width (both in Price units)
// and calendar days to expiry: r = (W − C)/C × 365/D.
//
// The result is only meaningful for 0 < credit < width and days > 0;
// callers must validate inputs first.
func AnnualRate(credit, width Price, days int) Rate {
	if credit <= 0 || days <= 0 {
		return Rate{}
	}
	return Rate{
		Num: int64(width-credit) * 365,
		Den: int64(credit) * int64(days),
	}
}

// Float64 returns the rate as a plain fraction (0.065 == 6.5%).
func (r Rate) Float64() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Percent returns the rate in percent (6.5 means 6.5%).
func (r Rate) Percent() float64 { return r.Float64() * 100 }

// Cmp compares two rates exactly (no float rounding).
func (r Rate) Cmp(o Rate) int {
	a := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den))
	b := new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den))
	return a.Cmp(b)
}

// SpreadBPS returns (r − other) in basis points for display. other is a
// plain fraction (e.g. a benchmark rate of 0.065).
func (r Rate) SpreadBPS(other float64) float64 {
	return (r.Float64() - other) * 10_000
}
