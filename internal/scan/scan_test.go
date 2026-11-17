package scan

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/broker"
)

func dt(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func testParams() Params {
	return Params{
		Symbol:          "SPX",
		Expiries:        []time.Time{dt(2026, 12, 18)},
		Widths:          []int64{250, 500},
		K1RangePct:      0.10,
		StrikeStep:      5,
		MinDaysToExpiry: 21,
		RequireQuotes:   true,
		Filters: Filters{
			MaxLegSpreadPct: 0.15,
			MinSellBid:      box.MustParsePrice("0.05"),
			MinDays:         21,
		},
	}
}

func TestEnumerateUniqueInRange(t *testing.T) {
	p := testParams()
	now := dt(2026, 11, 8)
	underlying := box.MustParsePrice("5912.30") // ±10% → [5321.07, 6503.53]
	combos := Enumerate(underlying, p, now)

	if len(combos) == 0 {
		t.Fatal("no combos")
	}
	// 5325..6500 step 5 → 236 K1s × 2 widths.
	if want := 236 * 2; len(combos) != want {
		t.Errorf("len = %d, want %d", len(combos), want)
	}
	seen := make(map[box.Combo]int)
	for _, c := range combos {
		seen[c]++
		if c.K2-c.K1 != 250 && c.K2-c.K1 != 500 {
			t.Errorf("bad width: %+v", c)
		}
		if int64(c.K1)%5 != 0 {
			t.Errorf("K1 not on step: %+v", c)
		}
		if c.K1 < 5321 || c.K1 > 6504 {
			t.Errorf("K1 out of range: %+v", c)
		}
		if c.D != 40 {
			t.Errorf("D = %d, want 40", c.D)
		}
	}
	for c, n := range seen {
		if n > 1 {
			t.Errorf("duplicate combo %+v", c)
		}
	}
}

func TestEnumerateDedupesConfig(t *testing.T) {
	p := testParams()
	p.Widths = []int64{250, 250, 500}
	p.Expiries = []time.Time{dt(2026, 12, 18), dt(2026, 12, 18)}
	combos := Enumerate(box.MustParsePrice("5912.30"), p, dt(2026, 11, 8))
	if want := 236 * 2; len(combos) != want {
		t.Errorf("len = %d, want %d (duplicates not removed)", len(combos), want)
	}
}

func TestDaysToExpiryLeapYear(t *testing.T) {
	cases := []struct {
		now, exp time.Time
		want     int
	}{
		{dt(2023, 3, 1), dt(2024, 3, 1), 366},   // span includes 2024-02-29
		{dt(2023, 2, 28), dt(2024, 2, 28), 365}, // span ends before 2024-02-29
		{dt(2024, 2, 28), dt(2024, 2, 29), 1},
		{dt(2024, 1, 1), dt(2024, 12, 31), 365},
		{dt(2026, 11, 8), dt(2026, 12, 18), 40},
	}
	for _, c := range cases {
		if got := DaysToExpiry(c.now, c.exp); got != c.want {
			t.Errorf("DaysToExpiry(%v, %v) = %d, want %d", c.now, c.exp, got, c.want)
		}
	}
}

var mp = box.MustParsePrice

// goodLegs builds legs that pass every filter for a 250-wide box
// (all per-leg spreads well under 15%).
func goodLegs() box.LegQuotes {
	return box.LegQuotes{
		SellCallK1: box.Quote{Bid: mp("98"), Ask: mp("98.10")},
		BuyCallK2:  box.Quote{Bid: mp("3"), Ask: mp("3.10")},
		BuyPutK1:   box.Quote{Bid: mp("20"), Ask: mp("20.20")},
		SellPutK2:  box.Quote{Bid: mp("24"), Ask: mp("24.20")},
	}
}

func goodCombo() box.Combo {
	return box.Combo{Expiry: dt(2026, 12, 18), K1: 5900, K2: 6150, D: 40}
}

func TestFilterOK(t *testing.T) {
	if r := Check(goodCombo(), goodLegs(), testParams().Filters); r != OK {
		t.Errorf("Check = %q, want OK", r)
	}
}

func TestFiltersReject(t *testing.T) {
	cases := []struct {
		name   string
		modC   func(*box.Combo)
		modL   func(*box.LegQuotes)
		modF   func(*Filters)
		reason RejectReason
	}{
		{
			"short DTE",
			func(c *box.Combo) { c.D = 10 }, nil, nil, ShortDTE,
		},
		{
			"zero bid",
			nil, func(l *box.LegQuotes) { l.BuyCallK2.Bid = 0 }, nil, BadQuote,
		},
		{
			"bid equals ask",
			nil, func(l *box.LegQuotes) { l.BuyPutK1.Ask = l.BuyPutK1.Bid }, nil, BadQuote,
		},
		{
			"crossed quote",
			nil, func(l *box.LegQuotes) { l.SellCallK1.Bid, l.SellCallK1.Ask = l.SellCallK1.Ask, l.SellCallK1.Bid }, nil, BadQuote,
		},
		{
			"sell bid below min",
			nil, func(l *box.LegQuotes) { l.SellCallK1.Bid = mp("0.03"); l.SellCallK1.Ask = mp("0.034") }, nil, LowSellBid,
		},
		{
			"wide leg spread",
			nil, func(l *box.LegQuotes) { l.BuyCallK2 = box.Quote{Bid: mp("3"), Ask: mp("7")} }, nil, WideLegSpread,
		},
		{
			"spread exactly at limit passes",
			nil, func(l *box.LegQuotes) { l.BuyCallK2 = box.Quote{Bid: mp("37"), Ask: mp("43")} }, nil, OK, // 6/40 = 15%
		},
		{
			"negative credit",
			nil, func(l *box.LegQuotes) {
				l.SellCallK1 = box.Quote{Bid: mp("1"), Ask: mp("1.05")}
				l.SellPutK2 = box.Quote{Bid: mp("1"), Ask: mp("1.05")}
				l.BuyCallK2 = box.Quote{Bid: mp("10"), Ask: mp("10.05")}
				l.BuyPutK1 = box.Quote{Bid: mp("10"), Ask: mp("10.05")}
			}, nil, BadCredit,
		},
		{
			"credit above width",
			func(c *box.Combo) { c.K2 = 5910 }, // width 10
			func(l *box.LegQuotes) {
				l.BuyCallK2 = box.Quote{Bid: mp("1"), Ask: mp("1.05")}
				l.BuyPutK1 = box.Quote{Bid: mp("1"), Ask: mp("1.05")}
			}, nil, BadCredit,
		},
		{
			"lower min bid passes",
			nil, func(l *box.LegQuotes) { l.SellCallK1.Bid = mp("0.03"); l.SellCallK1.Ask = mp("0.034") },
			func(f *Filters) { f.MinSellBid = mp("0.01") }, OK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := goodCombo()
			l := goodLegs()
			f := testParams().Filters
			if tc.modC != nil {
				tc.modC(&c)
			}
			if tc.modL != nil {
				tc.modL(&l)
			}
			if tc.modF != nil {
				tc.modF(&f)
			}
			if got := Check(c, l, f); got != tc.reason {
				t.Errorf("Check = %q, want %q", got, tc.reason)
			}
		})
	}
}

