// Command boxpilot is the BoxPilot CLI: a read-only scanner of SPX
// short box spread implied financing rates. Zero network access is
// needed when run against a fixture (broker: fixture).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boxpilot/internal/alert"
	"boxpilot/internal/box"
	"boxpilot/internal/broker"
	"boxpilot/internal/broker/fixture"
	"boxpilot/internal/broker/schwab"
	"boxpilot/internal/config"
	"boxpilot/internal/logx"
	"boxpilot/internal/report"
	"boxpilot/internal/scan"
	"boxpilot/internal/watch"
)

// env names for Schwab credentials (§4.2: nothing hardcoded).
const (
	envClientID     = "SCHWAB_CLIENT_ID"
	envClientSecret = "SCHWAB_APP_SECRET"
	envRefreshToken = "SCHWAB_REFRESH_TOKEN"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now()))
}

// run executes the scan and writes the report to out; it returns the
// process exit code. now is injectable for tests.
//
// Output contract: out carries ONLY the requested data format (table,
// csv, json) so it can be piped into other programs. diag carries every
// human-readable diagnostic — errors, the run summary and the scan log —
// so machine-readable output is never polluted. The table format is the
// one exception: it is for humans, so its summary stays on stdout.
func run(args []string, out, diag io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("boxpilot", flag.ContinueOnError)
	fs.SetOutput(diag)
	cfgPath := fs.String("config", "config.yaml", "path to the YAML config file")
	fixturePath := fs.String("fixture", "", "override: run offline against this fixture JSON file")
	watchMode := fs.Bool("watch", false, "keep scanning on an interval and push alerts (see watch: config)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
	}
	if err := cfg.Validate(now); err != nil {
		fmt.Fprintf(diag, "error: invalid config: %v\n", err)
		return 1
	}

	b, secrets, err := buildBroker(cfg, *fixturePath)
	if err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
	}
	logger := logx.New(diag, "boxpilot ", secrets)

	exps, err := cfg.Expiries()
	if err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
	}
	params := scan.Params{
		Symbol:          cfg.Symbol,
		Expiries:        exps,
		Widths:          cfg.Scan.Widths,
		K1RangePct:      cfg.Scan.K1RangePct,
		StrikeStep:      cfg.Scan.StrikeStep,
		MinDaysToExpiry: cfg.Scan.MinDaysToExpiry,
		RequireQuotes:   cfg.Filters.RequireQuotes,
		Filters: scan.Filters{
			MaxLegSpreadPct: cfg.Filters.MaxLegSpreadPct,
			MinSellBid:      box.PriceFromPoints(cfg.Filters.MinBid),
		},
	}

	if *watchMode {
		return runWatch(cfg, b, params, logger, diag)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rows, stats, err := scan.Execute(ctx, b, params, now)
	if err != nil {
		fmt.Fprintf(diag, "error: scan failed: %v\n", err)
		return 1
	}
	logger.Printf("scanned %d combos, %d valid", stats.Total, stats.Valid)

	if cfg.Output.Top > 0 && len(rows) > cfg.Output.Top {
		rows = rows[:cfg.Output.Top]
	}
	opts := report.Options{
		Benchmark: cfg.Benchmark.Rate,
		Color:     stdoutIsTerminal(),
		DiagColor: stderrIsTerminal(),
		Now:       now,
	}
	switch cfg.Output.Format {
	case "csv":
		if err := report.WriteCSV(out, rows, opts); err != nil {
			fmt.Fprintf(diag, "error: csv render: %v\n", err)
			return 1
		}
	case "json":
		if err := report.WriteJSON(out, rows, opts); err != nil {
			fmt.Fprintf(diag, "error: json render: %v\n", err)
			return 1
		}
	default:
		if err := report.RenderTable(out, rows, opts); err != nil {
			fmt.Fprintf(diag, "error: table render: %v\n", err)
			return 1
		}
	}

	if cfg.Output.CSVPath != "" && cfg.Output.Format != "csv" {
		f, err := os.Create(cfg.Output.CSVPath)
		if err != nil {
			fmt.Fprintf(diag, "error: csv export: %v\n", err)
			return 1
		}
		if err := report.WriteCSV(f, rows, opts); err != nil {
			f.Close()
			fmt.Fprintf(diag, "error: csv export: %v\n", err)
			return 1
		}
		f.Close()
		fmt.Fprintf(diag, "csv exported to %s\n", cfg.Output.CSVPath)
	}

	var best *scan.Result
	if len(rows) > 0 {
		best = &rows[0]
	}
	// The summary is a diagnostic, not data. For the machine-readable
	// formats (csv/json) stdout must hold nothing but the payload, so
	// the summary is diverted to diag; the human-facing table keeps it
	// on stdout.
	summaryOut := out
	if cfg.Output.Format != "table" {
		summaryOut = diag
	}
	report.RenderSummary(summaryOut, stats, best, opts)
	return 0
}

