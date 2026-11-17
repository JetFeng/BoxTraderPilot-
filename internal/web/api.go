package web

import (
	"encoding/json"
	"net/http"
	"time"

	"boxpilot/internal/app"
	"boxpilot/internal/box"
	"boxpilot/internal/config"
	"boxpilot/internal/scan"
	"boxpilot/internal/watch"
)

// --- DTOs -------------------------------------------------------------
//
// The API speaks its own types rather than serialising scan.Result
// directly: the internal representation (integer prices, exact
// rationals) is not something a browser should depend on.

type legDTO struct {
	Side   string  `json:"side"`
	Strike int64   `json:"strike"`
	Kind   string  `json:"kind"`
	Bid    float64 `json:"bid"`
	Ask    float64 `json:"ask"`
	Mid    float64 `json:"mid"`
}

type legsDTO struct {
	SellCallK1 legDTO `json:"sell_call_k1"`
	BuyCallK2  legDTO `json:"buy_call_k2"`
	BuyPutK1   legDTO `json:"buy_put_k1"`
	SellPutK2  legDTO `json:"sell_put_k2"`
}

type rowDTO struct {
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
	Legs        legsDTO `json:"legs"`
}

type statsDTO struct {
	Total      int            `json:"total"`
	Valid      int            `json:"valid"`
	Skipped    int            `json:"skipped"`
	Rejected   map[string]int `json:"rejected"`
	Underlying string         `json:"underlying"`
}

type rulesDTO struct {
	ThresholdBP   float64 `json:"threshold_bp"`
	ConfirmRounds int     `json:"confirm_rounds"`
	HysteresisBP  float64 `json:"hysteresis_bp"`
	CooldownSec   float64 `json:"cooldown_sec"`
	IntervalSec   float64 `json:"interval_sec"`
}

type statusDTO struct {
	Symbol           string         `json:"symbol"`
	Underlying       string         `json:"underlying"`
	BenchmarkPct     float64        `json:"benchmark_pct"`
	Total            int            `json:"total"`
	Valid            int            `json:"valid"`
	Rejected         map[string]int `json:"rejected"`
	LastRoundAt      string         `json:"last_round_at"`
	DataAgeSec       float64        `json:"data_age_sec"`
	Stale            bool           `json:"stale"`
	AlertsSent       int            `json:"alerts_sent"`
	LastError        string         `json:"last_error,omitempty"`
	OpportunityCount int            `json:"opportunity_count"`
	Rules            rulesDTO       `json:"rules"`
}

type opportunityDTO struct {
	Expiry      string  `json:"expiry"`
	D           int     `json:"D"`
	K1          int64   `json:"K1"`
	K2          int64   `json:"K2"`
	Width       int64   `json:"width"`
	SpreadBP    float64 `json:"spread_bp"`
	RNaturalPct float64 `json:"r_natural_pct"`
	Consecutive int     `json:"consecutive"`
	Alerted     bool    `json:"alerted"`
}

type scanDTO struct {
	Rows  []rowDTO `json:"rows"`
	Stats statsDTO `json:"stats"`
}

type watchDTO struct {
	Interval      string   `json:"interval"`
	ThresholdBP   float64  `json:"threshold_bp"`
	ConfirmRounds int      `json:"confirm_rounds"`
	HysteresisBP  float64  `json:"hysteresis_bp"`
	Cooldown      string   `json:"cooldown"`
	Channels      []string `json:"channels"`
}

type configDTO struct {
	Symbol          string   `json:"symbol"`
	Broker          string   `json:"broker"`
	BenchmarkRate   float64  `json:"benchmark_rate"`
	Expiries        []string `json:"expiries"`
	Widths          []int64  `json:"widths"`
	StrikeStep      int64    `json:"strike_step"`
	K1RangePct      float64  `json:"k1_range_pct"`
	MinDaysToExpiry int      `json:"min_days_to_expiry"`
	Watch           watchDTO `json:"watch"`
}

// configUpdate carries only the fields the client changed. A nil
// pointer means "leave as is".
type configUpdate struct {
	BenchmarkRate *float64  `json:"benchmark_rate"`
	Expiries      *[]string `json:"expiries"`
	ThresholdBP   *float64  `json:"threshold_bp"`
	ConfirmRounds *int      `json:"confirm_rounds"`
	HysteresisBP  *float64  `json:"hysteresis_bp"`
	Interval      *string   `json:"interval"`
	Cooldown      *string   `json:"cooldown"`
}

// --- handlers ---------------------------------------------------------

