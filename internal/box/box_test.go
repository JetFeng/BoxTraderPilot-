package box

import (
	"math/rand"
	"testing"
	"time"
)

func TestParsePrice(t *testing.T) {
	cases := []struct {
		in   string
		want Price
	}{
		{"12.35", 123500},
		{"12.3", 123000},
		{"0.05", 500},
		{"7", 70000},
		{"5912.30", 59123000},
		{"-1.25", -12500},
		{"+2.5", 25000},
		{" 3.10 ", 31000},
	}
	for _, c := range cases {
		got, err := ParsePrice(c.in)
		if err != nil {
			t.Fatalf("ParsePrice(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParsePrice(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "1.23456", "1.2.3"} {
		if _, err := ParsePrice(bad); err == nil {
			t.Errorf("ParsePrice(%q) expected error", bad)
		}
	}
}

func TestPriceStringAndFloat(t *testing.T) {
	if got := MustParsePrice("12.35").String(); got != "12.3500" {
		t.Errorf("String() = %q", got)
	}
	if got := MustParsePrice("245").Float64(); got != 245 {
		t.Errorf("Float64() = %v", got)
	}
}

func TestAnnualRateHandCases(t *testing.T) {
	// r = (W − C)/C × 365/D, 365-day basis.
	cases := []struct {
		name          string
		credit, width Price
		days          int
		want          float64
	}{
		{"flat year", MustParsePrice("200"), MustParsePrice("250"), 365, 0.25},
		{"half year", MustParsePrice("100"), MustParsePrice("200"), 180, 365.0 / 180.0},
		{"40d box", MustParsePrice("240"), MustParsePrice("250"), 40, (10.0 / 240.0) * (365.0 / 40.0)},
		{"credit near width", MustParsePrice("249.9999"), MustParsePrice("250"), 365, (0.0001 / 249.9999)},
		{"deep discount", MustParsePrice("1"), MustParsePrice("250"), 365, 249.0 * (365.0 / 365.0) / 1.0},
	}
	for _, c := range cases {
		got := AnnualRate(c.credit, c.width, c.days).Float64()
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAnnualRateRejects365vs360Mix(t *testing.T) {
	// Hand-check: (W−C)/C × 365/D with D=180 must equal 365/180 for C=W/2.
	r := AnnualRate(MustParsePrice("125"), MustParsePrice("250"), 180)
	if r.Float64() != 365.0/180.0 {
		t.Fatalf("365 basis violated: got %v", r.Float64())
	}
	if r.Float64() == 360.0/180.0 {
		t.Fatal("rate must not use a 360-day basis")
	}
}

func TestAnnualRateInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		credit, width Price
		days          int
	}{
		{0, MustParsePrice("250"), 40},
		{-5, MustParsePrice("250"), 40},
		{MustParsePrice("100"), MustParsePrice("250"), 0},
	} {
		if r := AnnualRate(tc.credit, tc.width, tc.days); r.Den != 0 {
			t.Errorf("AnnualRate(%v, %v, %d) = %+v, want zero Rate", tc.credit, tc.width, tc.days, r)
		}
	}
}

func TestEvaluateDirection(t *testing.T) {
	// Wide spreads on sell legs / tight on buy legs must push the
	// natural credit below the mid credit.
	l := LegQuotes{
		SellCallK1: Quote{Bid: 98, Ask: 102},
		BuyCallK2:  Quote{Bid: 3, Ask: 7},
		BuyPutK1:   Quote{Bid: 3, Ask: 7},
		SellPutK2:  Quote{Bid: 98, Ask: 102},
	}
	cr := Evaluate(l)
	if cr.Natural != 98-7-7+98 {
		t.Errorf("Natural = %v", cr.Natural)
	}
	if cr.Mid != 100-5-5+100 {
		t.Errorf("Mid = %v", cr.Mid)
	}
	if !(cr.Natural < cr.Mid) {
		t.Errorf("natural (%v) must be below mid (%v) here", cr.Natural, cr.Mid)
	}
}

// TestNaturalNeverBelowMid is the §7 invariant: for every combo with
// positive quotes, r_natural ≥ r_mid. Written backwards it must fail.
func TestNaturalNeverBelowMid(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 10000; i++ {
		mk := func() Quote {
			bid := 1 + rng.Int63n(500_000) // 0.0001 .. 50.0000
			spread := 1 + rng.Int63n(20_000)
			return Quote{Bid: Price(bid), Ask: Price(bid + spread)}
		}
		l := LegQuotes{mk(), mk(), mk(), mk()}
		cr := Evaluate(l)
		const W = Price(250) * PriceScale
		if cr.Natural <= 0 || cr.Mid <= 0 {
			continue
		}
		if cr.Natural >= W {
			continue // filtered out by scan as anomalous quote
		}
		days := 1 + rng.Intn(1000)
		rn := AnnualRate(cr.Natural, W, days)
		rm := AnnualRate(cr.Mid, W, days)
		if rn.Cmp(rm) < 0 {
			t.Fatalf("r_natural %v < r_mid %v for %+v", rn, rm, l)
		}
	}
}

func TestRateCmpExact(t *testing.T) {
	// Two rates that are equal but would compare unequal in float64
	// after careless rounding must compare equal here.
	a := Rate{Num: 1, Den: 3}
	b := Rate{Num: 2, Den: 6}
	if a.Cmp(b) != 0 {
		t.Errorf("1/3 vs 2/6: got %d", a.Cmp(b))
	}
	third := Rate{Num: 1, Den: 3}
	quarter := Rate{Num: 1, Den: 4}
	if third.Cmp(quarter) <= 0 {
		t.Errorf("1/3 must exceed 1/4")
	}
	if AnnualRate(MustParsePrice("10"), MustParsePrice("250"), 40).Cmp(
		AnnualRate(MustParsePrice("20"), MustParsePrice("250"), 40)) <= 0 {
		t.Errorf("smaller credit must imply higher rate")
	}
}

func TestComboWidthAndExpiry(t *testing.T) {
	c := Combo{Expiry: time.Date(2026, 12, 18, 0, 0, 0, 0, time.UTC), K1: 5900, K2: 6150}
	if c.Width() != 250 {
		t.Errorf("Width() = %d", c.Width())
	}
	if c.Width().Price() != 250*PriceScale {
		t.Errorf("Width().Price() = %v", c.Width().Price())
	}
}
