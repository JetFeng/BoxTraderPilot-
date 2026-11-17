// Command boxpilotd serves the BoxPilot scanner as a local web
// application: a status bar, a sortable results table, an opportunity
// board and a configuration panel.
//
// The service binds to loopback by default because it holds Schwab
// credentials and does not (yet) authenticate its callers. Exposing it
// on a routable address is possible but must be paired with
// authentication — see docs/deploy.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"boxpilot/internal/logx"
	"boxpilot/internal/web"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run starts the server and returns the process exit code. Everything
// the server emits is diagnostic, so it all goes to diag.
func run(args []string, diag io.Writer) int {
	fs := flag.NewFlagSet("boxpilotd", flag.ContinueOnError)
	fs.SetOutput(diag)
	cfgPath := fs.String("config", "config.yaml", "path to the YAML config file")
	fixturePath := fs.String("fixture", "", "override: run offline against this fixture JSON file")
	addr := fs.String("addr", envOr("BOXPILOT_ADDR", web.DefaultAddr), "listen address (loopback by default)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := logx.New(diag, "boxpilotd ", nil)
	srv, err := web.New(web.Options{
		Addr:        *addr,
		ConfigPath:  *cfgPath,
		FixturePath: *fixturePath,
		Logger:      logger,
		Diag:        diag,
	})
	if err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(diag, "error: %v\n", err)
		return 1
	}
	return 0
}

// envOr returns the environment variable's value, or def when unset.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
