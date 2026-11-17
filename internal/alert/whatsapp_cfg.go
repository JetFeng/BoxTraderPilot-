package alert

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotBuilt explains how to enable WhatsApp support. It is returned
// by NewWhatsApp in binaries built without the "whatsapp" tag.
var ErrNotBuilt = errors.New("whatsapp channel requires a binary built with -tags whatsapp (see README)")

// WhatsAppConfig configures the WhatsApp alerter. It is shared between
// the whatsmeow build (tag "whatsapp") and the stub build.
type WhatsAppConfig struct {
	// DBPath is the SQLite session store file. It holds the account's
	// session keys — treat it like a credential and never commit it.
	DBPath string
	// Recipient is the alert destination in E.164 digits (no "+"),
	// e.g. "14155550123". Empty means the paired account itself.
	Recipient string
}

// ParseE164 validates and normalizes an E.164 recipient: optional "+",
// then 7–15 ASCII digits.
func ParseE164(s string) (string, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "+")
	if len(s) < 7 || len(s) > 15 {
		return "", fmt.Errorf("phone number %q: must be 7-15 digits", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("phone number %q: digits only", s)
		}
	}
	return s, nil
}
