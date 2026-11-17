//go:build whatsapp

// Tests for the tagged build, where the WhatsApp channel is real.
//
// Only the local session store is exercised here: Connect() blocks on
// terminal QR pairing and must never be called from a test.
package alert

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestWhatsAppBuildOpensSessionStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session.db")
	w, err := NewWhatsApp(WhatsAppConfig{DBPath: dbPath})
	if err != nil {
		t.Fatalf("NewWhatsApp in tagged build: %v", err)
	}
	if w == nil {
		t.Fatal("NewWhatsApp returned a nil client")
	}
	if errors.Is(err, ErrNotBuilt) {
		t.Fatal("tagged build must not report ErrNotBuilt")
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
