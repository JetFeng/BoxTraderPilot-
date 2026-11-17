package watch

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"boxpilot/internal/alert"
	"boxpilot/internal/box"
	"boxpilot/internal/broker"
	"boxpilot/internal/scan"
)

func dt(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

var fixedNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// fakeBroker serves two box combos (5900/6150 and 5850/6100) whose leg
// mids and half-spreads are settable per test, plus a settable snapshot
// timestamp for staleness scenarios.
type fakeBroker struct {
	quotePrice box.Price
	ts         time.Time
	mid        [2][4]box.Price // [combo][SC, BC, BP, SP]
	hs         box.Price
}

func (f *fakeBroker) GetQuote(_ context.Context, _ string) (broker.Quote, error) {
	return broker.Quote{Price: f.quotePrice, Timestamp: f.ts}, nil
}

func (f *fakeBroker) GetOptionChain(_ context.Context, _ string, expiry time.Time) (broker.OptionChain, error) {
	mk := func(mid box.Price) *broker.Contract {
		return &broker.Contract{Bid: mid - f.hs, Ask: mid + f.hs, Last: mid}
	}
	strikes := []broker.StrikeQuotes{
		{Strike: 5850, Call: mk(f.mid[1][0]), Put: mk(f.mid[1][2])},
		{Strike: 5900, Call: mk(f.mid[0][0]), Put: mk(f.mid[0][2])},
		{Strike: 6100, Call: mk(f.mid[1][1]), Put: mk(f.mid[1][3])},
		{Strike: 6150, Call: mk(f.mid[0][1]), Put: mk(f.mid[0][3])},
	}
	return broker.OptionChain{Symbol: "SPX", Expiry: expiry, Timestamp: f.ts, Strikes: strikes}, nil
}

// recordingAlerter captures notifications.
type recordingAlerter struct {
	opps  []alert.Opportunity
	stale int
	err   error
}

func (r *recordingAlerter) Notify(_ context.Context, o alert.Opportunity) error {
	if r.err != nil {
		return r.err
	}
	r.opps = append(r.opps, o)
	return nil
}

func (r *recordingAlerter) NotifyStale(_ context.Context, _, _ time.Duration) error {
	if r.err != nil {
		return r.err
	}
	r.stale++
	return nil
}

// cheapMids keeps the box ~37bp below the 6.5% benchmark; expensiveMids
// is well above it. Values in points.
func cheapMids() [4]box.Price {
	return [4]box.Price{mp("131"), mp("6.5"), mp("6.5"), mp("131")}
}

func expensiveMids() [4]box.Price {
	return [4]box.Price{mp("124"), mp("10"), mp("10"), mp("124")}
}

func mp(s string) box.Price { return box.MustParsePrice(s) }

func testRules() Rules {
	return Rules{
		ThresholdBP:   -50,
		ConfirmRounds: 2,
		HysteresisBP:  25,
		Cooldown:      0,
	}
}

func newTestEngine(fb *fakeBroker, rec *recordingAlerter, rules Rules) *Engine {
	params := scanParams()
	return NewEngine(fb, params, 0.065, rules, rec, log.New(io.Discard, "", 0), func() time.Time { return fixedNow })
}

func scanParams() scan.Params {
	return scan.Params{
		Symbol:          "SPX",
		Expiries:        []time.Time{dt(2026, 12, 18)},
		Widths:          []int64{250},
		K1RangePct:      0.01, // 5900 ±1% → K1 ∈ 5845..5955 step 5
		StrikeStep:      5,
		MinDaysToExpiry: 21,
		RequireQuotes:   true,
		Filters: scan.Filters{
			MaxLegSpreadPct: 0.15,
			MinSellBid:      mp("0.05"),
		},
	}
}

func freshBroker() *fakeBroker {
	return &fakeBroker{
		quotePrice: mp("5900"),
		ts:         fixedNow,
		mid:        [2][4]box.Price{expensiveMids(), expensiveMids()},
		hs:         mp("0.005"),
	}
}

func TestWatchNoAlertWhenExpensive(t *testing.T) {
	fb := freshBroker()
	rec := &recordingAlerter{}
	e := newTestEngine(fb, rec, testRules())
	for i := 0; i < 3; i++ {
		if err := e.Round(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.opps) != 0 || rec.stale != 0 {
		t.Errorf("alerts = %d, stale = %d; want 0/0", len(rec.opps), rec.stale)
	}
}

func TestWatchConfirmRoundsAndHysteresis(t *testing.T) {
	fb := freshBroker()
	rec := &recordingAlerter{}
	e := newTestEngine(fb, rec, testRules())

	fb.mid[0] = cheapMids()
	if err := e.Round(context.Background()); err != nil { // round 1: cheap but unconfirmed
		t.Fatal(err)
	}
	if len(rec.opps) != 0 {
		t.Fatalf("alerted after 1 round despite ConfirmRounds=2")
	}
	if err := e.Round(context.Background()); err != nil { // round 2: confirmed → alert
		t.Fatal(err)
	}
	if len(rec.opps) != 1 {
		t.Fatalf("no alert after confirmation round")
	}
	if err := e.Round(context.Background()); err != nil { // round 3: same → no re-alert
		t.Fatal(err)
	}
	if len(rec.opps) != 1 {
		t.Fatalf("re-alerted without improvement")
	}

	// Improve by ≥25bp: raise SC/SP mids from 131 to 131.5 (credit
	// 248.98 → 249.98, spread −276bp → −643bp).
	fb.mid[0] = [4]box.Price{mp("131.5"), mp("6.5"), mp("6.5"), mp("131.5")}
	if err := e.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.opps) != 2 {
		t.Fatalf("no re-alert after %+.1fbp improvement; alerts=%d", rec.opps[len(rec.opps)-1].Result.SpreadBPS(0.065), len(rec.opps))
	}
}

func TestWatchEpisodeReset(t *testing.T) {
	fb := freshBroker()
	rec := &recordingAlerter{}
	e := newTestEngine(fb, rec, testRules())

	fb.mid[0] = cheapMids()
	e.Round(context.Background())
	e.Round(context.Background()) // alert #1
	if len(rec.opps) != 1 {
		t.Fatalf("alerts = %d, want 1", len(rec.opps))
	}

	fb.mid[0] = expensiveMids() // episode breaks
	e.Round(context.Background())
	if len(rec.opps) != 1 {
		t.Fatalf("alert during broken episode")
	}

	fb.mid[0] = cheapMids() // new episode needs re-confirmation
	e.Round(context.Background())
	if len(rec.opps) != 1 {
		t.Fatalf("alerted without re-confirmation")
	}
	e.Round(context.Background())
	if len(rec.opps) != 2 {
		t.Fatalf("alerts = %d, want 2 after re-confirmation", len(rec.opps))
	}
}

func TestWatchGlobalCooldown(t *testing.T) {
	fb := freshBroker()
	rec := &recordingAlerter{}
	rules := testRules()
	rules.Cooldown = time.Hour
	e := newTestEngine(fb, rec, rules)

	fb.mid[0] = cheapMids()
	fb.mid[1] = cheapMids()
	e.Round(context.Background())
	e.Round(context.Background()) // both combos confirmed, only one may alert
	if len(rec.opps) != 1 {
		t.Fatalf("alerts = %d, want 1 (cooldown)", len(rec.opps))
	}
	// Next round: both in alerted episode, no improvement → silence.
	e.Round(context.Background())
	if len(rec.opps) != 1 {
		t.Fatalf("alerts = %d, want 1", len(rec.opps))
	}
}

func TestWatchStaleSuspension(t *testing.T) {
	fb := freshBroker()
	rec := &recordingAlerter{}
	e := newTestEngine(fb, rec, testRules())

	fb.ts = fixedNow.Add(-30 * time.Minute)
	fb.mid[0] = cheapMids()
	e.Round(context.Background()) // stale → notice once, no opportunity
	if rec.stale != 1 || len(rec.opps) != 0 {
		t.Fatalf("stale=%d opps=%d, want 1/0", rec.stale, len(rec.opps))
	}
	e.Round(context.Background()) // still stale → no repeat notice
	if rec.stale != 1 {
		t.Fatalf("stale notice repeated: %d", rec.stale)
	}

	fb.ts = fixedNow // fresh again → alerts resume
	e.Round(context.Background())
	e.Round(context.Background())
	if len(rec.opps) != 1 {
		t.Fatalf("opps = %d, want 1 after recovery", len(rec.opps))
	}
	if rec.stale != 1 {
		t.Fatalf("stale = %d, want 1", rec.stale)
	}
}

func TestWatchStateGC(t *testing.T) {
	fb := freshBroker()
	rec := &recordingAlerter{}
	e := newTestEngine(fb, rec, testRules())

	fb.mid[0] = cheapMids()
	e.Round(context.Background())
	e.Round(context.Background())
	if len(rec.opps) != 1 {
		t.Fatalf("alerts = %d, want 1", len(rec.opps))
	}
	// Underlying moves away: both combos leave the K1 range, so their
	// trigger state must be garbage-collected.
	fb.quotePrice = mp("6300")
	fb.mid[0] = cheapMids()
	fb.mid[1] = expensiveMids()
	e.Round(context.Background())
	if len(e.state) != 0 {
		t.Fatalf("state size = %d after GC, want 0", len(e.state))
	}
}

func TestWatchDeliveryErrorNotMarkedAlerted(t *testing.T) {
	fb := freshBroker()
	rec := &recordingAlerter{err: errors.New("channel down")}
	e := newTestEngine(fb, rec, testRules())

	fb.mid[0] = cheapMids()
	e.Round(context.Background())
	if err := e.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.state["2026-12-18|5900|6150"]
	if st == nil || st.alerted {
		t.Fatalf("alerted flag set despite delivery error")
	}
}

func TestRoundErrorPropagates(t *testing.T) {
	fb := freshBroker()
	rec := &recordingAlerter{}
	e := newTestEngine(fb, rec, testRules())
	e.params.Symbol = "MISSING" // fixture/stub would fail; fakeBroker ignores symbol
	// Use an erroring broker instead:
	errBroker := &failingBroker{}
	e2 := NewEngine(errBroker, e.params, 0.065, testRules(), rec, log.New(io.Discard, "", 0), nil)
	if err := e2.Round(context.Background()); err == nil {
		t.Fatal("expected round error")
	}
}

type failingBroker struct{}

func (failingBroker) GetQuote(context.Context, string) (broker.Quote, error) {
	return broker.Quote{}, errors.New("boom")
}
func (failingBroker) GetOptionChain(context.Context, string, time.Time) (broker.OptionChain, error) {
	return broker.OptionChain{}, errors.New("boom")
}

// --- Snapshot and runtime reconfiguration -------------------------------

func TestSnapshotExposesTheLastRound(t *testing.T) {
	fb := &fakeBroker{quotePrice: mp("5912.30"), ts: fixedNow, mid: [2][4]box.Price{cheapMids(), cheapMids()}, hs: mp("0.05")}
	rec := &recordingAlerter{}
	e := newTestEngine(fb, rec, testRules())

	if err := e.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := e.Snapshot()
	if snap.LastRoundAt.IsZero() {
		t.Error("LastRoundAt is zero")
	}
	if len(snap.Rows) == 0 {
		t.Fatal("snapshot has no rows")
	}
	if snap.Stats.Valid != len(snap.Rows) {
		t.Errorf("stats valid %d != rows %d", snap.Stats.Valid, len(snap.Rows))
	}
	if snap.Stats.Underlying != mp("5912.30") {
		t.Errorf("underlying = %v, want 5912.30", snap.Stats.Underlying)
	}
	if snap.LastError != "" {
		t.Errorf("LastError = %q, want empty", snap.LastError)
	}
	if snap.Benchmark != 0.065 {
		t.Errorf("benchmark = %v", snap.Benchmark)
	}
	// cheapMids is ~37bp below the benchmark, so it clears the -50bp
	// threshold and must show up as an opportunity.
	if len(snap.Opportunities) == 0 {
		t.Error("no opportunities in snapshot")
	}
	for _, o := range snap.Opportunities {
		if o.Consecutive < 1 {
			t.Errorf("opportunity %v has consecutive = %d, want >= 1", o, o.Consecutive)
		}
	}
}

func TestSnapshotIsEmptyBeforeTheFirstRound(t *testing.T) {
	fb := &fakeBroker{quotePrice: mp("5912.30"), ts: fixedNow, mid: [2][4]box.Price{cheapMids(), cheapMids()}, hs: mp("0.05")}
	e := newTestEngine(fb, &recordingAlerter{}, testRules())
	snap := e.Snapshot()
	if len(snap.Rows) != 0 {
		t.Errorf("rows = %d, want 0 before the first round", len(snap.Rows))
	}
	if !snap.LastRoundAt.IsZero() {
		t.Errorf("LastRoundAt = %v, want zero", snap.LastRoundAt)
	}
}

func TestUpdateRulesTakesEffectImmediately(t *testing.T) {
	fb := &fakeBroker{quotePrice: mp("5912.30"), ts: fixedNow, mid: [2][4]box.Price{cheapMids(), cheapMids()}, hs: mp("0.05")}
	rec := &recordingAlerter{}
	e := newTestEngine(fb, rec, testRules())

	// At a -50bp threshold the cheap combo qualifies.
	if err := e.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.Snapshot().Opportunities) == 0 {
		t.Fatal("expected an opportunity at the default threshold")
	}

	// A threshold below the actual spread excludes it again.
	e.UpdateRules(Rules{ThresholdBP: -10000, ConfirmRounds: 1, HysteresisBP: 25}, 0.065)
	if err := e.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := e.Snapshot()
	if snap.Rules.ThresholdBP != -10000 {
		t.Errorf("threshold = %v, want -10000", snap.Rules.ThresholdBP)
	}
	if len(snap.Opportunities) != 0 {
		t.Errorf("opportunities = %d, want 0 at a -10000bp threshold", len(snap.Opportunities))
	}
}

