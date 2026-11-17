// Package report renders scan results as table, CSV, or JSON, plus the
// run summary. All amounts are shown in index points (USD = points ×
// 100); rates use the 365-day convention.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/scan"
)

// ansiGreen/greenReset highlight spreads cheaper than the benchmark.
const (
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiReset  = "\x1b[0m"
)

// color returns the ANSI escape when enabled, or an empty string when
// the destination is not a terminal. Keeping escape codes out of
// non-terminal output is what makes the csv/json payloads machine
// readable.
func color(enabled bool, code string) string {
	if !enabled {
		return ""
	}
	return code
}

// Options tweak rendering.
type Options struct {
	Benchmark float64 // plain fraction, e.g. 0.065
	Color     bool    // ANSI highlight for negative spreads (data stream)
	// DiagColor enables ANSI in the summary. It is separate from Color
	// because the summary may be written to the diagnostics stream
	// while the data goes to stdout.
	DiagColor bool
	Now       time.Time
}

// Header is the shared column set (§3.2-5).
var Header = []string{
	"expiry", "D", "K1", "K2", "width", "C_mid", "C_natural",
	"r_mid_pct", "r_natural_pct", "spread_bp", "legs_ok",
}

func rowStrings(r scan.Result, o Options) []string {
	c := r.Combo
	spread := r.SpreadBPS(o.Benchmark)
	return []string{
		c.Expiry.Format("2006-01-02"),
		fmt.Sprintf("%d", c.D),
		fmt.Sprintf("%d", c.K1),
		fmt.Sprintf("%d", c.K2),
		fmt.Sprintf("%d", c.Width()),
		fmt.Sprintf("%.4f", r.Credits.Mid.Float64()),
		fmt.Sprintf("%.4f", r.Credits.Natural.Float64()),
		fmt.Sprintf("%.4f", r.RMid.Percent()),
		fmt.Sprintf("%.4f", r.RNatural.Percent()),
		fmt.Sprintf("%+.1f", spread),
		"4",
	}
}

// RenderTable writes the fixed-width ASCII table (§3.2-5). Negative
// spreads (cheaper than benchmark) are highlighted when o.Color is set.
func RenderTable(w io.Writer, rows []scan.Result, o Options) error {
	data := make([][]string, 0, len(rows))
	for _, r := range rows {
		data = append(data, rowStrings(r, o))
	}
	widths := make([]int, len(Header))
	for i, h := range Header {
		widths[i] = len(h)
	}
	for _, row := range data {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i == len(cells)-1 {
				fmt.Fprintf(w, " %-*s", widths[i], cell) // last col left-ish
				continue
			}
			fmt.Fprintf(w, " %-*s ", widths[i], cell)
		}
		fmt.Fprintln(w)
	}
	// Header.
	for i, h := range Header {
		if i == len(Header)-1 {
			fmt.Fprintf(w, " %-*s", widths[i], strings.ToUpper(h))
			continue
		}
		fmt.Fprintf(w, " %-*s ", widths[i], strings.ToUpper(h))
	}
	fmt.Fprintln(w)
	for i, width := range widths {
		sep := strings.Repeat("-", width+2)
		if i < len(widths)-1 {
			sep += "+"
		}
		fmt.Fprint(w, sep)
	}
	fmt.Fprintln(w)
	// Data.
	for i, r := range rows {
		row := data[i]
		spread := r.SpreadBPS(o.Benchmark)
		if spread < 0 && o.Color {
			row[9] = ansiGreen + row[9] + ansiReset
		}
		writeRow(row)
	}
	return nil
}

