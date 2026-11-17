// Package broker defines the read-only market data surface of BoxPilot.
//
// Security red line (Phase 1): this interface is strictly read-only. It
// must never gain account, position, or trading capabilities. Any order-
// related identifier is forbidden in this repository (CI grep guard).
package broker

import (
	"context"
	"fmt"
	"sort"
	"time"

	"boxpilot/internal/box"
)

// Quote is a single underlying quote snapshot.
type Quote struct {
	Price     box.Price
	Timestamp time.Time
}

// Contract is one option contract's market data.
type Contract struct {
	Bid, Ask         box.Price
	Last             box.Price
	Volume, Interest int64
}

// StrikeQuotes couples call and put quotes at one strike (points).
type StrikeQuotes struct {
	Strike int64
	Call   *Contract
	Put    *Contract
}

// OptionChain is a whole expiry's chain as one snapshot.
type OptionChain struct {
	Symbol    string
	Expiry    time.Time
	Timestamp time.Time // as-of quote time of the snapshot
	Strikes   []StrikeQuotes
}

// At returns the quotes at the given strike, if present.
func (c OptionChain) At(strike int64) (StrikeQuotes, bool) {
	i := sort.Search(len(c.Strikes), func(i int) bool { return c.Strikes[i].Strike >= strike })
	if i < len(c.Strikes) && c.Strikes[i].Strike == strike {
		return c.Strikes[i], true
	}
	return StrikeQuotes{}, false
}

// Broker is the read-only market data interface.
type Broker interface {
	// GetOptionChain fetches the whole chain for one expiry in a single
	// request. Implementations must batch — one request per expiry.
	GetOptionChain(ctx context.Context, symbol string, expiry time.Time) (OptionChain, error)
	// GetQuote fetches the underlying quote (used for the K1 range).
	GetQuote(ctx context.Context, symbol string) (Quote, error)
}

// ErrNotFound reports a missing expiry/symbol in a provider response.
var ErrNotFound = fmt.Errorf("not found")
