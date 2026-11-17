//go:build whatsapp

// WhatsApp channel acceptance test for the TAGGED build.
//
// The tagged build has a real WhatsApp client, so the untagged test
// ("the channel must report ErrNotBuilt") cannot apply. Instead this
// file asserts that the channel is genuinely compiled in and that
// constructing it only touches the local session store — no network.
//
// Nothing here may call Connect(): that would block on terminal QR
// pairing and hang the test run.
package main

import (
	"errors"
	"path/filepath"
	"testing"

	"boxpilot/internal/alert"
)

func TestTaggedBuildHasRealWhatsAppChannel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session.db")
	w, err := alert.NewWhatsApp(alert.WhatsAppConfig{DBPath: dbPath})
	if err != nil {
		t.Fatalf("NewWhatsApp in tagged build: %v", err)
	}
	if w == nil {
		t.Fatal("NewWhatsApp returned a nil client")
	}
	if errors.Is(err, alert.ErrNotBuilt) {
		t.Fatal("tagged build still reports ErrNotBuilt")
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
