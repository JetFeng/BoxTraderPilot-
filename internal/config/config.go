// Package config loads and validates the BoxPilot YAML configuration.
// No credentials may appear in the config file; Schwab credentials are
// read from the environment (see cmd/boxpilot).
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// DateFormat is the expiry date format used in the config file.
const DateFormat = "2006-01-02"

// Config is the root of boxpilot.yaml.
type Config struct {
	Symbol    string       `yaml:"symbol"`
	Broker    string       `yaml:"broker"` // "schwab" | "fixture"
	Fixture   FixtureCfg   `yaml:"fixture"`
	Scan      ScanCfg      `yaml:"scan"`
	Benchmark BenchmarkCfg `yaml:"benchmark"`
	Filters   FiltersCfg   `yaml:"filters"`
	Output    OutputCfg    `yaml:"output"`
	Watch     WatchCfg     `yaml:"watch"`
}

// FixtureCfg selects the offline fixture input.
type FixtureCfg struct {
	Path string `yaml:"path"` // testdata/spx_chain_fixture.json
}

// ScanCfg describes what to scan.
type ScanCfg struct {
	Expiries        []string `yaml:"expiries"` // YYYY-MM-DD
	Widths          []int64  `yaml:"widths"`   // K2 − K1, in points
	K1RangePct      float64  `yaml:"k1_range_pct"`
	StrikeStep      int64    `yaml:"strike_step"`
	MinDaysToExpiry int      `yaml:"min_days_to_expiry"`
}

// BenchmarkCfg is the user's alternative financing rate.
type BenchmarkCfg struct {
	Rate float64 `yaml:"rate"` // plain fraction, (0, 0.5]
}

// FiltersCfg are the quote-quality filters.
type FiltersCfg struct {
	MaxLegSpreadPct float64 `yaml:"max_leg_spread_pct"`
	MinBid          float64 `yaml:"min_bid"` // points
	RequireQuotes   bool    `yaml:"require_quotes"`
}

// OutputCfg controls presentation.
type OutputCfg struct {
	Format  string `yaml:"format"` // table | csv | json
	CSVPath string `yaml:"csv_path"`
	Top     int    `yaml:"top"` // show top N rows; 0 = all
}

// Watch defaults applied when a field is absent from the config.
const (
	defaultThresholdBP   = -50.0
	defaultConfirmRounds = 1
	defaultHysteresisBP  = 25.0
)

// WatchCfg configures watch mode (activated with -watch on the CLI).
//
// ThresholdBP, ConfirmRounds and HysteresisBP are pointers so that an
// explicitly configured zero can be told apart from an absent field.
// Zero is a meaningful value for all three (alert on any spread below
// the benchmark / no confirmation round / re-alert on any improvement),
// so it must not be silently replaced by a default.
type WatchCfg struct {
	Interval      string      `yaml:"interval"`       // e.g. "30s"
	ThresholdBP   *float64    `yaml:"threshold_bp"`   // alert while spread_bp ≤ this
	ConfirmRounds *int        `yaml:"confirm_rounds"` // rounds the condition must hold
	HysteresisBP  *float64    `yaml:"hysteresis_bp"`  // re-alert improvement step
	Cooldown      string      `yaml:"cooldown"`       // min interval between alerts
	Channels      []string    `yaml:"channels"`       // whatsapp | log
	WhatsApp      WhatsAppCfg `yaml:"whatsapp"`
}

// WhatsAppCfg configures the whatsapp channel.
type WhatsAppCfg struct {
	Recipient string `yaml:"recipient"` // E.164 digits; WHATSAPP_RECIPIENT overrides
	DBPath    string `yaml:"db_path"`   // session store; contains keys — never commit
}

// WatchOpts is the parsed, validated form of WatchCfg.
type WatchOpts struct {
	Interval      time.Duration
	Cooldown      time.Duration
	ThresholdBP   float64
	ConfirmRounds int
	HysteresisBP  float64
}

// ParseWatch parses the watch section, applying defaults for unset
// fields. The section may be entirely absent (zero value) — then the
// defaults apply. A field explicitly set to zero is honoured as-is.
func (c *Config) ParseWatch() (WatchOpts, error) {
	var o WatchOpts
	w := c.Watch

	o.ThresholdBP = defaultThresholdBP
	if w.ThresholdBP != nil {
		o.ThresholdBP = *w.ThresholdBP
	}
	o.ConfirmRounds = defaultConfirmRounds
	if w.ConfirmRounds != nil {
		o.ConfirmRounds = *w.ConfirmRounds
	}
	o.HysteresisBP = defaultHysteresisBP
	if w.HysteresisBP != nil {
		o.HysteresisBP = *w.HysteresisBP
	}
	if o.ConfirmRounds < 0 {
		return o, fmt.Errorf("watch.confirm_rounds: must be >= 0, got %d", o.ConfirmRounds)
	}
	if o.HysteresisBP < 0 {
		return o, fmt.Errorf("watch.hysteresis_bp: must be >= 0, got %v", o.HysteresisBP)
	}

	interval := w.Interval
	if interval == "" {
		interval = "60s"
	}
	d, err := time.ParseDuration(interval)
	if err != nil || d < 5*time.Second || d > time.Hour {
		return o, fmt.Errorf("watch.interval: must be a duration in [5s, 1h], got %q", w.Interval)
	}
	o.Interval = d
	cooldown := w.Cooldown
	if cooldown == "" {
		cooldown = "1m"
	}
	cd, err := time.ParseDuration(cooldown)
	if err != nil || cd < 0 || cd > 24*time.Hour {
		return o, fmt.Errorf("watch.cooldown: must be a duration in [0, 24h], got %q", w.Cooldown)
	}
	o.Cooldown = cd
	if len(w.Channels) == 0 {
		w.Channels = []string{"log"}
	}
	return o, nil
}

