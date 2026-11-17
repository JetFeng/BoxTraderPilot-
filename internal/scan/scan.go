// Package scan enumerates short box combos, filters them by quote
// quality, evaluates implied financing rates, and sorts the survivors.
package scan

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/broker"
)

// StaleThreshold is how old market data may be before the CLI prints a
// prominent staleness warning (§4.5 of the requirements).
const StaleThreshold = 15 * time.Minute

// Params describes one scan run.
type Params struct {
	Symbol          string
	Expiries        []time.Time // deduped, validated dates
	Widths          []int64     // K2 − K1, points, deduped
	K1RangePct      float64     // e.g. 0.10
	StrikeStep      int64
	MinDaysToExpiry int
	RequireQuotes   bool
	Filters         Filters
}

// Filters are quote-quality gates (§3.2-3).
type Filters struct {
	MaxLegSpreadPct float64   // per-leg (ask−bid)/mid
	MinSellBid      box.Price // sell legs' bids must clear this
	MinDays         int       // D below this is rejected
}

// RejectReason identifies why a combo was dropped. The empty string
// means the combo survived.
type RejectReason string

// Reject reasons, in the order they are checked.
const (
	OK            RejectReason = ""
	ShortDTE      RejectReason = "dte_below_min"
	MissingQuote  RejectReason = "missing_quote"
	BadQuote      RejectReason = "bad_quote" // bid ≤ 0 or bid ≥ ask
	LowSellBid    RejectReason = "sell_bid_below_min"
	WideLegSpread RejectReason = "leg_spread_too_wide"
	BadCredit     RejectReason = "credit_out_of_range"
)

// ReasonOrder fixes a stable display order for reject counts.
var ReasonOrder = []RejectReason{
	ShortDTE, MissingQuote, BadQuote, LowSellBid, WideLegSpread, BadCredit,
}

// Result is one surviving combo with its evaluation.
type Result struct {
	Combo    box.Combo
	Legs     box.LegQuotes
	Credits  box.Credits
	RNatural box.Rate
	RMid     box.Rate
}

// SpreadBPS is the natural-rate spread over a benchmark (plain
// fraction), in basis points.
func (r Result) SpreadBPS(benchmark float64) float64 {
	return r.RNatural.SpreadBPS(benchmark)
}

// Stats summarizes a run.
type Stats struct {
	Total     int
	Valid     int
	Skipped   int // dropped silently (require_quotes=false)
	Rejected  map[RejectReason]int
	DataTimes []time.Time // quote/chain snapshot times
}

// SortByRate orders results by r_natural ascending (exact comparison).
func SortByRate(rows []Result) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].RNatural.Cmp(rows[j].RNatural) < 0
	})
}

// DaysToExpiry counts calendar days between the dates of now and expiry
// (UTC midnights); leap years follow from time.Time arithmetic.
func DaysToExpiry(now, expiry time.Time) int {
	d1 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	d2 := time.Date(expiry.Year(), expiry.Month(), expiry.Day(), 0, 0, 0, 0, time.UTC)
	return int(d2.Sub(d1).Hours() / 24)
}

// Enumerate builds the deduped (expiry, K1, K2) combo list. K1 ranges
// over strike_step multiples within underlying ± k1_range_pct and
// K2 = K1 + width. D is computed against now.
func Enumerate(underlying box.Price, p Params, now time.Time) []box.Combo {
	// Range bounds: rangePct is a fraction (0.10 = ±10%). Integer math in
	// per-mille keeps strike selection free of float money arithmetic.
	perMille := int64(math.Round(p.K1RangePct * 1000))
	lo := underlying.Mul(1000 - perMille).Div(1000)
	hi := underlying.Mul(1000 + perMille).Div(1000)
	loP := (int64(lo) + box.PriceScale/2) / box.PriceScale // nearest point
	hiP := (int64(hi) + box.PriceScale/2) / box.PriceScale

	first := ((loP + p.StrikeStep - 1) / p.StrikeStep) * p.StrikeStep // ceil
	last := (hiP / p.StrikeStep) * p.StrikeStep                       // floor

	widths := dedupeWidths(p.Widths)
	exps := dedupeExpiries(p.Expiries)

	seen := make(map[string]struct{})
	var out []box.Combo
	for _, exp := range exps {
		D := DaysToExpiry(now, exp)
		for k1 := first; k1 <= last; k1 += p.StrikeStep {
			for _, w := range widths {
				k2 := k1 + w
				key := fmt.Sprintf("%s|%d|%d", exp.Format("2006-01-02"), k1, k2)
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, box.Combo{Expiry: exp, K1: box.Strike(k1), K2: box.Strike(k2), D: D})
			}
		}
	}
	return out
}

