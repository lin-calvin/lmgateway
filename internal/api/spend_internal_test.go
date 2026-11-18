package api

import (
	"testing"
	"time"
)

func TestTokenWindowUsesLocalCalendarDays(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC) // 22:00 local
	from, to, err := tokenWindow("7d", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	// Seven local calendar days including today: Nov 12 00:00 +08.
	wantFrom := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)
	if !from.Equal(wantFrom) {
		t.Fatalf("from = %s, want %s", from, wantFrom)
	}
	if !to.Equal(now) {
		t.Fatalf("to = %s, want %s", to, now)
	}
}

func TestLocalBucketStart(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-11-17 08:30 local, i.e. 00:30 UTC.
	at := time.Date(2026, 11, 17, 0, 30, 0, 0, time.UTC)
	day := localBucketStart(at, 24*time.Hour, loc)
	wantDay := time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC) // local midnight
	if !day.Equal(wantDay) {
		t.Fatalf("day bucket = %s, want %s", day, wantDay)
	}
	hour := localBucketStart(at, time.Hour, loc)
	wantHour := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC) // 08:00 local
	if !hour.Equal(wantHour) {
		t.Fatalf("hour bucket = %s, want %s", hour, wantHour)
	}
	if next := nextLocalBucket(day, 24*time.Hour, loc); !next.Equal(wantDay.Add(24 * time.Hour)) {
		t.Fatalf("next day = %s", next)
	}
}
