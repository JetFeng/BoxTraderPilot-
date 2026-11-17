// Package app holds the wiring shared by the two front ends: the CLI
// (cmd/boxpilot) and the web server (cmd/boxpilotd). Keeping the
// broker/parameter/alerter assembly in one place guarantees both
// binaries resolve credentials, scan scope and alert channels the
// same way.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"boxpilot/internal/alert"
	"boxpilot/internal/box"
	"boxpilot/internal/broker"
	"boxpilot/internal/broker/fixture"
	"boxpilot/internal/broker/schwab"
	"boxpilot/internal/config"
	"boxpilot/internal/logx"
	"boxpilot/internal/scan"
)

// Environment variable names for Schwab credentials (§4.2: nothing
// hardcoded).
const (
	EnvClientID     = "SCHWAB_CLIENT_ID"
	EnvClientSecret = "SCHWAB_APP_SECRET"
	EnvRefreshToken = "SCHWAB_REFRESH_TOKEN"
)

// BuildBroker constructs the read-only data source. The fixturePath
// argument overrides the config so acceptance runs stay fully offline.
// The returned secrets are the values that must be masked in logs.
func BuildBroker(cfg *config.Config, fixturePath string) (broker.Broker, []string, error) {
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

	id, secret, refresh := os.Getenv(EnvClientID), os.Getenv(EnvClientSecret), os.Getenv(EnvRefreshToken)
	var missing []string
	if id == "" {
		missing = append(missing, EnvClientID)
	}
	if secret == "" {
		missing = append(missing, EnvClientSecret)
	}
	if refresh == "" {
		missing = append(missing, EnvRefreshToken)
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("missing environment variables: %s", joinComma(missing))
	}

	// Schwab rotates the refresh token on every refresh; persist it so
	// a restart does not silently lose access.
	storePath := schwab.DefaultTokenStorePath()
	if storePath == "" {
		// Persistence is best-effort: without it the process still
		// works, but a rotation would be lost on restart.
		fmt.Fprintln(os.Stderr, "warning: no writable path for the Schwab token store; "+
			"a rotated refresh token will be lost on restart")
	}
	c, err := schwab.New(schwab.Config{
		ClientID:       id,
		ClientSecret:   secret,
		RefreshToken:   refresh,
		TokenStorePath: storePath,
	})
	if err != nil {
		return nil, nil, err
	}
	return c, []string{id, secret, refresh}, nil
}

// BuildParams derives the scan parameters from the configuration.
func BuildParams(cfg *config.Config) (scan.Params, error) {
	exps, err := cfg.Expiries()
	if err != nil {
		return scan.Params{}, err
	}
	return scan.Params{
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
	}, nil
}

// BuildAlerters assembles the alert channels. The returned closer
// releases channel resources (WhatsApp session).
func BuildAlerters(cfg *config.Config, ctx context.Context, logger *logx.Logger, diag io.Writer) (alert.Alerter, func() error, error) {
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
