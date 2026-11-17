// Package alert delivers watch-mode notifications. Alerting is
// outbound-only and read-only — it never touches trading functionality.
package alert

import (
	"context"
	"fmt"
	"strings"
	"time"

	"boxpilot/internal/scan"
)

// Opportunity is one alert-worthy scan result.
type Opportunity struct {
	Symbol    string
	Benchmark float64 // plain fraction, e.g. 0.065
	Result    scan.Result
}

// Text renders the opportunity as a plain-text alert message.
func (o Opportunity) Text() string {
	r := o.Result
	c := r.Combo
	var b strings.Builder
	fmt.Fprintf(&b, "ALERT: %s short box cheaper than benchmark %.2f%%\n", o.Symbol, o.Benchmark*100)
	fmt.Fprintf(&b, "expiry %s D=%d  K1=%d K2=%d width=%d\n",
		c.Expiry.Format("2006-01-02"), c.D, c.K1, c.K2, c.Width())
	fmt.Fprintf(&b, "r_natural %.4f%%  r_mid %.4f%%  spread %+.1fbp\n",
		r.RNatural.Percent(), r.RMid.Percent(), r.SpreadBPS(o.Benchmark))
	fmt.Fprintf(&b, "C_natural %s  C_mid %s points\n", r.Credits.Natural, r.Credits.Mid)
	b.WriteString("legs (bid/ask/mid):\n")
	legs := []struct {
		side   string
		strike int64
		kind   string
		q      [3]string // bid, ask, mid
	}{
		{"SELL", int64(c.K1), "call", [3]string{r.Legs.SellCallK1.Bid.String(), r.Legs.SellCallK1.Ask.String(), r.Legs.SellCallK1.Mid().String()}},
		{"BUY", int64(c.K2), "call", [3]string{r.Legs.BuyCallK2.Bid.String(), r.Legs.BuyCallK2.Ask.String(), r.Legs.BuyCallK2.Mid().String()}},
		{"BUY", int64(c.K1), "put", [3]string{r.Legs.BuyPutK1.Bid.String(), r.Legs.BuyPutK1.Ask.String(), r.Legs.BuyPutK1.Mid().String()}},
		{"SELL", int64(c.K2), "put", [3]string{r.Legs.SellPutK2.Bid.String(), r.Legs.SellPutK2.Ask.String(), r.Legs.SellPutK2.Mid().String()}},
	}
	for _, leg := range legs {
		fmt.Fprintf(&b, "  %-4s %d %-4s %s/%s/%s\n", leg.side, leg.strike, leg.kind, leg.q[0], leg.q[1], leg.q[2])
	}
	return b.String()
}

// StaleText renders the stale-data suspension notice.
func StaleText(age time.Duration, limit time.Duration) string {
	return fmt.Sprintf("WARNING: BoxPilot market data is %s old (limit %s); opportunity alerts suspended until data is fresh",
		age.Round(time.Minute), limit)
}

// Alerter delivers alert messages. All methods must be safe for
// repeated concurrent use from the watch engine.
type Alerter interface {
	// Notify sends one opportunity alert.
	Notify(ctx context.Context, o Opportunity) error
	// NotifyStale sends a data-staleness suspension notice.
	NotifyStale(ctx context.Context, age time.Duration, limit time.Duration) error
}
