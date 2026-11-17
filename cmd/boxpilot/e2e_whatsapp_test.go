//go:build !whatsapp

// WhatsApp channel acceptance test for the DEFAULT build.
//
// Build-tag guard: in a -tags whatsapp build the channel is real and
// Connect() blocks on terminal QR pairing, so this test must not run
// there. Without this guard `go test -tags whatsapp ./...` hangs
// forever waiting for a phone scan.
package main

import (
	"os"
	"strings"
	"testing"
)

func TestEndToEndWatchWhatsAppNeedsBuildTag(t *testing.T) {
	// In the default build (no whatsapp tag), selecting the whatsapp
	// channel must fail with rebuild instructions.
	cfg := writeE2EConfig(t, "", "table")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\nwatch:\n  interval: 30s\n  channels: [whatsapp]\n")...)
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, diag, code := runCLI(t, []string{"-config", cfg, "-watch", "-fixture", fixturePath}, fixtureNow)
	if code == 0 {
		t.Errorf("expected non-zero exit without whatsapp build tag")
	}
	if !strings.Contains(diag, "-tags whatsapp") {
		t.Errorf("error should explain the build tag:\n%s", diag)
	}
}