func TestSortByRateAscending(t *testing.T) {
	w := box.MustParsePrice("250") * box.PriceScale
	rng := rand.New(rand.NewSource(7))
	var rows []Result
	for i := 0; i < 50; i++ {
		credit := box.Price(100_000 + rng.Int63n(140_000)) // 10.00 .. 24.00
		r := Result{
			Combo:    box.Combo{D: 40 + i%3},
			Credits:  box.Credits{Natural: credit, Mid: credit + 200},
			RNatural: box.AnnualRate(credit, w, 40+i%3),
			RMid:     box.AnnualRate(credit+200, w, 40+i%3),
		}
		rows = append(rows, r)
	}
	SortByRate(rows)
	for i := 1; i < len(rows); i++ {
		if rows[i-1].RNatural.Cmp(rows[i].RNatural) >= 0 {
			t.Fatalf("row %d (%v) not strictly before row %d (%v)", i-1, rows[i-1].RNatural, i, rows[i].RNatural)
		}
	}
}

// stubBroker serves canned data for Execute tests.
type stubBroker struct {
	quote  broker.Quote
	chains map[string]broker.OptionChain
}

func (s *stubBroker) GetQuote(_ context.Context, _ string) (broker.Quote, error) {
	return s.quote, nil
}

func (s *stubBroker) GetOptionChain(_ context.Context, _ string, expiry time.Time) (broker.OptionChain, error) {
	return s.chains[expiry.Format("2006-01-02")], nil
}

