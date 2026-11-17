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
	"sync"
	"time"

	"boxpilot/internal/alert"
	"boxpilot/internal/box"
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

// Opportunity is one combo that currently meets the trigger condition,
// together with its episode state.
type Opportunity struct {
	Key         string
	Expiry      time.Time
	K1          box.Strike
	K2          box.Strike
	Width       box.Strike
	D           int
	SpreadBP    float64
	RNaturalPct float64
	// Consecutive is how many rounds in a row the condition has held.
	Consecutive int
	// Alerted reports whether this episode has already fired.
	Alerted bool
}

// Snapshot is a point-in-time view of the engine. It is safe to read
// while the engine is polling, and is what UIs render from.
type Snapshot struct {
	// Rows holds the last scan's surviving combos, sorted by r_natural
	// ascending.
	Rows []scan.Result
	// Stats summarizes the last scan.
	Stats scan.Stats
	// Opportunities are the rows currently meeting the trigger
	// condition, with their episode state.
	Opportunities []Opportunity
	// LastRoundAt is when the last scan finished.
	LastRoundAt time.Time
	// LastError is the last scan error, if any.
	LastError string
	// StaleData reports that the market data is too old to act on.
	StaleData bool
	// AlertsSent counts the opportunity alerts delivered so far.
	AlertsSent int
	// Rules and Benchmark are the trigger settings in force.
	Rules     Rules
	Benchmark float64
}

// Engine evaluates scan rounds and fires alerts. Create with NewEngine,
// then either call Round per poll tick or Run for the built-in loop.
type Engine struct {
	broker  broker.Broker
	alerter alert.Alerter
	log     *log.Logger
	now     func() time.Time

	// mu guards every mutable field below. The engine may be polled in
	// one goroutine while a UI reads snapshots in others.
	mu sync.RWMutex

	params    scan.Params
	rules     Rules
	benchmark float64

	state         map[string]*comboState // key: expiry|K1|K2
	lastAlertAt   time.Time
	staleNotified bool
	alertsSent    int

	lastRows    []scan.Result
	lastStats   scan.Stats
	lastErr     error
	lastRoundAt time.Time
	staleData   bool
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
		alerter:   alerter,
		log:       lg,
		now:       now,
		params:    p,
		benchmark: benchmark,
		rules:     rules,
		state:     make(map[string]*comboState),
	}
}

// UpdateParams swaps the scan parameters (e.g. after a configuration
// change). Episode state is preserved so an in-flight opportunity does
// not re-trigger.
func (e *Engine) UpdateParams(p scan.Params) {
	e.mu.Lock()
	e.params = p
	e.mu.Unlock()
}

// UpdateRules swaps the trigger rules and the benchmark rate.
func (e *Engine) UpdateRules(rules Rules, benchmark float64) {
	e.mu.Lock()
	e.rules = rules
	e.benchmark = benchmark
	e.mu.Unlock()
}

// Rules returns the trigger settings currently in force.
func (e *Engine) Rules() Rules {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.rules
}

// Snapshot returns a point-in-time view of the engine.
func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	s := Snapshot{
		Rows:        append([]scan.Result(nil), e.lastRows...),
		Stats:       e.lastStats,
		LastRoundAt: e.lastRoundAt,
		StaleData:   e.staleData,
		AlertsSent:  e.alertsSent,
		Rules:       e.rules,
		Benchmark:   e.benchmark,
	}
	if e.lastErr != nil {
		s.LastError = e.lastErr.Error()
	}
	for _, r := range e.lastRows {
		spread := r.SpreadBPS(e.benchmark)
		if spread > e.rules.ThresholdBP {
			continue
		}
		op := Opportunity{
			Key:         comboKey(r.Combo),
			Expiry:      r.Combo.Expiry,
			K1:          r.Combo.K1,
			K2:          r.Combo.K2,
			Width:       r.Combo.Width(),
			D:           r.Combo.D,
			SpreadBP:    spread,
			RNaturalPct: r.RNatural.Percent(),
		}
		if st := e.state[op.Key]; st != nil {
			op.Consecutive = st.consecutive
			op.Alerted = st.alerted
		}
		s.Opportunities = append(s.Opportunities, op)
	}
	return s
}

