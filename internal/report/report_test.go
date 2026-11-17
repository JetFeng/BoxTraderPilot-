package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/scan"
)

func dt(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func sampleRows() []scan.Result {
	w := box.Strike(250).Price() // 250 points in Price units
	mk := func(k1 int64, nat, mid string, days int) scan.Result {
		cn := box.MustParsePrice(nat)
		cm := box.MustParsePrice(mid)
		legs := box.LegQuotes{
			SellCallK1: box.Quote{Bid: cn / 2, Ask: cn/2 + 100},
			BuyCallK2:  box.Quote{Bid: 300, Ask: 400},
			BuyPutK1:   box.Quote{Bid: 300, Ask: 400},
			SellPutK2:  box.Quote{Bid: cn / 2, Ask: cn/2 + 100},
		}
		return scan.Result{
			Combo:    box.Combo{Expiry: dt(2026, 12, 18), K1: box.Strike(k1), K2: box.Strike(k1 + 250), D: days},
			Legs:     legs,
			Credits:  box.Credits{Natural: cn, Mid: cm},
			RNatural: box.AnnualRate(cn, w, days),
			RMid:     box.AnnualRate(cm, w, days),
		}
	}
	// Higher credit → lower rate; build in non-sorted order. The last row
	// is cheap enough (2.46%) to be below the 6.5% benchmark → negative
	// spread.
	return []scan.Result{
		mk(5700, "240", "242", 40),
		mk(5900, "200", "205", 40),
		mk(6100, "244", "245", 365),
	}
}

func testOpts() Options {
	return Options{Benchmark: 0.065, Now: dt(2026, 11, 8)}
}

func TestRenderTableColumnsAndHighlight(t *testing.T) {
	rows := sampleRows()
	var buf bytes.Buffer
	if err := RenderTable(&buf, rows, testOpts()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, col := range []string{"EXPIRY", "D", "K1", "K2", "WIDTH", "C_MID", "C_NATURAL", "R_MID_PCT", "R_NATURAL_PCT", "SPREAD_BP", "LEGS_OK"} {
		if !strings.Contains(out, col) {
			t.Errorf("table missing column %q", col)
		}
	}
	if !strings.Contains(out, "2026-12-18") || !strings.Contains(out, "5900") {
		t.Errorf("table missing data rows:\n%s", out)
	}
	// r_natural for the 200-credit row: (250−200)/200 × 365/40 = 2.28125
	// fraction = 228.1250 percent.
	if !strings.Contains(out, "228.1250") {
		t.Errorf("expected r_natural 228.1250 in table:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("color codes must be off by default:\n%s", out)
	}
	// spread = 5.703125% − 6.5% = −79.7bp → negative highlighted in color mode.
	opts := testOpts()
	opts.Color = true
	buf.Reset()
	if err := RenderTable(&buf, rows, opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\x1b[32m") {
		t.Errorf("expected green highlight for negative spread:\n%s", buf.String())
	}
}

func TestWriteCSVRoundTrip(t *testing.T) {
	rows := sampleRows()
	var buf bytes.Buffer
	if err := WriteCSV(&buf, rows, testOpts()); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != len(rows)+1 {
		t.Fatalf("rows = %d", len(recs))
	}
	for i, h := range Header {
		if recs[0][i] != h {
			t.Errorf("header[%d] = %q, want %q", i, recs[0][i], h)
		}
	}
	// Recompute the r_natural cell of row 1 from its C_natural; CSV is
	// printed with 4 decimals, so compare within formatting tolerance.
	r := rows[0]
	want := r.RNatural.Percent()
	var got float64
	if _, err := fmt.Sscanf(recs[1][8], "%g", &got); err != nil {
		t.Fatalf("parse r_natural_pct: %v", err)
	}
	if diff := got - want; diff > 1e-4 || diff < -1e-4 {
		t.Errorf("r_natural_pct = %v, want %v ±1e-4", got, want)
	}
	if recs[1][10] != "4" {
		t.Errorf("legs_ok = %q", recs[1][10])
	}
}

func TestWriteJSON(t *testing.T) {
	rows := sampleRows()
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rows, testOpts()); err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(rows) {
		t.Fatalf("decoded %d rows", len(decoded))
	}
	if decoded[0]["expiry"] != "2026-12-18" || decoded[0]["legs_ok"] != float64(4) {
		t.Errorf("unexpected json row: %v", decoded[0])
	}
	if decoded[0]["K1"] != float64(5700) || decoded[0]["K2"] != float64(5950) {
		t.Errorf("unexpected strikes: %v", decoded[0])
	}
}

func TestRenderSummary(t *testing.T) {
	rows := sampleRows()
	stats := scan.Stats{
		Total:     472,
		Valid:     len(rows),
		Rejected:  map[scan.RejectReason]int{scan.MissingQuote: 100, scan.BadQuote: 3},
		DataTimes: []time.Time{dt(2026, 11, 8).Add(-30 * time.Minute)},
	}
	var buf bytes.Buffer
	RenderSummary(&buf, stats, &rows[0], Options{Benchmark: 0.065, Now: dt(2026, 11, 8)})
	out := buf.String()
	for _, want := range []string{"472", "100", "bad_quote", "6.50%", "WARNING", "best combo", "SELL K1 call", "BUY  K2 call"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	// Fresh data → no warning.
	stats.DataTimes = []time.Time{dt(2026, 11, 8)}
	buf.Reset()
	RenderSummary(&buf, stats, &rows[0], Options{Benchmark: 0.065, Now: dt(2026, 11, 8)})
	if strings.Contains(buf.String(), "WARNING") {
		t.Errorf("unexpected staleness warning:\n%s", buf.String())
	}
	// Nil best with zero valid.
	stats.Valid = 0
	buf.Reset()
	RenderSummary(&buf, stats, nil, Options{Benchmark: 0.065, Now: dt(2026, 11, 8)})
	if !strings.Contains(buf.String(), "no valid combos") {
		t.Errorf("expected 'no valid combos'")
	}
}
