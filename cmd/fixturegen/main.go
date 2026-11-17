// Command fixturegen writes a deterministic, clearly FAKE SPX option
// chain snapshot to testdata/spx_chain_fixture.json so the scanner can
// be developed and verified with zero network access. It is a dev tool;
// nothing here is real market data.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"time"

	"boxpilot/internal/broker/fixture"
)

const (
	asOfStr      = "2026-11-06T21:00:00Z" // fake snapshot time (UTC)
	underlyingPx = 5912.30                // fake index level
	strikeLo     = 5300
	strikeHi     = 6500
	strikeStep   = 5
	rate         = 0.065 // synthetic risk-free rate used by the pricer
	seed         = 20261106
)

var expiries = []string{"2026-12-18", "2027-01-15", "2027-03-19"}

// normCDF is the standard normal CDF via math.Erf.
func normCDF(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt2)) }

// bs prices a European option (q = 0) with the Black-Scholes formula.
func bs(call bool, s, k, t, r, sigma float64) float64 {
	if t <= 0 || sigma <= 0 {
		if call {
			return math.Max(s-k, 0)
		}
		return math.Max(k-s, 0)
	}
	sq := sigma * math.Sqrt(t)
	d1 := (math.Log(s/k) + (r+sigma*sigma/2)*t) / sq
	d2 := d1 - sq
	disc := math.Exp(-r * t)
	if call {
		return s*normCDF(d1) - k*disc*normCDF(d2)
	}
	return k*disc*normCDF(-d2) - s*normCDF(-d1)
}

func round2(v float64) string { return trimFloat(math.Round(v*100) / 100) }

// trimFloat formats v with two decimals, trimming redundant zeros.
func trimFloat(v float64) string {
	s := fmt.Sprintf("%.10f", v)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s += "0"
	}
	return s
}

func main() {
	asOf, err := time.Parse(time.RFC3339, asOfStr)
	if err != nil {
		panic(err)
	}
	rng := rand.New(rand.NewSource(seed))
	out := fixture.File{
		AsOf:       asOf,
		Underlying: fixture.Underlying{Symbol: "SPX", Price: round2(underlyingPx)},
	}

	for ei, expStr := range expiries {
		exp, _ := time.Parse("2006-01-02", expStr)
		days := int(exp.Sub(asOf.Truncate(24*time.Hour)).Hours() / 24) // ~calendar DTE
		t := float64(days) / 365.0
		ch := fixture.Chain{Expiry: expStr}
		for k := strikeLo; k <= strikeHi; k += strikeStep {
			sigma := 0.13 + 0.00000002*float64(k)*float64(k)/100 // gentle fake smile
			callPx := bs(true, underlyingPx, float64(k), t, rate, sigma)
			putPx := bs(false, underlyingPx, float64(k), t, rate, sigma)
			st := fixture.Strike{Strike: int64(k)}

			mk := func(px float64) *fixture.Contract {
				spread := 0.02 + 0.03*rng.Float64()
				bid := px * (1 - spread/2)
				ask := px*(1+spread/2) + 0.01
				// Deep OTM quotes: sometimes no bid (illiquid).
				if px < 0.10 && rng.Float64() < 0.5 {
					bid = 0
				}
				return &fixture.Contract{
					Bid:          round2(math.Max(bid, 0)),
					Ask:          round2(ask),
					Last:         round2(px * (1 + (rng.Float64()-0.5)*0.01)),
					Volume:       int64(rng.Intn(4000)),
					OpenInterest: int64(rng.Intn(20000)),
				}
			}
			st.Call = mk(callPx)
			st.Put = mk(putPx)

			// Deterministic anomalies to exercise the filters:
			switch {
			case ei == 0 && k == 5800: // crossed quote → bad_quote
				st.Call.Bid, st.Call.Ask = st.Call.Ask, st.Call.Bid
			case ei == 0 && k == 6200: // missing put → missing_quote
				st.Put = nil
			case ei == 1 && k == 5350: // zero bid → min_bid / bad_quote
				st.Put.Bid = "0.00"
			}
			ch.Strikes = append(ch.Strikes, st)
		}
		out.Chains = append(out.Chains, ch)
	}

	sort.Slice(out.Chains, func(i, j int) bool { return out.Chains[i].Expiry < out.Chains[j].Expiry })
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		panic(err)
	}
	path := "testdata/spx_chain_fixture.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s (%d chains)\n", path, len(out.Chains))
}