// buildBroker constructs the read-only data source. The -fixture flag
// overrides the config so acceptance runs stay fully offline.
func buildBroker(cfg *config.Config, fixturePath string) (broker.Broker, []string, error) {
	if fixturePath != "" || cfg.Broker == "fixture" {
		path := fixturePath
		if path == "" {
			path = cfg.Fixture.Path
		}
		f, err := fixture.Load(path)
		if err != nil {
			return nil, nil, err
		}
		return fixture.NewBroker(f), nil, nil
	}

	id, secret, refresh := os.Getenv(envClientID), os.Getenv(envClientSecret), os.Getenv(envRefreshToken)
	var missing []string
	if id == "" {
		missing = append(missing, envClientID)
	}
	if secret == "" {
		missing = append(missing, envClientSecret)
	}
	if refresh == "" {
		missing = append(missing, envRefreshToken)
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("missing environment variables: %s", joinComma(missing))
	}
	c, err := schwab.New(schwab.Config{
		ClientID:     id,
		ClientSecret: secret,
		RefreshToken: refresh,
		// Schwab rotates the refresh token on every refresh; persist it
		// so a restart does not silently lose access.
		TokenStorePath: schwab.DefaultTokenStorePath(),
	})
	if err != nil {
		return nil, nil, err
	}
	return c, []string{id, secret, refresh}, nil
}

// runWatch is the polling alert loop. It runs until SIGINT/SIGTERM.
func runWatch(cfg *config.Config, b broker.Broker, params scan.Params, logger *logx.Logger, diag io.Writer) int {
	if err := cfg.ValidateWatch(); err != nil {
		fmt.Fprintf(diag, "error: invalid watch config: %v\n", err)
		return 1
	}
	opts, err := cfg.ParseWatch()
	if err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	alerter, closer, err := buildAlerters(cfg, ctx, logger, diag)
	if err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
	}
	if closer != nil {
		defer closer()
	}

	rules := watch.Rules{
		ThresholdBP:   opts.ThresholdBP,
		ConfirmRounds: opts.ConfirmRounds,
		HysteresisBP:  opts.HysteresisBP,
		Cooldown:      opts.Cooldown,
	}
	engine := watch.NewEngine(b, params, cfg.Benchmark.Rate, rules, alerter,
		log.New(diag, "watch ", log.LstdFlags|log.LUTC), nil)

	fmt.Fprintf(diag, "watch mode: every %s, threshold %+.0fbp, channels: %s (Ctrl+C to stop)\n",
		opts.Interval, opts.ThresholdBP, joinComma(cfg.WatchChannels()))
	if err := engine.Run(ctx, opts.Interval); err != nil {
		fmt.Fprintf(diag, "error: watch stopped: %v\n", err)
		return 1
	}
	return 0
}

// buildAlerters assembles the alert channels. The returned closer
// releases channel resources (WhatsApp session).
func buildAlerters(cfg *config.Config, ctx context.Context, logger *logx.Logger, diag io.Writer) (alert.Alerter, func() error, error) {
	var channels []alert.Alerter
	var closers []func() error
	for _, name := range cfg.WatchChannels() {
		switch name {
		case "log":
			channels = append(channels, alert.NewLogAlerter(diag))
		case "whatsapp":
			wa, err := alert.NewWhatsApp(alert.WhatsAppConfig{
				DBPath:    orDefault(cfg.Watch.WhatsApp.DBPath, "whatsapp_session.db"),
				Recipient: orDefault(os.Getenv("WHATSAPP_RECIPIENT"), cfg.Watch.WhatsApp.Recipient),
			})
			if err != nil {
				if errors.Is(err, alert.ErrNotBuilt) {
					return nil, nil, fmt.Errorf("whatsapp channel selected, but this binary was built without it; rebuild with: go build -tags whatsapp ./...")
				}
				return nil, nil, err
			}
			if err := wa.Connect(ctx); err != nil {
				return nil, nil, fmt.Errorf("whatsapp connect: %w", err)
			}
			channels = append(channels, wa)
			closers = append(closers, wa.Close)
			logger.Printf("whatsapp channel connected")
		}
	}
	multi := alert.NewMulti(channels...)
	return multi, func() error {
		var errs []error
		for _, c := range closers {
			if err := c(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// stdoutIsTerminal enables ANSI highlights only on a real terminal.
func stdoutIsTerminal() bool { return isTerminal(os.Stdout) }

// stderrIsTerminal does the same for the diagnostics stream.
func stderrIsTerminal() bool { return isTerminal(os.Stderr) }

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
