// Tests that apply to every build, tagged or not. Keeping them out of
// the build-tag-specific files means the WhatsApp build does not lose
// coverage of the shared alerting logic.
package alert

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/scan"
)

func sampleOpportunity() Opportunity {
	w := box.Strike(250).Price()
	legs := box.LegQuotes{
		SellCallK1: box.Quote{Bid: box.MustParsePrice("715.82"), Ask: box.MustParsePrice("730.88")},
		BuyCallK2:  box.Quote{Bid: box.MustParsePrice("93.65"), Ask: box.MustParsePrice("97.56")},
		BuyPutK1:   box.Quote{Bid: box.MustParsePrice("11.14"), Ask: box.MustParsePrice("11.68")},
		SellPutK2:  box.Quote{Bid: box.MustParsePrice("356.10"), Ask: box.MustParsePrice("364.42")},
	}
	cr := box.Evaluate(legs)
	return Opportunity{
		Symbol:    "SPX",
		Benchmark: 0.065,
		Result: scan.Result{
			Combo:    box.Combo{Expiry: time.Date(2027, 3, 19, 0, 0, 0, 0, time.UTC), K1: 5325, K2: 6325, D: 131},
			Legs:     legs,
			Credits:  cr,
			RNatural: box.AnnualRate(cr.Natural, w, 131),
			RMid:     box.AnnualRate(cr.Mid, w, 131),
		},
	}
}

func TestOpportunityText(t *testing.T) {
	o := sampleOpportunity()
	txt := o.Text()
	for _, want := range []string{
		"ALERT: SPX short box",
		"6.50%",
		"2027-03-19",
		"K1=5325 K2=6325 width=1000",
		"r_natural", "r_mid", "spread",
		"C_natural", "C_mid",
		"SELL 5325 call", "BUY  6325 call", "BUY  5325 put", "SELL 6325 put",
		"715.8200/730.8800",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("message missing %q:\n%s", want, txt)
		}
	}
	// Hand-check the natural credit: 715.82 − 97.56 − 11.68 + 356.10.
	if got := o.Result.Credits.Natural; got != box.MustParsePrice("962.68") {
		t.Errorf("C_natural = %v, want 962.6800", got)
	}
}

func TestStaleText(t *testing.T) {
	got := StaleText(42*time.Minute, 15*time.Minute)
	if !strings.Contains(got, "42m") || !strings.Contains(got, "15m") || !strings.Contains(got, "suspended") {
		t.Errorf("stale text = %q", got)
	}
}

func TestParseE164(t *testing.T) {
	for in, want := range map[string]string{
		"14155550123":     "14155550123",
		"+14155550123":    "14155550123",
		" 8613800138000 ": "8613800138000",
	} {
		got, err := ParseE164(in)
		if err != nil || got != want {
			t.Errorf("ParseE164(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "123", "1234567890123456", "12a4567", "+", "+-1234567"} {
		if _, err := ParseE164(bad); err == nil {
			t.Errorf("ParseE164(%q) accepted", bad)
		}
	}
}

type fakeAlerter struct {
	notified int
	stale    int
	err      error
}

func (f *fakeAlerter) Notify(context.Context, Opportunity) error {
	if f.err != nil {
		return f.err
	}
	f.notified++
	return nil
}

func (f *fakeAlerter) NotifyStale(context.Context, time.Duration, time.Duration) error {
	if f.err != nil {
		return f.err
	}
	f.stale++
	return nil
}

func TestMultiFansOutAndAggregates(t *testing.T) {
	a, b := &fakeAlerter{}, &fakeAlerter{err: errors.New("b down")}
	m := NewMulti(a, b)
	if err := m.Notify(context.Background(), sampleOpportunity()); err == nil {
		t.Fatal("expected aggregated error")
	}
	if a.notified != 1 || b.notified != 0 {
		t.Errorf("a=%d b=%d", a.notified, b.notified)
	}
	ok1, ok2 := &fakeAlerter{}, &fakeAlerter{}
	m = NewMulti(ok1, ok2)
	if err := m.NotifyStale(context.Background(), time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok1.stale != 1 || ok2.stale != 1 {
		t.Errorf("fan-out stale a=%d b=%d", ok1.stale, ok2.stale)
	}
}

func TestLogAlerterWrites(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogAlerter(&buf)
	if err := l.Notify(context.Background(), sampleOpportunity()); err != nil {
		t.Fatal(err)
	}
	if err := l.NotifyStale(context.Background(), time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "ALERT") || !strings.Contains(out, "WARNING") {
		t.Errorf("log output missing alerts:\n%s", out)
	}
}
