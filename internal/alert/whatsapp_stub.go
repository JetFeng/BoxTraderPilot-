//go:build !whatsapp

// Stub build (no "whatsapp" tag): the WhatsApp channel reports a clear
// build instruction instead of pulling in whatsmeow. Keeps the default
// build lightweight and dependency-free beyond yaml.
package alert

import (
	"context"
	"time"
)

// WhatsApp is the disabled stub.
type WhatsApp struct{}

// NewWhatsApp always returns ErrNotBuilt in this build.
func NewWhatsApp(WhatsAppConfig) (*WhatsApp, error) { return nil, ErrNotBuilt }

// Connect implements the WhatsApp lifecycle, always failing.
func (w *WhatsApp) Connect(context.Context) error { return ErrNotBuilt }

// Notify implements Alerter.
func (w *WhatsApp) Notify(context.Context, Opportunity) error { return ErrNotBuilt }

// NotifyStale implements Alerter.
func (w *WhatsApp) NotifyStale(context.Context, time.Duration, time.Duration) error {
	return ErrNotBuilt
}

// Close implements the WhatsApp lifecycle.
func (w *WhatsApp) Close() error { return nil }
