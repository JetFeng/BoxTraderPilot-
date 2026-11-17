package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixturePath = "../../testdata/spx_chain_fixture.json"

// fixtureNow is five minutes after the fixture's as_of timestamp, so
// the data counts as fresh.
var fixtureNow = time.Date(2026, 11, 6, 21, 5, 0, 0, time.UTC)

const testYAML = `
symbol: SPX
broker: fixture
fixture:
  path: %s
scan:
  expiries: ["2026-12-18", "2027-01-15", "2027-03-19"]
  widths: [250, 500, 1000]
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
  top: 30
watch:
  interval: 30s
  threshold_bp: -50
  confirm_rounds: 2
  hysteresis_bp: 25
  cooldown: 1m
  channels: [log]
`

// newTestServer builds a server backed by the offline fixture and
// returns an httptest server around its handler.
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "boxpilot.yaml")
	yaml := strings.Replace(testYAML, "%s", fixturePath, 1)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		ConfigPath:  cfgPath,
		FixturePath: fixturePath,
		Diag:        io.Discard,
		Now:         func() time.Time { return fixtureNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// getJSON fetches a path and decodes the JSON body.
func getJSON(t *testing.T, ts *httptest.Server, path string, out any) {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("GET %s = HTTP %d: %s", path, res.StatusCode, body)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
}

// postJSON sends a JSON body and returns the status code and body.
func postJSON(t *testing.T, ts *httptest.Server, path string, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(ts.URL+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

// runRound performs one scan round so the snapshot is populated.
func runRound(t *testing.T, s *Server) {
	t.Helper()
	if err := s.engine.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIndexServesTheSPA(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "BoxPilot") {
		t.Errorf("index page missing branding:\n%.200s", body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestAssetsAreEmbedded(t *testing.T) {
	_, ts := newTestServer(t)
	for path, wantType := range map[string]string{
		"/assets/app.js":    "javascript",
		"/assets/style.css": "css",
	} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = HTTP %d", path, res.StatusCode)
			continue
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Errorf("GET %s Content-Type = %q, want %q", path, ct, wantType)
		}
	}
}

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t)
	var out map[string]string
	getJSON(t, ts, "/healthz", &out)
	if out["status"] != "ok" {
		t.Errorf("healthz = %v", out)
	}
}

func TestStatusReportsTheScan(t *testing.T) {
	s, ts := newTestServer(t)
	runRound(t, s)

	var st statusDTO
	getJSON(t, ts, "/api/status", &st)
	if st.Symbol != "SPX" {
		t.Errorf("symbol = %q", st.Symbol)
	}
	if st.Total == 0 || st.Valid == 0 {
		t.Errorf("counts = %d/%d, want non-zero", st.Valid, st.Total)
	}
	if st.Underlying != "5912.3000" {
		t.Errorf("underlying = %q, want 5912.3000", st.Underlying)
	}
	if st.LastRoundAt == "" {
		t.Error("last_round_at is empty")
	}
	if st.Stale {
		t.Error("fixture data 5 minutes old must not be stale")
	}
	if st.Rules.ThresholdBP != -50 {
		t.Errorf("threshold = %v, want -50", st.Rules.ThresholdBP)
	}
}

func TestScanReturnsRowsWithLegs(t *testing.T) {
	s, ts := newTestServer(t)
	runRound(t, s)

	var out scanDTO
	getJSON(t, ts, "/api/scan", &out)
	if len(out.Rows) == 0 {
		t.Fatal("no rows")
	}
	// Rows come back sorted by r_natural ascending.
	for i := 1; i < len(out.Rows); i++ {
		if out.Rows[i].RNaturalPct < out.Rows[i-1].RNaturalPct {
			t.Fatalf("row %d: r_natural %v < %v (not ascending)", i, out.Rows[i].RNaturalPct, out.Rows[i-1].RNaturalPct)
		}
	}
	top := out.Rows[0]
	if top.K1 != 5325 || top.K2 != 6325 {
		t.Errorf("top row = K1 %d K2 %d, want 5325/6325", top.K1, top.K2)
	}
	if top.Legs.SellCallK1.Bid != 715.82 {
		t.Errorf("sell call bid = %v, want 715.82", top.Legs.SellCallK1.Bid)
	}
	if top.LegsOK != 4 {
		t.Errorf("legs_ok = %d, want 4", top.LegsOK)
	}
}

func TestConfigUpdateAppliesImmediately(t *testing.T) {
	s, ts := newTestServer(t)
	runRound(t, s)

	code, body := postJSON(t, ts, "/api/config", map[string]any{
		"threshold_bp":   0,
		"confirm_rounds": 0,
		"hysteresis_bp":  0,
		"benchmark_rate": 0.055,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/config = HTTP %d: %s", code, body)
	}
	var dto configDTO
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Watch.ThresholdBP != 0 || dto.Watch.ConfirmRounds != 0 || dto.Watch.HysteresisBP != 0 {
		t.Errorf("explicit zero was replaced by a default: %+v", dto.Watch)
	}
	if dto.BenchmarkRate != 0.055 {
		t.Errorf("benchmark = %v, want 0.055", dto.BenchmarkRate)
	}

	// The running engine must pick the change up.
	var st statusDTO
	getJSON(t, ts, "/api/status", &st)
	if st.Rules.ThresholdBP != 0 {
		t.Errorf("engine threshold = %v, want 0", st.Rules.ThresholdBP)
	}
	if st.BenchmarkPct != 5.5 {
		t.Errorf("engine benchmark = %v, want 5.5", st.BenchmarkPct)
	}
	if got := s.engine.Snapshot().Rules.ThresholdBP; got != 0 {
		t.Errorf("engine rules threshold = %v, want 0", got)
	}
}

func TestConfigUpdateRejectsInvalidAndKeepsState(t *testing.T) {
	s, ts := newTestServer(t)
	runRound(t, s)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"past expiry", map[string]any{"expiries": []string{"2020-01-17"}}, "in the past"},
		{"negative hysteresis", map[string]any{"hysteresis_bp": -5}, "hysteresis_bp"},
		{"negative confirm rounds", map[string]any{"confirm_rounds": -1}, "confirm_rounds"},
		{"benchmark out of range", map[string]any{"benchmark_rate": 0.9}, "benchmark.rate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postJSON(t, ts, "/api/config", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("HTTP %d, want 400: %s", code, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("error %q does not mention %q", body, tc.want)
			}
		})
	}

	// The live configuration must be untouched by the rejections.
	var dto configDTO
	getJSON(t, ts, "/api/config", &dto)
	if dto.BenchmarkRate != 0.065 {
		t.Errorf("benchmark = %v, want the original 0.065", dto.BenchmarkRate)
	}
	if len(dto.Expiries) != 3 {
		t.Errorf("expiries = %v, want the original three", dto.Expiries)
	}
	if dto.Watch.ThresholdBP != -50 {
		t.Errorf("threshold = %v, want the original -50", dto.Watch.ThresholdBP)
	}
}

