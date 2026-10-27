//go:build integration && load

package postgres_test

import (
	"testing"
	"time"
)

func TestLoadPercentileStatistics(t *testing.T) {
	values := []time.Duration{100 * time.Millisecond, time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	got := summarizeLatencies(values)
	if got.Count != 4 || got.P50MS != 10 || got.P95MS != 100 || got.P99MS != 100 {
		t.Fatalf("percentiles=%+v", got)
	}
	if values[0] != 100*time.Millisecond {
		t.Fatal("statistics changed caller-owned observations")
	}
	if empty := summarizeLatencies(nil); empty.Count != 0 || empty.P95MS != 0 {
		t.Fatalf("empty statistics=%+v", empty)
	}
}

func TestLoadResultClassifiesExpectedConflictSeparately(t *testing.T) {
	var result loadObservations
	result.record(201, "", time.Millisecond)
	result.record(409, "insufficient_available_stock", 2*time.Millisecond)
	result.record(503, "database_busy", 3*time.Millisecond)
	summary := result.summary(time.Second)
	if summary.Successes != 1 || summary.Conflicts != 1 || summary.TechnicalErrors != 1 || summary.RequestsPerSecond != 3 {
		t.Fatalf("summary=%+v", summary)
	}
}