func TestExecuteStatsConsistency(t *testing.T) {
	ts := dt(2026, 11, 8)
	contract := func(bid, ask string) *broker.Contract {
		return &broker.Contract{Bid: box.MustParsePrice(bid), Ask: box.MustParsePrice(ask)}
	}
	// Chain with strikes 5900 and 6150 (width 250) plus decoys.
	strikes := []broker.StrikeQuotes{
		{Strike: 5900, Call: contract("98", "98.10"), Put: contract("20", "20.20")},
		{Strike: 5925, Call: contract("80", "80.80"), Put: contract("25", "25.25")},
		{Strike: 6150, Call: contract("3", "3.10"), Put: contract("24", "24.20")},
		{Strike: 6175, Call: nil, Put: contract("20", "20.20")}, // missing call
	}
	ch := broker.OptionChain{Symbol: "SPX", Expiry: dt(2026, 12, 18), Timestamp: ts, Strikes: strikes}
	sb := &stubBroker{
		quote:  broker.Quote{Price: box.MustParsePrice("5912.30"), Timestamp: ts},
		chains: map[string]broker.OptionChain{"2026-12-18": ch},
	}

	p := testParams()
	p.Expiries = []time.Time{dt(2026, 12, 18)}
	p.Widths = []int64{250}
	p.K1RangePct = 0.02 // ±2% → K1 ∈ 5795..6030 step 5
	rows, stats, err := Execute(context.Background(), sb, p, ts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 48 {
		t.Errorf("Total = %d, want 48", stats.Total)
	}
	sum := stats.Valid + stats.Skipped
	for _, n := range stats.Rejected {
		sum += n
	}
	if sum != stats.Total {
		t.Errorf("Valid(%d)+Skipped(%d)+Rejected(%v) != Total(%d)", stats.Valid, stats.Skipped, stats.Rejected, stats.Total)
	}
	if len(rows) != stats.Valid {
		t.Errorf("rows %d != Valid %d", len(rows), stats.Valid)
	}
	if stats.Rejected[MissingQuote] != 47 {
		t.Errorf("Rejected[missing_quote] = %d, want 47", stats.Rejected[MissingQuote])
	}
	if len(rows) != 1 {
		t.Fatalf("valid rows = %d, want 1 (K1=5900, K2=6150)", len(rows))
	}
	r := rows[0]
	if r.Combo.K1 != 5900 || r.Combo.K2 != 6150 {
		t.Errorf("survivor = %+v", r.Combo)
	}
	// Verify the survivor's credit by hand: 98 − 3.10 − 20.20 + 24 = 98.70.
	if want := mp("98.70"); r.Credits.Natural != want {
		t.Errorf("C_natural = %v, want %v", r.Credits.Natural, want)
	}
	// Invariant: r_natural ≥ r_mid; credit within (0, W).
	if r.RNatural.Cmp(r.RMid) < 0 {
		t.Errorf("r_natural %v < r_mid %v", r.RNatural, r.RMid)
	}
	w := r.Combo.Width().Price()
	if r.Credits.Natural <= 0 || r.Credits.Natural >= w {
		t.Errorf("credit %v outside (0, %v)", r.Credits.Natural, w)
	}
}

func TestExecuteRequireQuotesFalse(t *testing.T) {
	ts := dt(2026, 11, 8)
	ch := broker.OptionChain{Symbol: "SPX", Expiry: dt(2026, 12, 18), Timestamp: ts}
	sb := &stubBroker{
		quote:  broker.Quote{Price: box.MustParsePrice("5912.30"), Timestamp: ts},
		chains: map[string]broker.OptionChain{"2026-12-18": ch},
	}
	p := testParams()
	p.RequireQuotes = false
	_, stats, err := Execute(context.Background(), sb, p, ts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped == 0 || stats.Rejected[MissingQuote] != 0 {
		t.Errorf("Skipped=%d Rejected[missing]=%d", stats.Skipped, stats.Rejected[MissingQuote])
	}
	if stats.Valid+stats.Skipped != stats.Total {
		t.Errorf("Valid(%d)+Skipped(%d) != Total(%d)", stats.Valid, stats.Skipped, stats.Total)
	}
}

func TestMaxDataAge(t *testing.T) {
	now := dt(2026, 11, 8).Add(12 * time.Hour)
	times := []time.Time{
		dt(2026, 11, 8).Add(11 * time.Hour),
		dt(2026, 11, 8).Add(10 * time.Hour),
	}
	if got := MaxDataAge(times, now); got != 2*time.Hour {
		t.Errorf("MaxDataAge = %v", got)
	}
	if got := MaxDataAge(nil, now); got != 0 {
		t.Errorf("MaxDataAge(nil) = %v", got)
	}
}
