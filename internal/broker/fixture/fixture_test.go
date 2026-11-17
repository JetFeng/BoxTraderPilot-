package fixture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/broker"
)

func loadFixture(t *testing.T) *File {
	t.Helper()
	f, err := Load("../../testdata/spx_chain_fixture.json")
	if err != nil {
		t.Skipf("fixture not generated yet: %v (run: go run ./cmd/fixturegen)", err)
	}
	return f
}

func TestBrokerQuote(t *testing.T) {
	f := loadFixture(t)
	b := NewBroker(f)
	q, err := b.GetQuote(context.Background(), "SPX")
	if err != nil {
		t.Fatal(err)
	}
	if q.Price != box.MustParsePrice("5912.3") {
		t.Errorf("price = %v", q.Price)
	}
	if !q.Timestamp.Equal(f.AsOf) {
		t.Errorf("timestamp = %v", q.Timestamp)
	}
	if _, err := b.GetQuote(context.Background(), "RUT"); !errors.Is(err, broker.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestBrokerChain(t *testing.T) {
	f := loadFixture(t)
	b := NewBroker(f)
	exp, _ := time.Parse("2006-01-02", "2026-12-18")
	ch, err := b.GetOptionChain(context.Background(), "SPX", exp)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Timestamp != f.AsOf {
		t.Errorf("timestamp = %v", ch.Timestamp)
	}
	sq, ok := ch.At(5900)
	if !ok {
		t.Fatal("strike 5900 missing")
	}
	if sq.Call == nil || sq.Put == nil {
		t.Fatal("expected call and put at 5900")
	}
	if !(sq.Call.Bid > 0 && sq.Call.Bid < sq.Call.Ask) {
		t.Errorf("call quote = %+v", sq.Call)
	}
	// Strike 6200 in first expiry has a missing put by construction.
	sq6200, ok := ch.At(6200)
	if !ok {
		t.Fatal("strike 6200 missing")
	}
	if sq6200.Put != nil {
		t.Error("expected nil put at 6200")
	}
	_, err = b.GetOptionChain(context.Background(), "SPX", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, broker.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"as_of": "x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "bad.json")); err == nil {
		t.Fatal("expected error for bad as_of")
	}
	bad2 := `{"as_of": "2026-11-06T21:00:00Z", "underlying": {"symbol": "SPX", "price": "abc"}}`
	if err := os.WriteFile(filepath.Join(dir, "bad2.json"), []byte(bad2), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "bad2.json")); err == nil {
		t.Fatal("expected error for bad price")
	}
}