// comboKey identifies a combo across rounds.
func comboKey(c box.Combo) string {
	return c.Expiry.Format("2006-01-02") + "|" +
		strconv.FormatInt(int64(c.K1), 10) + "|" + strconv.FormatInt(int64(c.K2), 10)
}

// Round executes one scan-and-evaluate cycle. Transient scan errors are
// logged and returned; the caller decides whether to keep watching.
func (e *Engine) Round(ctx context.Context) error {
	e.mu.RLock()
	params := e.params
	e.mu.RUnlock()

	rows, stats, err := scan.Execute(ctx, e.broker, params, e.now())
	now := e.now()
	if err != nil {
		e.setSnapshot(nil, stats, err, now, false)
		return err
	}
	e.log.Printf("round: %d combos, %d valid", stats.Total, stats.Valid)

	// Stale data suspends opportunity alerts; notify once per episode.
	age := scan.MaxDataAge(stats.DataTimes, now)
	if age > scan.StaleThreshold {
		e.setSnapshot(rows, stats, nil, now, true)
		if e.markStale() {
			e.notifyStale(ctx, age, scan.StaleThreshold)
		}
		return nil
	}
	e.clearStale()
	e.setSnapshot(rows, stats, nil, now, false)

	seen := make(map[string]struct{}, len(rows))
	for i := range rows {
		r := rows[i]
		key := comboKey(r.Combo)
		seen[key] = struct{}{}
		e.evaluate(ctx, key, r)
	}
	// Drop state for combos that left the scan universe (e.g. the
	// underlying moved out of range).
	e.mu.Lock()
	for key := range e.state {
		if _, ok := seen[key]; !ok {
			delete(e.state, key)
		}
	}
	e.mu.Unlock()
	return nil
}

// setSnapshot records the outcome of a round.
func (e *Engine) setSnapshot(rows []scan.Result, stats scan.Stats, err error, at time.Time, stale bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastRows = rows
	e.lastStats = stats
	e.lastErr = err
	e.lastRoundAt = at
	e.staleData = stale
}

// markStale reports whether this is the first stale round of the
// current episode (the notice is sent once).
func (e *Engine) markStale() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.staleNotified {
		return false
	}
	e.staleNotified = true
	return true
}

// clearStale resets the stale-notice latch once data is fresh again.
func (e *Engine) clearStale() {
	e.mu.Lock()
	e.staleNotified = false
	e.mu.Unlock()
}

// evaluate applies the trigger rules to one row.
func (e *Engine) evaluate(ctx context.Context, key string, r scan.Result) {
	e.mu.Lock()
	st := e.state[key]
	if st == nil {
		st = &comboState{lastRateBP: math.Inf(1)}
		e.state[key] = st
	}
	spread := r.SpreadBPS(e.benchmark)
	threshold := e.rules.ThresholdBP
	confirmRounds := e.rules.ConfirmRounds
	hysteresis := e.rules.HysteresisBP
	cooldown := e.rules.Cooldown
	lastAlertAt := e.lastAlertAt
	symbol := e.params.Symbol
	benchmark := e.benchmark

	if spread > threshold {
		// Condition not met: reset the episode.
		st.consecutive = 0
		st.alerted = false
		e.mu.Unlock()
		return
	}
	st.consecutive++
	if st.consecutive < confirmRounds {
		e.mu.Unlock()
		return
	}
	if st.alerted && st.lastRateBP-spread < hysteresis {
		e.mu.Unlock()
		return // not improved enough to re-alert
	}
	if !lastAlertAt.IsZero() && e.now().Sub(lastAlertAt) < cooldown {
		e.mu.Unlock()
		return // global cooldown
	}
	e.mu.Unlock()

	// Deliver outside the lock: a slow channel must not block readers
	// of the snapshot.
	if err := e.alerter.Notify(ctx, alert.Opportunity{Symbol: symbol, Benchmark: benchmark, Result: r}); err != nil {
		e.log.Printf("alert delivery failed: %v", err)
		return // do not mark alerted; retry next round
	}
	e.mu.Lock()
	st.alerted = true
	st.lastRateBP = spread
	e.lastAlertAt = e.now()
	e.alertsSent++
	e.mu.Unlock()
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
