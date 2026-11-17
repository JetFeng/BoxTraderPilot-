//go:build !whatsapp

// Tests for the default build, where the WhatsApp channel is a stub
// that refuses to run. The shared alerting tests live in
// alert_common_test.go so they also run in the tagged build.
package alert

import (
	"context"
	"errors"
	"testing"
)

func TestStubReportsBuildInstruction(t *testing.T) {
	// Runs in the default (non-whatsapp) build.
	w, err := NewWhatsApp(WhatsAppConfig{DBPath: "x.db"})
	if !errors.Is(err, ErrNotBuilt) || w != nil {
		t.Fatalf("NewWhatsApp = %v, %v; want ErrNotBuilt", w, err)
	}
	stub := &WhatsApp{}
	if !errors.Is(stub.Notify(context.Background(), Opportunity{}), ErrNotBuilt) {
		t.Fatal("stub Notify must return ErrNotBuilt")
	}
	if !errors.Is(stub.NotifyStale(context.Background(), 0, 0), ErrNotBuilt) {
		t.Fatal("stub NotifyStale must return ErrNotBuilt")
	}
	if err := stub.Close(); err != nil {
		t.Fatal(err)
	}
}
