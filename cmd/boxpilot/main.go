// Command boxpilot is the BoxPilot CLI: a read-only scanner of SPX
// short box spread implied financing rates. Zero network access is
// needed when run against a fixture (broker: fixture).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boxpilot/internal/app"
	"boxpilot/internal/broker"
	"boxpilot/internal/config"
	"boxpilot/internal/logx"
	"boxpilot/internal/report"
	"boxpilot/internal/scan"
	"boxpilot/internal/watch"
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

	b, secrets, err := app.BuildBroker(cfg, *fixturePath)
	if err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
	}
	logger := logx.New(diag, "boxpilot ", secrets)

	params, err := app.BuildParams(cfg)
	if err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
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

	alerter, closer, err := app.BuildAlerters(cfg, ctx, logger, diag)
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
