// Package watch turns the one-shot scanner into a polling monitor:
// it re-runs the scan on an interval, applies alert trigger rules with
// hysteresis and cooldown, and notifies through an alert.Alerter.
// Everything here is still strictly read-only.
package watch

import (
	"context"
	"io"
	"log"
	"math"
	"strconv"
	"time"

	"boxpilot/internal/alert"
	"boxpilot/internal/broker"
	"boxpilot/internal/scan"
)

// Rules are the trigger conditions that turn scan rows into alerts.
type Rules struct {
	// ThresholdBP: alert while spread_bp ≤ this. Negative values mean
	// "cheaper than benchmark"; 0 means "any cheaper-than-benchmark".
	ThresholdBP float64
	// ConfirmRounds: how many consecutive rounds the condition must hold
	// before the first alert of an episode (noise filter).
	ConfirmRounds int
	// HysteresisBP: within one episode, re-alert only after the spread
	// improves by at least this many basis points.
	HysteresisBP float64
	// Cooldown: minimum interval between any two alerts.
	Cooldown time.Duration
}

// Engine evaluates scan rounds and fires alerts. Create with NewEngine,
// then either call Round per poll tick or Run for the built-in loop.
type Engine struct {
	broker    broker.Broker
	params    scan.Params
	rules     Rules
	alerter   alert.Alerter
	log       *log.Logger
	now       func() time.Time
	benchmark float64

	state         map[string]*comboState // key: expiry|K1|K2
	lastAlertAt   time.Time
	staleNotified bool
}

type comboState struct {
	consecutive int     // consecutive rounds meeting the condition
	alerted     bool    // already alerted in the current episode
	lastRateBP  float64 // spread_bp at last alert (for hysteresis)
}

// NewEngine builds an engine. now may be nil (defaults to time.Now).
func NewEngine(b broker.Broker, p scan.Params, benchmark float64, rules Rules, alerter alert.Alerter, lg *log.Logger, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	if lg == nil {
		lg = log.New(io.Discard, "watch ", log.LstdFlags|log.LUTC)
	}
	return &Engine{
		broker:    b,
		params:    p,
		benchmark: benchmark,
		rules:     rules,
		alerter:   alerter,
		log:       lg,
		now:       now,
		state:     make(map[string]*comboState),
	}
}

// Round executes one scan-and-evaluate cycle. Transient scan errors are
// logged and returned; the caller decides whether to keep watching.
func (e *Engine) Round(ctx context.Context) error {
	rows, stats, err := scan.Execute(ctx, e.broker, e.params, e.now())
	if err != nil {
		return err
	}
	e.log.Printf("round: %d combos, %d valid", stats.Total, stats.Valid)

	// Stale data suspends opportunity alerts; notify once per episode.
	if age := scan.MaxDataAge(stats.DataTimes, e.now()); age > scan.StaleThreshold {
		if !e.staleNotified {
			e.staleNotified = true
			e.notifyStale(ctx, age, scan.StaleThreshold)
		}
		return nil
	}
	e.staleNotified = false

	seen := make(map[string]struct{}, len(rows))
	for i := range rows {
		r := rows[i]
		key := r.Combo.Expiry.Format("2006-01-02") + "|" +
			strconv.FormatInt(int64(r.Combo.K1), 10) + "|" + strconv.FormatInt(int64(r.Combo.K2), 10)
		seen[key] = struct{}{}
		e.evaluate(ctx, key, r)
	}
	// Drop state for combos that left the scan universe (e.g. the
	// underlying moved out of range).
	for key := range e.state {
		if _, ok := seen[key]; !ok {
			delete(e.state, key)
		}
	}
	return nil
}

// evaluate applies the trigger rules to one row.
func (e *Engine) evaluate(ctx context.Context, key string, r scan.Result) {
	st := e.state[key]
	if st == nil {
		st = &comboState{lastRateBP: math.Inf(1)}
		e.state[key] = st
	}
	spread := r.SpreadBPS(e.benchmark)

	if spread > e.rules.ThresholdBP {
		// Condition not met: reset the episode.
		st.consecutive = 0
		st.alerted = false
		return
	}
	st.consecutive++
	if st.consecutive < e.rules.ConfirmRounds {
		return
	}
	if st.alerted && st.lastRateBP-spread < e.rules.HysteresisBP {
		return // not improved enough to re-alert
	}
	if !e.lastAlertAt.IsZero() && e.now().Sub(e.lastAlertAt) < e.rules.Cooldown {
		return // global cooldown
	}
	if err := e.alerter.Notify(ctx, alert.Opportunity{Symbol: e.params.Symbol, Benchmark: e.benchmark, Result: r}); err != nil {
		e.log.Printf("alert delivery failed: %v", err)
		return // do not mark alerted; retry next round
	}
	st.alerted = true
	st.lastRateBP = spread
	e.lastAlertAt = e.now()
}

// notifyStale delivers the stale-data notice; failures are logged.
func (e *Engine) notifyStale(ctx context.Context, age, limit time.Duration) {
	if err := e.alerter.NotifyStale(ctx, age, limit); err != nil {
		e.log.Printf("stale notice delivery failed: %v", err)
	}
}

// Run polls forever until ctx is canceled: Round, wait the interval,
// repeat. Round errors are logged but never stop the watch.
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	for {
		if err := e.Round(ctx); err != nil && ctx.Err() == nil {
			e.log.Printf("round failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
