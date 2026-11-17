package alert

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Multi fans an alert out to several alerters. Individual failures are
// aggregated; delivery continues to the remaining channels.
type Multi struct {
	channels []Alerter
}

// NewMulti builds a fan-out alerter. With no channels it behaves like a
// LogAlerter writing to io.Discard (silently succeeds) — callers should
// prefer at least one channel.
func NewMulti(channels ...Alerter) *Multi {
	return &Multi{channels: channels}
}

// Notify implements Alerter.
func (m *Multi) Notify(ctx context.Context, o Opportunity) error {
	var errs []error
	for _, ch := range m.channels {
		if err := ch.Notify(ctx, o); err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", ch, err))
		}
	}
	return errors.Join(errs...)
}

// NotifyStale implements Alerter.
func (m *Multi) NotifyStale(ctx context.Context, age, limit time.Duration) error {
	var errs []error
	for _, ch := range m.channels {
		if err := ch.NotifyStale(ctx, age, limit); err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", ch, err))
		}
	}
	return errors.Join(errs...)
}