func TestUpdateRulesRecomputesSpreadsAgainstANewBenchmark(t *testing.T) {
	fb := &fakeBroker{quotePrice: mp("5912.30"), ts: fixedNow, mid: [2][4]box.Price{cheapMids(), cheapMids()}, hs: mp("0.05")}
	e := newTestEngine(fb, &recordingAlerter{}, testRules())

	if err := e.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := e.Snapshot().Opportunities
	if len(before) == 0 {
		t.Fatal("expected an opportunity at the default benchmark")
	}

	// Raising the benchmark makes the same combo look more attractive:
	// spread_bp = r_natural − benchmark, so a higher benchmark pushes
	// the spread further negative (the box borrows more cheaply than
	// the alternative).
	e.UpdateRules(testRules(), 0.10)
	if err := e.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := e.Snapshot().Opportunities
	if len(after) == 0 {
		t.Fatal("expected an opportunity at the raised benchmark")
	}
	if after[0].SpreadBP >= before[0].SpreadBP {
		t.Errorf("spread %v did not fall after raising the benchmark (was %v)",
			after[0].SpreadBP, before[0].SpreadBP)
	}
}

func TestUpdateParamsSwapsTheScanScope(t *testing.T) {
	fb := &fakeBroker{quotePrice: mp("5912.30"), ts: fixedNow, mid: [2][4]box.Price{cheapMids(), cheapMids()}, hs: mp("0.05")}
	e := newTestEngine(fb, &recordingAlerter{}, testRules())

	p := scanParams()
	p.Widths = []int64{250}
	e.UpdateParams(p)
	if err := e.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.Snapshot().Rows {
		if r.Combo.Width() != 250 {
			t.Fatalf("width = %d, want 250", r.Combo.Width())
		}
	}
}