// handleStatus reports the scanner's overall state.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	now := s.opts.Now()

	s.mu.RLock()
	symbol := s.cfg.Symbol
	bench := s.bench
	s.mu.RUnlock()

	age := scan.MaxDataAge(snap.Stats.DataTimes, now)

	writeJSON(w, http.StatusOK, statusDTO{
		Symbol:           symbol,
		Underlying:       snap.Stats.Underlying.String(),
		BenchmarkPct:     bench * 100,
		Total:            snap.Stats.Total,
		Valid:            snap.Stats.Valid,
		Rejected:         rejectedCopy(snap.Stats.Rejected),
		LastRoundAt:      fmtTime(snap.LastRoundAt),
		DataAgeSec:       age.Seconds(),
		Stale:            snap.StaleData,
		AlertsSent:       snap.AlertsSent,
		LastError:        snap.LastError,
		OpportunityCount: len(snap.Opportunities),
		Rules:            s.rulesDTOFrom(snap),
	})
}

// handleScan returns the last scan's rows.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	s.mu.RLock()
	bench := s.bench
	s.mu.RUnlock()

	rows := make([]rowDTO, 0, len(snap.Rows))
	for _, res := range snap.Rows {
		rows = append(rows, rowDTOFrom(res, bench))
	}
	writeJSON(w, http.StatusOK, scanDTO{
		Rows: rows,
		Stats: statsDTO{
			Total:      snap.Stats.Total,
			Valid:      snap.Stats.Valid,
			Skipped:    snap.Stats.Skipped,
			Rejected:   rejectedCopy(snap.Stats.Rejected),
			Underlying: snap.Stats.Underlying.String(),
		},
	})
}

// handleOpportunities returns the combos currently meeting the trigger
// condition, with their episode state.
func (s *Server) handleOpportunities(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	out := make([]opportunityDTO, 0, len(snap.Opportunities))
	for _, o := range snap.Opportunities {
		out = append(out, opportunityDTO{
			Expiry:      o.Expiry.Format("2006-01-02"),
			D:           o.D,
			K1:          int64(o.K1),
			K2:          int64(o.K2),
			Width:       int64(o.Width),
			SpreadBP:    o.SpreadBP,
			RNaturalPct: o.RNaturalPct,
			Consecutive: o.Consecutive,
			Alerted:     o.Alerted,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"opportunities": out})
}

// handleGetConfig returns the effective configuration.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, s.configDTOLocked())
}

// handleUpdateConfig applies a partial configuration change and
// hot-swaps it into the running engine.
func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var upd configUpdate
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&upd); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	s.mu.RLock()
	next := cloneConfig(s.cfg)
	s.mu.RUnlock()

	if upd.BenchmarkRate != nil {
		next.Benchmark.Rate = *upd.BenchmarkRate
	}
	if upd.Expiries != nil {
		next.Scan.Expiries = append([]string(nil), (*upd.Expiries)...)
	}
	if upd.ThresholdBP != nil {
		v := *upd.ThresholdBP
		next.Watch.ThresholdBP = &v
	}
	if upd.ConfirmRounds != nil {
		v := *upd.ConfirmRounds
		next.Watch.ConfirmRounds = &v
	}
	if upd.HysteresisBP != nil {
		v := *upd.HysteresisBP
		next.Watch.HysteresisBP = &v
	}
	if upd.Interval != nil {
		next.Watch.Interval = *upd.Interval
	}
	if upd.Cooldown != nil {
		next.Watch.Cooldown = *upd.Cooldown
	}

	now := s.opts.Now()
	if err := next.Validate(now); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := next.ValidateWatch(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	wopts, err := next.ParseWatch()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	params, err := app.BuildParams(next)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rules := watchRules(wopts)
	s.mu.Lock()
	s.cfg = next
	s.params = params
	s.rules = rules
	s.bench = next.Benchmark.Rate
	dto := s.configDTOLocked()
	s.mu.Unlock()

	// Episode state survives the change: an opportunity that already
	// alerted must not fire again just because a knob moved.
	s.engine.UpdateParams(params)
	s.engine.UpdateRules(rules, next.Benchmark.Rate)

	s.opts.Logger.Printf("config updated: threshold %+.0fbp, confirm %d, hysteresis %+.0fbp",
		rules.ThresholdBP, rules.ConfirmRounds, rules.HysteresisBP)
	writeJSON(w, http.StatusOK, dto)
}

