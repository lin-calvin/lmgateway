package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lmgateway/internal/store"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return d
}

func TestJSONRoundTrip(t *testing.T) {
	d := openTest(t)
	defer d.Close()
	ctx := context.Background()

	js, err := d.NewJSONStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()

	got, err := js.Get(ctx, "provider/x")
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	_ = got

	d1, err := js.Put(ctx, "provider/openai", map[string]any{"name": "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if d1.Version != 1 {
		t.Errorf("version should be 1, got %d", d1.Version)
	}
	d2, err := js.Put(ctx, "provider/openai", map[string]any{"name": "openai", "model": "gpt"})
	if err != nil {
		t.Fatal(err)
	}
	if d2.Version != 2 {
		t.Errorf("version should be 2, got %d", d2.Version)
	}
	if _, err := js.Update(ctx, "provider/openai", map[string]any{}, 1); err != store.ErrConflict {
		t.Errorf("stale update should conflict, got %v", err)
	}
	// restart persistence check
	provs, err := js.List(ctx, "provider/")
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 1 {
		t.Errorf("expected 1 provider, got %d", len(provs))
	}
}

func TestTSRoundTrip(t *testing.T) {
	d := openTest(t)
	defer d.Close()
	ctx := context.Background()

	back := d.NewTSBackend()
	now := time.Now().UTC()
	recs := []*store.TSRecord{
		{Ts: now, Stream: "spend", Tags: map[string]string{"provider": "p", "model": "m1"}, Fields: map[string]any{"total_tokens": 100.0}},
		{Ts: now.Add(time.Minute), Stream: "spend", Tags: map[string]string{"provider": "p", "model": "m2"}, Fields: map[string]any{"total_tokens": 200.0}},
	}
	if err := back.AppendBatch(ctx, recs); err != nil {
		t.Fatal(err)
	}
	got, err := back.Query(ctx, store.TSQuery{
		Stream: "spend", From: now.Add(-time.Hour), To: now.Add(time.Hour),
		Tag: map[string]string{"provider": "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got))
	}
	if got[0].Fields["total_tokens"] != 100.0 {
		t.Errorf("field mismatch: %v", got[0].Fields)
	}
	rows, err := back.Aggregate(ctx, store.TSAgg{
		Stream: "spend", From: now.Add(-time.Hour), To: now.Add(time.Hour),
		GroupBy: []string{"model"}, Sum: []string{"total_tokens"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 agg rows, got %d", len(rows))
	}

	// DeleteByQuery：删 before 之前的（只删第一条），第二条保留
	n, err := back.DeleteByQuery(ctx, "spend", now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 deleted, got %d", n)
	}
	left, _ := back.Query(ctx, store.TSQuery{Stream: "spend"})
	if len(left) != 1 {
		t.Errorf("expected 1 record left, got %d", len(left))
	}
}