func TestConfigUpdateRejectsUnknownField(t *testing.T) {
	_, ts := newTestServer(t)
	code, body := postJSON(t, ts, "/api/config", map[string]any{"nope": 1})
	if code != http.StatusBadRequest {
		t.Fatalf("HTTP %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, "unknown field") {
		t.Errorf("error %q should name the unknown field", body)
	}
}

func TestOpportunitiesFollowTheThreshold(t *testing.T) {
	s, ts := newTestServer(t)
	runRound(t, s)

	// With the default threshold of -50bp nothing in the fixture qualifies.
	var out struct {
		Opportunities []opportunityDTO `json:"opportunities"`
	}
	getJSON(t, ts, "/api/opportunities", &out)
	if len(out.Opportunities) != 0 {
		t.Errorf("opportunities = %d, want 0 at a -50bp threshold", len(out.Opportunities))
	}

	// Loosen the threshold: the fixture's spreads are positive, so a
	// permissive threshold must surface rows.
	code, body := postJSON(t, ts, "/api/config", map[string]any{"threshold_bp": 2000})
	if code != http.StatusOK {
		t.Fatalf("POST /api/config = HTTP %d: %s", code, body)
	}
	runRound(t, s)

	getJSON(t, ts, "/api/opportunities", &out)
	if len(out.Opportunities) == 0 {
		t.Fatal("opportunities = 0, want some at a 2000bp threshold")
	}
	for _, o := range out.Opportunities {
		if o.SpreadBP > 2000 {
			t.Errorf("opportunity spread %v exceeds the threshold", o.SpreadBP)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", ":8080"} {
		if !isLoopback(addr) {
			t.Errorf("isLoopback(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0:8080", "192.168.1.10:8080", "example.com:443"} {
		if isLoopback(addr) {
			t.Errorf("isLoopback(%q) = true, want false", addr)
		}
	}
}

func TestCloneConfigIsolatesTheLiveConfig(t *testing.T) {
	s, _ := newTestServer(t)
	clone := cloneConfig(s.cfg)
	clone.Scan.Expiries[0] = "2099-01-01"
	clone.Scan.Widths[0] = 999
	*clone.Watch.ThresholdBP = 12345

	if s.cfg.Scan.Expiries[0] == "2099-01-01" {
		t.Error("clone shares the expiries slice with the live config")
	}
	if s.cfg.Scan.Widths[0] == 999 {
		t.Error("clone shares the widths slice with the live config")
	}
	if *s.cfg.Watch.ThresholdBP == 12345 {
		t.Error("clone shares the threshold pointer with the live config")
	}
}
