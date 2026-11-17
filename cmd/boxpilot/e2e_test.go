// End-to-end acceptance test (§7, §8.1): runs the whole CLI against the
// checked-in fake fixture with zero network access and validates the
// table/CSV output ordering and numbers.
package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/broker/fixture"
)

const fixturePath = "../../testdata/spx_chain_fixture.json"

var fixtureNow = time.Date(2026, 11, 6, 21, 5, 0, 0, time.UTC) // just after as_of

// runCLI runs the CLI with separate data and diagnostics buffers so the
// tests can assert that machine-readable output stays clean.
func runCLI(t *testing.T, args []string, now time.Time) (data, diag string, code int) {
	t.Helper()
	var dataBuf, diagBuf strings.Builder
	code = run(args, &dataBuf, &diagBuf, now)
	return dataBuf.String(), diagBuf.String(), code
}

func writeE2EConfig(t *testing.T, csvPath string, format string) string {
	t.Helper()
	csvLine := ""
	if csvPath != "" {
		csvLine = fmt.Sprintf("  csv_path: %q\n", csvPath)
	}
	yaml := fmt.Sprintf(`
symbol: SPX
broker: fixture
fixture:
  path: %q
scan:
  expiries: ["2026-12-18", "2027-01-15", "2027-03-19"]
  widths: [250, 500, 1000]
  k1_range_pct: 0.10
  strike_step: 5
  min_days_to_expiry: 21
benchmark:
  rate: 0.065
filters:
  max_leg_spread_pct: 0.15
  min_bid: 0.05
  require_quotes: true
output:
  format: %s
%s  top: 30
`, fixturePath, format, csvLine)
	p := filepath.Join(t.TempDir(), "boxpilot.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// dataRows extracts the whitespace-separated data lines of the table.
func dataRows(t *testing.T, out string) [][]string {
	t.Helper()
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 11 && len(f[0]) == 10 && f[0][4] == '-' {
			rows = append(rows, f)
		}
	}
	if len(rows) == 0 {
		t.Fatalf("no data rows in output:\n%s", out)
	}
	return rows
}

func TestEndToEndFixtureScan(t *testing.T) {
	cfg := writeE2EConfig(t, "", "table")
	data, _, code := runCLI(t, []string{"-config", cfg}, fixtureNow)
	if code != 0 {
		t.Fatalf("exit code %d\n%s", code, data)
	}
	got := data

	// Header columns per §3.2-5.
	for _, col := range []string{"EXPIRY", "D", "K1", "K2", "WIDTH", "C_MID", "C_NATURAL", "R_MID_PCT", "R_NATURAL_PCT", "SPREAD_BP", "LEGS_OK"} {
		if !strings.Contains(got, col) {
			t.Errorf("output missing column %q", col)
		}
	}
	// Summary sections per §3.2-6.
	for _, want := range []string{"combos scanned:", "combos valid:", "rejected:", "best combo:", "data timestamps:", "benchmark rate:   6.50%"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// Data is fresh (5 min old) → no staleness warning.
	if strings.Contains(got, "WARNING") {
		t.Errorf("unexpected staleness warning:\n%s", got)
	}

	rows := dataRows(t, got)
	if len(rows) > 30 {
		t.Errorf("table shows %d rows, top=30", len(rows))
	}
	// Non-decreasing r_natural (4dp printing can collide; strictness of
	// the underlying exact sort is covered by scan's unit test).
	prev := -1.0
	for i, f := range rows {
		rnat, err := strconv.ParseFloat(f[8], 64)
		if err != nil {
			t.Fatalf("row %d: bad r_natural %q", i, f[8])
		}
		rmid, _ := strconv.ParseFloat(f[7], 64)
		spread, _ := strconv.ParseFloat(f[9], 64)
		if i > 0 && rnat < prev {
			t.Errorf("row %d: r_natural %v out of order after %v", i, rnat, prev)
		}
		if rnat < rmid {
			t.Errorf("row %d: r_natural %v < r_mid %v", i, rnat, rmid)
		}
		if want := (rnat/100 - 0.065) * 10000; abs(spread-want) > 0.15 {
			t.Errorf("row %d: spread_bp %v, want %v", i, spread, want)
		}
		prev = rnat
	}

	// Independently recompute the top row from the fixture quotes.
	verifyTopRow(t, rows[0])
}

// verifyTopRow recomputes credits and rate for the top combo from the
// fixture file and compares them with the printed row.
func verifyTopRow(t *testing.T, f []string) {
	t.Helper()
	fx, err := fixture.Load(fixturePath)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	expStr, k1s, k2s := f[0], f[2], f[3]
	k1, _ := strconv.ParseInt(k1s, 10, 64)
	k2, _ := strconv.ParseInt(k2s, 10, 64)

	var chain *fixture.Chain
	for i := range fx.Chains {
		if fx.Chains[i].Expiry == expStr {
			chain = &fx.Chains[i]
		}
	}
	if chain == nil {
		t.Fatalf("expiry %s not in fixture", expStr)
	}
	at := func(k int64) *fixture.Strike {
		for i := range chain.Strikes {
			if chain.Strikes[i].Strike == k {
				return &chain.Strikes[i]
			}
		}
		return nil
	}
	s1, s2 := at(k1), at(k2)
	if s1 == nil || s2 == nil {
		t.Fatalf("strikes %d/%d not in fixture", k1, k2)
	}
	if s1.Call == nil || s2.Call == nil || s1.Put == nil || s2.Put == nil {
		t.Fatal("fixture leg missing")
	}
	q := func(c *fixture.Contract) box.Quote {
		bid, err1 := box.ParsePrice(c.Bid)
		ask, err2 := box.ParsePrice(c.Ask)
		if err1 != nil || err2 != nil {
			t.Fatal("fixture price parse")
		}
		return box.Quote{Bid: bid, Ask: ask}
	}
	legs := box.LegQuotes{
		SellCallK1: q(s1.Call), BuyCallK2: q(s2.Call),
		BuyPutK1: q(s1.Put), SellPutK2: q(s2.Put),
	}
	cr := box.Evaluate(legs)
	exp, _ := time.Parse("2006-01-02", expStr)
	D := int(exp.Sub(time.Date(fixtureNow.Year(), fixtureNow.Month(), fixtureNow.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
	r := box.AnnualRate(cr.Natural, box.Strike(k2-k1).Price(), D)

	printedC, _ := strconv.ParseFloat(f[6], 64)
	if abs(printedC-cr.Natural.Float64()) > 0.001 {
		t.Errorf("C_natural = %v, recomputed %v", printedC, cr.Natural.Float64())
	}
	printedR, _ := strconv.ParseFloat(f[8], 64)
	if abs(printedR-r.Percent()) > 0.01 {
		t.Errorf("r_natural_pct = %v, recomputed %v", printedR, r.Percent())
	}
	printedD, _ := strconv.Atoi(f[1])
	if printedD != D {
		t.Errorf("D = %d, want %d", printedD, D)
	}
}

func TestEndToEndStaleWarning(t *testing.T) {
	cfg := writeE2EConfig(t, "", "table")
	data, _, code := runCLI(t, []string{"-config", cfg}, fixtureNow.Add(30*time.Minute))
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(data, "WARNING: market data is") {
		t.Errorf("expected staleness warning:\n%s", data)
	}
}

func TestEndToEndCSVExport(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "out.csv")
	cfg := writeE2EConfig(t, csvPath, "table")
	data, _, code := runCLI(t, []string{"-config", cfg}, fixtureNow)
	if code != 0 {
		t.Fatalf("exit code %d\n%s", code, data)
	}
	recs, err := csv.NewReader(mustOpen(t, csvPath)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 2 {
		t.Fatalf("csv has %d rows", len(recs))
	}
	if recs[0][0] != "expiry" || recs[0][10] != "legs_ok" {
		t.Errorf("csv header = %v", recs[0])
	}
	// CSV data must match the table order (same run).
	tableRows := dataRows(t, data)
	if len(recs)-1 != len(tableRows) {
		t.Errorf("csv rows %d != table rows %d", len(recs)-1, len(tableRows))
	}
	if recs[1][0] != tableRows[0][0] || recs[1][2] != tableRows[0][2] {
		t.Errorf("csv first row %v != table first row %v", recs[1], tableRows[0])
	}
}

// TestEndToEndJSONFormat asserts that the json format writes a strictly
// parseable JSON document to stdout. The run summary is a diagnostic and
// must not be mixed into the payload.
func TestEndToEndJSONFormat(t *testing.T) {
	cfg := writeE2EConfig(t, "", "json")
	data, diag, code := runCLI(t, []string{"-config", cfg}, fixtureNow)
	if code != 0 {
		t.Fatalf("exit code %d\n%s", code, diag)
	}

	var rows []struct {
		Expiry   string  `json:"expiry"`
		D        int     `json:"D"`
		K1       int64   `json:"K1"`
		Width    int64   `json:"width"`
		SpreadBP float64 `json:"spread_bp"`
	}
	if err := json.Unmarshal([]byte(data), &rows); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n--- stdout ---\n%s", err, data)
	}
	if len(rows) == 0 {
		t.Fatal("json payload has no rows")
	}
	if rows[0].SpreadBP == 0 && rows[0].Width == 0 {
		t.Errorf("json row looks empty: %+v", rows[0])
	}
	// The summary must have been diverted to the diagnostics stream.
	if !strings.Contains(diag, "=== summary ===") {
		t.Errorf("summary missing from diagnostics stream:\n%s", diag)
	}
	if strings.Contains(data, "=== summary ===") {
		t.Errorf("summary leaked into the json payload:\n%s", data)
	}
	// No ANSI escape codes in machine-readable output.
	if strings.Contains(data, "\x1b[") {
		t.Errorf("json payload contains ANSI escape codes")
	}
}

// TestEndToEndCSVFormat asserts that the csv format writes a strictly
// parseable CSV document to stdout (regression: the run summary used to
// be appended to stdout and corrupted the payload).
func TestEndToEndCSVFormat(t *testing.T) {
	cfg := writeE2EConfig(t, "", "csv")
	data, diag, code := runCLI(t, []string{"-config", cfg}, fixtureNow)
	if code != 0 {
		t.Fatalf("exit code %d\n%s", code, diag)
	}

	recs, err := csv.NewReader(strings.NewReader(data)).ReadAll()
	if err != nil {
		t.Fatalf("stdout is not valid CSV: %v\n--- stdout ---\n%s", err, data)
	}
	if len(recs) < 2 {
		t.Fatalf("csv has %d rows", len(recs))
	}
	if recs[0][0] != "expiry" || recs[0][10] != "legs_ok" {
		t.Errorf("csv header = %v", recs[0])
	}
	for i, r := range recs {
		if len(r) != 11 {
			t.Fatalf("csv row %d has %d fields, want 11: %v", i, len(r), r)
		}
	}
	if !strings.Contains(diag, "=== summary ===") {
		t.Errorf("summary missing from diagnostics stream:\n%s", diag)
	}
	if strings.Contains(data, "=== summary ===") {
		t.Errorf("summary leaked into the csv payload:\n%s", data)
	}
	if strings.Contains(data, "\x1b[") {
		t.Errorf("csv payload contains ANSI escape codes")
	}
}

func TestEndToEndMissingFixtureFails(t *testing.T) {
	cfg := writeE2EConfig(t, "", "table")
	_, _, code := runCLI(t, []string{"-config", cfg, "-fixture", filepath.Join(t.TempDir(), "nope.json")}, fixtureNow)
	if code == 0 {
		t.Errorf("expected non-zero exit for missing fixture")
	}
}

func TestEndToEndWatchRejectsUnknownChannel(t *testing.T) {
	cfg := writeE2EConfig(t, "", "table")
	// Append an invalid watch section.
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\nwatch:\n  interval: 30s\n  channels: [pager]\n")...)
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, diag, code := runCLI(t, []string{"-config", cfg, "-watch"}, fixtureNow)
	if code == 0 {
		t.Errorf("expected non-zero exit for unknown watch channel")
	}
	if !strings.Contains(diag, "pager") {
		t.Errorf("error should name the bad channel:\n%s", diag)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
