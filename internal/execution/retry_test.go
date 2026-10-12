package execution

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()
	p.Random = func() float64 { return 0.5 }
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{1, 2500 * time.Millisecond}, {2, 5 * time.Second}, {3, 10 * time.Second}, {20, 450 * time.Second},
	} {
		if got := p.next(tc.failures, 0); got != tc.want {
			t.Fatalf("failure %d: got %s want %s", tc.failures, got, tc.want)
		}
	}
	if got := p.next(1, 31*time.Second); got != 31*time.Second {
		t.Fatal(got)
	}
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if p.exhausted(now, now.Add(-time.Minute), 12) {
		t.Fatal("three full CRM attempts exhausted the retry budget during a brief outage")
	}
	if p.exhausted(now, now.Add(-23*time.Hour), 119) {
		t.Fatal("premature exhaustion")
	}
	if !p.exhausted(now, now.Add(-23*time.Hour), 120) {
		t.Fatal("provider budget ignored")
	}
	if !p.exhausted(now, now.Add(-24*time.Hour), 0) {
		t.Fatal("age budget ignored")
	}
}

func TestProviderRetryAfterLowerBound(t *testing.T) {
	if got := retryAfterDelay("17"); got != 17*time.Second {
		t.Fatalf("seconds Retry-After=%s", got)
	}
	if got := retryAfterDelay("99999"); got != time.Hour {
		t.Fatalf("unbounded Retry-After=%s", got)
	}
	if got := retryAfterDelay("garbage"); got != 0 {
		t.Fatalf("invalid Retry-After=%s", got)
	}
	date := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfterDelay(date); got < time.Second || got > 4*time.Second {
		t.Fatalf("date Retry-After=%s", got)
	}
}
