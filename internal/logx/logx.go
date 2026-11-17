// Package logx provides a logger that masks configured secrets so no
// token, secret, or account number can leak into logs (§4.3).
package logx

import (
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
)

const mask = "***"

// Sanitize replaces every occurrence of each secret in s with a masked
// placeholder. Longer secrets are replaced first so overlapping values
// do not partially leak.
func Sanitize(s string, secrets []string) string {
	uniq := make([]string, 0, len(secrets))
	seen := map[string]struct{}{}
	for _, sec := range secrets {
		if sec == "" || len(sec) < 4 {
			continue // nothing identifiable to mask
		}
		if _, ok := seen[sec]; ok {
			continue
		}
		seen[sec] = struct{}{}
		uniq = append(uniq, sec)
	}
	sort.Slice(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })
	for _, sec := range uniq {
		s = strings.ReplaceAll(s, sec, mask)
	}
	return s
}

// Logger wraps log.Logger with secret masking.
type Logger struct {
	mu      sync.Mutex
	inner   *log.Logger
	secrets []string
}

// New returns a Logger writing to w with the given secret values masked.
func New(w io.Writer, prefix string, secrets []string) *Logger {
	return &Logger{
		inner:   log.New(w, prefix, log.LstdFlags|log.LUTC),
		secrets: append([]string(nil), secrets...),
	}
}

// SetSecrets replaces the masked secret set (e.g. after a token refresh).
func (l *Logger) SetSecrets(secrets []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secrets = append([]string(nil), secrets...)
}

// Printf logs a masked formatted message.
func (l *Logger) Printf(format string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := Sanitize(fmt.Sprintf(format, v...), l.secrets)
	l.inner.Print(msg)
}

// Println logs masked values.
func (l *Logger) Println(v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprint(x)
	}
	l.inner.Print(Sanitize(strings.Join(parts, " "), l.secrets))
}
