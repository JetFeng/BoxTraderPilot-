package logx

import (
	"bytes"
	"strings"
	"testing"
)

func TestSanitizeMasksSecrets(t *testing.T) {
	fakeToken := "FAKETOKEN-aaaa-bbbb-cccc-dddddddddddd"
	fakeSecret := "APPSECRET-1234567890"
	out := Sanitize("token="+fakeToken+" secret="+fakeSecret+" ok=SPX",
		[]string{fakeToken, fakeSecret})
	if strings.Contains(out, fakeToken) || strings.Contains(out, fakeSecret) {
		t.Fatalf("secrets leaked: %q", out)
	}
	if !strings.Contains(out, "***") || !strings.Contains(out, "ok=SPX") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestSanitizeLongestFirst(t *testing.T) {
	// The longer secret contains the shorter one; the shorter must not
	// eat part of the longer and leave a leak.
	long := "ABCDEFGHIJ-long-secret-value"
	short := "ABCDEFGHIJ"
	out := Sanitize("x "+long+" y", []string{short, long})
	if strings.Contains(out, "ABCDEFGHIJ") {
		t.Fatalf("partial leak: %q", out)
	}
}

func TestSanitizeIgnoresShortOrEmpty(t *testing.T) {
	if got := Sanitize("abc abcd", []string{"", "abc"}); got != "abc abcd" {
		t.Fatalf("short secrets must not be masked: %q", got)
	}
}

func TestLoggerMasks(t *testing.T) {
	fakeToken := "REFRESHTOKEN-zzz-yyy-xxx"
	var buf bytes.Buffer
	l := New(&buf, "test ", []string{fakeToken})
	l.Printf("refresh with %s done", fakeToken)
	l.Println("again", fakeToken)
	out := buf.String()
	if strings.Contains(out, fakeToken) {
		t.Fatalf("token leaked into log: %q", out)
	}
	if !strings.Contains(out, "***") {
		t.Fatalf("expected mask in log: %q", out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "test") {
		t.Fatalf("prefix missing: %q", out)
	}
}