func dedupeWidths(in []int64) []int64 {
	seen := make(map[int64]struct{}, len(in))
	out := in[:0]
	for _, w := range in {
		if _, ok := seen[w]; ok {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}

func dedupeExpiries(in []time.Time) []time.Time {
	seen := make(map[time.Time]struct{}, len(in))
	out := in[:0]
	for _, e := range in {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// Check applies the filter rules in their canonical order (§3.2-3) and
// returns the first reason a combo fails, or OK.
func Check(c box.Combo, legs box.LegQuotes, f Filters) RejectReason {
	if c.D < f.MinDays {
		return ShortDTE
	}
	qs := [4]*box.Quote{
		&legs.SellCallK1, &legs.BuyCallK2, &legs.BuyPutK1, &legs.SellPutK2,
	}
	sell := [2]*box.Quote{&legs.SellCallK1, &legs.SellPutK2}
	for _, q := range qs {
		if q.Bid <= 0 || q.Bid >= q.Ask {
			return BadQuote
		}
	}
	for _, q := range sell {
		if q.Bid < f.MinSellBid {
			return LowSellBid
		}
	}
	for _, q := range qs {
		mid := q.Mid()
		if mid <= 0 {
			return BadQuote
		}
		if float64(q.Ask-q.Bid)/float64(mid) > f.MaxLegSpreadPct {
			return WideLegSpread
		}
	}
	cr := box.Evaluate(legs).Natural
	w := c.Width().Price()
	if cr <= 0 || cr >= w {
		return BadCredit
	}
	return OK
}

// legsFor pulls the four legs out of a chain; ok is false if any leg is
// absent.
func legsFor(ch broker.OptionChain, c box.Combo) (box.LegQuotes, bool) {
	s1, ok1 := ch.At(int64(c.K1))
	s2, ok2 := ch.At(int64(c.K2))
	if !ok1 || !ok2 || s1.Call == nil || s2.Call == nil || s1.Put == nil || s2.Put == nil {
		return box.LegQuotes{}, false
	}
	return box.LegQuotes{
		SellCallK1: box.Quote{Bid: s1.Call.Bid, Ask: s1.Call.Ask},
		BuyCallK2:  box.Quote{Bid: s2.Call.Bid, Ask: s2.Call.Ask},
		BuyPutK1:   box.Quote{Bid: s1.Put.Bid, Ask: s1.Put.Ask},
		SellPutK2:  box.Quote{Bid: s2.Put.Bid, Ask: s2.Put.Ask},
	}, true
}

// Execute runs the full scan: fetch (cached per expiry) → enumerate →
// filter → evaluate → sort ascending by r_natural.
func Execute(ctx context.Context, b broker.Broker, p Params, now time.Time) ([]Result, Stats, error) {
	stats := Stats{Rejected: make(map[RejectReason]int)}

	uq, err := b.GetQuote(ctx, p.Symbol)
	if err != nil {
		return nil, stats, fmt.Errorf("underlying quote %s: %w", p.Symbol, err)
	}
	stats.DataTimes = append(stats.DataTimes, uq.Timestamp)

	chains := make(map[string]broker.OptionChain)
	for _, exp := range dedupeExpiries(p.Expiries) {
		ch, err := b.GetOptionChain(ctx, p.Symbol, exp)
		if err != nil {
			return nil, stats, fmt.Errorf("option chain %s %s: %w", p.Symbol, exp.Format("2006-01-02"), err)
		}
		chains[exp.Format("2006-01-02")] = ch
		stats.DataTimes = append(stats.DataTimes, ch.Timestamp)
	}

	combos := Enumerate(uq.Price, p, now)
	stats.Total = len(combos)

	p.Filters.MinDays = p.MinDaysToExpiry
	rows := make([]Result, 0, len(combos))
	for _, c := range combos {
		ch := chains[c.Expiry.Format("2006-01-02")]
		legs, ok := legsFor(ch, c)
		if !ok {
			if p.RequireQuotes {
				stats.Rejected[MissingQuote]++
			} else {
				stats.Skipped++
			}
			continue
		}
		if reason := Check(c, legs, p.Filters); reason != OK {
			stats.Rejected[reason]++
			continue
		}
		cr := box.Evaluate(legs)
		w := c.Width().Price()
		rows = append(rows, Result{
			Combo:    c,
			Legs:     legs,
			Credits:  cr,
			RNatural: box.AnnualRate(cr.Natural, w, c.D),
			RMid:     box.AnnualRate(cr.Mid, w, c.D),
		})
	}
	stats.Valid = len(rows)
	SortByRate(rows)
	return rows, stats, nil
}

// MaxDataAge returns the age of the oldest snapshot in times relative
// to now; the staleness warning uses this.
func MaxDataAge(times []time.Time, now time.Time) time.Duration {
	var oldest time.Time
	for _, t := range times {
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return now.Sub(oldest)
}