// configDTOLocked renders the effective configuration. The caller must
// hold s.mu.
func (s *Server) configDTOLocked() configDTO {
	opts, err := s.cfg.ParseWatch()
	if err != nil {
		opts = config.WatchOpts{}
	}
	return configDTO{
		Symbol:          s.cfg.Symbol,
		Broker:          s.cfg.Broker,
		BenchmarkRate:   s.cfg.Benchmark.Rate,
		Expiries:        append([]string(nil), s.cfg.Scan.Expiries...),
		Widths:          append([]int64(nil), s.cfg.Scan.Widths...),
		StrikeStep:      s.cfg.Scan.StrikeStep,
		K1RangePct:      s.cfg.Scan.K1RangePct,
		MinDaysToExpiry: s.cfg.Scan.MinDaysToExpiry,
		Watch: watchDTO{
			Interval:      opts.Interval.String(),
			ThresholdBP:   opts.ThresholdBP,
			ConfirmRounds: opts.ConfirmRounds,
			HysteresisBP:  opts.HysteresisBP,
			Cooldown:      opts.Cooldown.String(),
			Channels:      s.cfg.WatchChannels(),
		},
	}
}

// --- conversion helpers ----------------------------------------------

// watchRules maps the parsed watch options onto the engine's rules.
func watchRules(o config.WatchOpts) watch.Rules {
	return watch.Rules{
		ThresholdBP:   o.ThresholdBP,
		ConfirmRounds: o.ConfirmRounds,
		HysteresisBP:  o.HysteresisBP,
		Cooldown:      o.Cooldown,
	}
}

// rulesDTOFrom renders the engine's current trigger settings.
func (s *Server) rulesDTOFrom(snap watch.Snapshot) rulesDTO {
	return rulesDTO{
		ThresholdBP:   snap.Rules.ThresholdBP,
		ConfirmRounds: snap.Rules.ConfirmRounds,
		HysteresisBP:  snap.Rules.HysteresisBP,
		CooldownSec:   snap.Rules.Cooldown.Seconds(),
		IntervalSec:   s.currentInterval().Seconds(),
	}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func rejectedCopy(in map[scan.RejectReason]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

func legDTOFrom(side string, strike int64, kind string, q box.Quote) legDTO {
	return legDTO{
		Side:   side,
		Strike: strike,
		Kind:   kind,
		Bid:    q.Bid.Float64(),
		Ask:    q.Ask.Float64(),
		Mid:    q.Mid().Float64(),
	}
}

func rowDTOFrom(r scan.Result, benchmark float64) rowDTO {
	c := r.Combo
	return rowDTO{
		Expiry:      c.Expiry.Format("2006-01-02"),
		D:           c.D,
		K1:          int64(c.K1),
		K2:          int64(c.K2),
		Width:       int64(c.Width()),
		CMid:        r.Credits.Mid.Float64(),
		CNatural:    r.Credits.Natural.Float64(),
		RMidPct:     r.RMid.Percent(),
		RNaturalPct: r.RNatural.Percent(),
		SpreadBP:    r.SpreadBPS(benchmark),
		LegsOK:      4,
		Legs: legsDTO{
			SellCallK1: legDTOFrom("SELL", int64(c.K1), "call", r.Legs.SellCallK1),
			BuyCallK2:  legDTOFrom("BUY", int64(c.K2), "call", r.Legs.BuyCallK2),
			BuyPutK1:   legDTOFrom("BUY", int64(c.K1), "put", r.Legs.BuyPutK1),
			SellPutK2:  legDTOFrom("SELL", int64(c.K2), "put", r.Legs.SellPutK2),
		},
	}
}

// cloneConfig copies a configuration so it can be edited without
// touching the live one.
func cloneConfig(c *config.Config) *config.Config {
	out := *c
	out.Scan.Expiries = append([]string(nil), c.Scan.Expiries...)
	out.Scan.Widths = append([]int64(nil), c.Scan.Widths...)
	out.Filters = c.Filters
	out.Output = c.Output
	out.Watch.Channels = append([]string(nil), c.Watch.Channels...)
	out.Watch.WhatsApp = c.Watch.WhatsApp
	if c.Watch.ThresholdBP != nil {
		v := *c.Watch.ThresholdBP
		out.Watch.ThresholdBP = &v
	}
	if c.Watch.ConfirmRounds != nil {
		v := *c.Watch.ConfirmRounds
		out.Watch.ConfirmRounds = &v
	}
	if c.Watch.HysteresisBP != nil {
		v := *c.Watch.HysteresisBP
		out.Watch.HysteresisBP = &v
	}
	return &out
}
