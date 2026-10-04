package handler

import "testing"

func TestLimitObservationValueSanitizesUTF8(t *testing.T) {
	got := limitObservationValue(string([]byte{'o', 'k', 0xff, 'x'}))
	if got != "ok\ufffdx" {
		t.Fatalf("unexpected sanitized value: %q", got)
	}
}
