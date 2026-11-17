package alert

import (
	"context"
	"io"
	"log"
	"time"
)

// LogAlerter writes alerts to a logger — the zero-dependency channel and
// the default when no other channel is configured.
type LogAlerter struct {
	Log *log.Logger
}

// NewLogAlerter returns a LogAlerter writing to w.
func NewLogAlerter(w io.Writer) *LogAlerter {
	return &LogAlerter{Log: log.New(w, "ALERT ", log.LstdFlags|log.LUTC)}
}

// Notify implements Alerter.
func (l *LogAlerter) Notify(_ context.Context, o Opportunity) error {
	l.Log.Print(o.Text())
	return nil
}

// NotifyStale implements Alerter.
func (l *LogAlerter) NotifyStale(_ context.Context, age, limit time.Duration) error {
	l.Log.Print(StaleText(age, limit))
	return nil
}
