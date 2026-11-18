package store

import (
	"testing"
	"time"
)

func TestAggregateLocationAlignsLocalDay(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	recs := []*TSRecord{
		{Ts: time.Date(2026, 11, 17, 0, 30, 0, 0, time.UTC), Stream: "spend", Fields: map[string]any{"total_tokens": 1.0}},
		{Ts: time.Date(2026, 11, 17, 15, 30, 0, 0, time.UTC), Stream: "spend", Fields: map[string]any{"total_tokens": 2.0}},
		{Ts: time.Date(2026, 11, 17, 16, 30, 0, 0, time.UTC), Stream: "spend", Fields: map[string]any{"total_tokens": 4.0}},
	}
	rows := AggregateRecords(recs, TSAgg{Stream: "spend", Bucket: 24 * time.Hour, Location: loc, Sum: []string{"total_tokens"}})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	// Local Nov 17 is 2026-11-16T16:00Z..2026-11-17T16:00Z; first two records.
	if !rows[0].Bucket.Equal(time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)) || rows[0].Count != 2 || rows[0].Sums["total_tokens"] != 3 {
		t.Fatalf("day1 = %+v", rows[0])
	}
	if !rows[1].Bucket.Equal(time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)) || rows[1].Count != 1 || rows[1].Sums["total_tokens"] != 4 {
		t.Fatalf("day2 = %+v", rows[1])
	}
}