// WatchChannels returns the configured channel names (default: log).
func (c *Config) WatchChannels() []string {
	if len(c.Watch.Channels) == 0 {
		return []string{"log"}
	}
	return c.Watch.Channels
}

// ValidateWatch checks watch-mode settings; call only when watch is on.
func (c *Config) ValidateWatch() error {
	if _, err := c.ParseWatch(); err != nil {
		return err
	}
	for _, ch := range c.WatchChannels() {
		switch ch {
		case "log", "whatsapp":
		default:
			return fmt.Errorf("watch.channels: unknown channel %q (want whatsapp|log)", ch)
		}
	}
	return nil
}

// Load reads and parses the YAML config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &c, nil
}

// Expiries parses the configured expiry dates.
func (c *Config) Expiries() ([]time.Time, error) {
	out := make([]time.Time, 0, len(c.Scan.Expiries))
	for _, s := range c.Scan.Expiries {
		d, err := time.ParseInLocation(DateFormat, s, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("scan.expiries: invalid date %q (want %s)", s, DateFormat)
		}
		out = append(out, d)
	}
	return out, nil
}

// Validate checks the config for internal consistency. now is the scan
// time; expiry dates must not lie in the past.
func (c *Config) Validate(now time.Time) error {
	if c.Symbol == "" {
		return fmt.Errorf("symbol is required")
	}
	switch c.Broker {
	case "schwab", "fixture":
	default:
		return fmt.Errorf("broker must be %q or %q, got %q", "schwab", "fixture", c.Broker)
	}
	if c.Broker == "fixture" && c.Fixture.Path == "" {
		return fmt.Errorf("fixture.path is required when broker=fixture")
	}
	exps, err := c.Expiries()
	if err != nil {
		return err
	}
	if len(exps) == 0 {
		return fmt.Errorf("scan.expiries must not be empty")
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for _, e := range exps {
		if e.Before(today) {
			return fmt.Errorf("scan.expiries: %s is in the past", e.Format(DateFormat))
		}
	}
	if c.Scan.StrikeStep <= 0 {
		return fmt.Errorf("scan.strike_step must be > 0")
	}
	if len(c.Scan.Widths) == 0 {
		return fmt.Errorf("scan.widths must not be empty")
	}
	for _, w := range c.Scan.Widths {
		if w <= 0 {
			return fmt.Errorf("scan.widths: width must be > 0, got %d", w)
		}
		if w%c.Scan.StrikeStep != 0 {
			return fmt.Errorf("scan.widths: width %d must be a multiple of strike_step %d", w, c.Scan.StrikeStep)
		}
	}
	if c.Scan.K1RangePct <= 0 || c.Scan.K1RangePct > 0.5 {
		return fmt.Errorf("scan.k1_range_pct must be in (0, 0.5], got %v", c.Scan.K1RangePct)
	}
	if c.Scan.StrikeStep <= 0 {
		return fmt.Errorf("scan.strike_step must be > 0")
	}
	if c.Scan.MinDaysToExpiry < 0 {
		return fmt.Errorf("scan.min_days_to_expiry must be >= 0")
	}
	if c.Benchmark.Rate <= 0 || c.Benchmark.Rate > 0.5 {
		return fmt.Errorf("benchmark.rate must be in (0, 0.5], got %v", c.Benchmark.Rate)
	}
	if c.Filters.MaxLegSpreadPct <= 0 || c.Filters.MaxLegSpreadPct > 1 {
		return fmt.Errorf("filters.max_leg_spread_pct must be in (0, 1], got %v", c.Filters.MaxLegSpreadPct)
	}
	if c.Filters.MinBid <= 0 {
		return fmt.Errorf("filters.min_bid must be > 0, got %v", c.Filters.MinBid)
	}
	switch c.Output.Format {
	case "table", "csv", "json":
	default:
		return fmt.Errorf("output.format must be table|csv|json, got %q", c.Output.Format)
	}
	if c.Output.Top < 0 {
		return fmt.Errorf("output.top must be >= 0")
	}
	return nil
}
