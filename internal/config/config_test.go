package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const validYAML = `
symbol: SPX
broker: fixture
fixture:
  path: testdata/spx_chain_fixture.json
scan:
  expiries: ["2026-12-18", "2027-01-15"]
  widths: [250, 500]
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
  format: table
  csv_path: ""
  top: 30
`

func nowForTest() time.Time {
	return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
}

func loadValid(t *testing.T) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "boxpilot.yaml")
	if err := os.WriteFile(p, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(nowForTest()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return c
}

func TestLoadValid(t *testing.T) {
	c := loadValid(t)
	if c.Symbol != "SPX" || c.Broker != "fixture" {
		t.Errorf("symbol/broker = %q/%q", c.Symbol, c.Broker)
	}
	exps, err := c.Expiries()
	if err != nil || len(exps) != 2 {
		t.Fatalf("Expiries: %v %v", exps, err)
	}
	if exps[0].Format(DateFormat) != "2026-12-18" {
		t.Errorf("expiries[0] = %s", exps[0])
	}
}

func mutate(t *testing.T, mod func(*Config)) *Config {
	t.Helper()
	c := loadValid(t)
	mod(c)
	return c
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
	}{
		{"empty symbol", func(c *Config) { c.Symbol = "" }},
		{"bad broker", func(c *Config) { c.Broker = "ib" }},
		{"fixture missing path", func(c *Config) { c.Fixture.Path = "" }},
		{"no expiries", func(c *Config) { c.Scan.Expiries = nil }},
		{"bad date", func(c *Config) { c.Scan.Expiries = []string{"12/18/2026"} }},
		{"past date", func(c *Config) { c.Scan.Expiries = []string{"2026-11-07"} }},
		{"no widths", func(c *Config) { c.Scan.Widths = nil }},
		{"negative width", func(c *Config) { c.Scan.Widths = []int64{-250} }},
		{"width not step multiple", func(c *Config) { c.Scan.Widths = []int64{253} }},
		{"zero range pct", func(c *Config) { c.Scan.K1RangePct = 0 }},
		{"range pct too big", func(c *Config) { c.Scan.K1RangePct = 0.6 }},
		{"zero step", func(c *Config) { c.Scan.StrikeStep = 0 }},
		{"negative min dte", func(c *Config) { c.Scan.MinDaysToExpiry = -1 }},
		{"benchmark zero", func(c *Config) { c.Benchmark.Rate = 0 }},
		{"benchmark above 50pct", func(c *Config) { c.Benchmark.Rate = 0.51 }},
		{"benchmark negative", func(c *Config) { c.Benchmark.Rate = -0.01 }},
		{"spread pct zero", func(c *Config) { c.Filters.MaxLegSpreadPct = 0 }},
		{"spread pct above 100pct", func(c *Config) { c.Filters.MaxLegSpreadPct = 1.5 }},
		{"min bid zero", func(c *Config) { c.Filters.MinBid = 0 }},
		{"bad format", func(c *Config) { c.Output.Format = "xml" }},
		{"negative top", func(c *Config) { c.Output.Top = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mutate(t, tc.mod)
			if err := c.Validate(nowForTest()); err == nil {
				t.Errorf("Validate accepted invalid config")
			}
		})
	}
}

func TestValidateBoundaryBenchmark(t *testing.T) {
	for _, r := range []float64{0.0001, 0.065, 0.5} {
		c := mutate(t, func(c *Config) { c.Benchmark.Rate = r })
		if err := c.Validate(nowForTest()); err != nil {
			t.Errorf("benchmark %v rejected: %v", r, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParseWatchDefaults(t *testing.T) {
	c := loadValid(t)
	o, err := c.ParseWatch()
	if err != nil {
		t.Fatal(err)
	}
	if o.Interval != 60*time.Second || o.Cooldown != time.Minute ||
		o.ConfirmRounds != 1 || o.ThresholdBP != -50 || o.HysteresisBP != 25 {
		t.Errorf("defaults = %+v", o)
	}
	if got := c.WatchChannels(); len(got) != 1 || got[0] != "log" {
		t.Errorf("default channels = %v", got)
	}
}

func TestParseWatchExplicit(t *testing.T) {
	c := mutate(t, func(c *Config) {
		c.Watch.Interval = "30s"
		c.Watch.Cooldown = "10m"
		c.Watch.ThresholdBP = -100
		c.Watch.ConfirmRounds = 3
		c.Watch.HysteresisBP = 10
		c.Watch.Channels = []string{"log", "whatsapp"}
		c.Watch.WhatsApp.DBPath = "wa.db"
		c.Watch.WhatsApp.Recipient = "14155550123"
	})
	if err := c.ValidateWatch(); err != nil {
		t.Fatal(err)
	}
	o, err := c.ParseWatch()
	if err != nil {
		t.Fatal(err)
	}
	if o.Interval != 30*time.Second || o.Cooldown != 10*time.Minute ||
		o.ConfirmRounds != 3 || o.ThresholdBP != -100 || o.HysteresisBP != 10 {
		t.Errorf("opts = %+v", o)
	}
}

func TestValidateWatchRejects(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
	}{
		{"bad interval", func(c *Config) { c.Watch.Interval = "2s" }},
		{"interval too long", func(c *Config) { c.Watch.Interval = "2h" }},
		{"bad cooldown", func(c *Config) { c.Watch.Cooldown = "soon" }},
		{"negative cooldown", func(c *Config) { c.Watch.Cooldown = "-5m" }},
		{"unknown channel", func(c *Config) { c.Watch.Channels = []string{"pager"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mutate(t, tc.mod)
			if err := c.ValidateWatch(); err == nil {
				t.Errorf("ValidateWatch accepted invalid config")
			}
		})
	}
}
