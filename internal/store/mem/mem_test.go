package mem

import (
	"context"
	"testing"
	"time"

	"lmgateway/internal/store"
)

func TestJSONCRUD(t *testing.T) {
	js := NewJSON()
	ctx := context.Background()

	if _, err := js.Get(ctx, "provider/x"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	d, err := js.Put(ctx, "provider/openai", map[string]any{"name": "openai"})
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if d.Version != 1 {
		t.Errorf("version should be 1, got %d", d.Version)
	}
	// upsert bumps version
	d, err = js.Put(ctx, "provider/openai", map[string]any{"name": "openai", "model": "gpt"})
	if err != nil {
		t.Fatalf("Put 2 failed: %v", err)
	}
	if d.Version != 2 {
		t.Errorf("version should be 2, got %d", d.Version)
	}
	// optimistic lock
	if _, err := js.Update(ctx, "provider/openai", map[string]any{}, 1); err != store.ErrConflict {
		t.Errorf("stale version should conflict, got %v", err)
	}
	if _, err := js.Update(ctx, "provider/openai", map[string]any{"name": "x"}, 2); err != nil {
		t.Errorf("correct version should pass: %v", err)
	}
	// list by prefix
	if _, err := js.Put(ctx, "provider/azure", map[string]any{"name": "azure"}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Put(ctx, "rule/1", map[string]any{"phase": "req"}); err != nil {
		t.Fatal(err)
	}
	provs, err := js.List(ctx, "provider/")
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 2 {
		t.Errorf("expected 2 providers, got %d", len(provs))
	}
}

func TestJSONWatch(t *testing.T) {
	js := NewJSON()
	ctx := context.Background()
	got := make(chan store.JSONChange, 4)
	unsub, err := js.Watch(ctx, "provider/", func(c store.JSONChange) { got <- c })
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	if _, err := js.Put(ctx, "provider/openai", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		if c.Prefix != "provider/" {
			t.Errorf("prefix mismatch: %q", c.Prefix)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not fire on local put")
	}
}

func TestTSAggregate(t *testing.T) {
	back := NewTSBackend()
	now := time.Now().UTC()
	back.AppendBatch(context.Background(), []*store.TSRecord{
		{Ts: now, Stream: "spend", Tags: map[string]string{"model": "a"}, Fields: map[string]any{"total_tokens": float64(100), "cost": 1.0}},
		{Ts: now.Add(time.Minute), Stream: "spend", Tags: map[string]string{"model": "b"}, Fields: map[string]any{"total_tokens": float64(200), "cost": 2.0}},
		{Ts: now.Add(2 * time.Minute), Stream: "spend", Tags: map[string]string{"model": "a"}, Fields: map[string]any{"total_tokens": float64(50), "cost": 0.5}},
	})
	rows, err := back.Aggregate(context.Background(), store.TSAgg{
		Stream: "spend", From: now.Add(-time.Hour), To: now.Add(time.Hour),
		GroupBy: []string{"model"}, Sum: []string{"total_tokens", "cost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]store.TSAggRow{}
	for _, r := range rows {
		byModel[r.Tags["model"]] = r
	}
	if byModel["a"].Sums["total_tokens"] != 150 || byModel["a"].Sums["cost"] != 1.5 {
		t.Errorf("model a agg wrong: %+v", byModel["a"])
	}
	if byModel["a"].Count != 2 {
		t.Errorf("model a count should be 2, got %d", byModel["a"].Count)
	}
	if byModel["b"].Sums["total_tokens"] != 200 {
		t.Errorf("model b agg wrong: %+v", byModel["b"])
	}
}

// buffered TSStore：Append 入队 → Flush 落盘 → Query 可见
func TestBufferedTSFlush(t *testing.T) {
	back := NewTSBackend()
	ts := store.NewTS(back, store.BufferedOpts{BatchSize: 100, Interval: time.Hour})
	defer ts.Close()

	for i := 0; i < 5; i++ {
		if err := ts.Append(context.Background(), &store.TSRecord{
			Ts: time.Now().UTC(), Stream: "spend",
			Tags: map[string]string{"model": "m"}, Fields: map[string]any{"total_tokens": float64(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ts.Flush(ctx); err != nil {
		t.Fatalf("flush failed: %v", err)
	}
	recs, err := ts.Query(ctx, store.TSQuery{Stream: "spend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Errorf("expected 5 records after flush, got %d", len(recs))
	}
}