// WriteCSV writes the rows in CSV form (used for export and csv format).
func WriteCSV(w io.Writer, rows []scan.Result, o Options) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(Header); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write(rowStrings(r, o)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// jsonRow mirrors Header with typed fields.
type jsonRow struct {
	Expiry      string  `json:"expiry"`
	D           int     `json:"D"`
	K1          int64   `json:"K1"`
	K2          int64   `json:"K2"`
	Width       int64   `json:"width"`
	CMid        float64 `json:"C_mid"`
	CNatural    float64 `json:"C_natural"`
	RMidPct     float64 `json:"r_mid_pct"`
	RNaturalPct float64 `json:"r_natural_pct"`
	SpreadBP    float64 `json:"spread_bp"`
	LegsOK      int     `json:"legs_ok"`
}

// WriteJSON writes the rows as a JSON array.
func WriteJSON(w io.Writer, rows []scan.Result, o Options) error {
	out := make([]jsonRow, 0, len(rows))
	for _, r := range rows {
		c := r.Combo
		out = append(out, jsonRow{
			Expiry:      c.Expiry.Format("2006-01-02"),
			D:           c.D,
			K1:          int64(c.K1),
			K2:          int64(c.K2),
			Width:       int64(c.Width()),
			CMid:        r.Credits.Mid.Float64(),
			CNatural:    r.Credits.Natural.Float64(),
			RMidPct:     r.RMid.Percent(),
			RNaturalPct: r.RNatural.Percent(),
			SpreadBP:    r.SpreadBPS(o.Benchmark),
			LegsOK:      4,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// uniqueTimes drops duplicate timestamps, preserving order.
func uniqueTimes(in []time.Time) []time.Time {
	seen := make(map[time.Time]struct{}, len(in))
	out := make([]time.Time, 0, len(in))
	for _, t := range in {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// RenderSummary prints run statistics, the best combo's full leg detail,
// data timestamps, and the staleness warning (§3.2-6, §4.5).
func RenderSummary(w io.Writer, stats scan.Stats, best *scan.Result, o Options) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	p("=== summary ===")
	p("combos scanned:   %d", stats.Total)
	p("combos valid:     %d", stats.Valid)
	if stats.Skipped > 0 {
		p("combos skipped:   %d (require_quotes=false)", stats.Skipped)
	}
	p("rejected:")
	for _, reason := range scan.ReasonOrder {
		if n := stats.Rejected[reason]; n > 0 {
			p("  %-20s %d", reason, n)
		}
	}
	p("benchmark rate:   %.2f%%", o.Benchmark*100)
	p("data timestamps:")
	for _, t := range uniqueTimes(stats.DataTimes) {
		p("  %s", t.UTC().Format(time.RFC3339))
	}
	if age := scan.MaxDataAge(stats.DataTimes, o.Now); age > scan.StaleThreshold {
		p("%sWARNING: market data is %s old (limit %s); prices may be stale%s",
			color(o.DiagColor, ansiYellow), age.Round(time.Minute), scan.StaleThreshold, color(o.DiagColor, ansiReset))
	} else {
		p("data age: %s (within %s)", age.Round(time.Second), scan.StaleThreshold)
	}

	if best == nil {
		p("no valid combos found")
		return
	}
	c := best.Combo
	p("")
	p("best combo: %s  K1=%d K2=%d (width %d)  D=%d",
		c.Expiry.Format("2006-01-02"), c.K1, c.K2, c.Width(), c.D)
	p("  C_mid=%s  C_natural=%s points  r_mid=%.4f%%  r_natural=%.4f%%  spread=%+.1fbp",
		best.Credits.Mid, best.Credits.Natural,
		best.RMid.Percent(), best.RNatural.Percent(), best.SpreadBPS(o.Benchmark))
	p("  legs (bid/ask/mid, points):")
	legs := []struct {
		name string
		q    box.Quote
	}{
		{"SELL K1 call", best.Legs.SellCallK1},
		{"BUY  K2 call", best.Legs.BuyCallK2},
		{"BUY  K1 put ", best.Legs.BuyPutK1},
		{"SELL K2 put ", best.Legs.SellPutK2},
	}
	for _, leg := range legs {
		p("    %s  bid=%s  ask=%s  mid=%s", leg.name, leg.q.Bid, leg.q.Ask, leg.q.Mid())
	}
}
