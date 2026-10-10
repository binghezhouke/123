package panapi

import (
	"math"
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfterAcceptsSecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
		valid bool
	}{
		{name: "seconds", value: "5", want: 5 * time.Second, valid: true},
		{name: "seconds with padding", value: "  12  ", want: 12 * time.Second, valid: true},
		{name: "zero seconds", value: "0", want: 0, valid: true},
		{name: "http date", value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second, valid: true},
		{name: "http date in the past", value: now.Add(-time.Minute).Format(http.TimeFormat), want: 0, valid: true},
		{name: "overflowing numeric", value: "999999999999999999999999", want: time.Duration(math.MaxInt64), valid: true},
		{name: "empty", value: "", valid: false},
		{name: "garbage", value: "soon", valid: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, valid := parseRetryAfter(tc.value, now)
			if valid != tc.valid {
				t.Fatalf("parseRetryAfter(%q) valid = %v, want %v", tc.value, valid, tc.valid)
			}
			if !valid {
				return
			}
			if diff := got - tc.want; diff > time.Second || diff < -time.Second {
				t.Fatalf("parseRetryAfter(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

func TestAPISchedulerCooldownIsBounded(t *testing.T) {
	clock := &testClock{current: time.Unix(0, 0)}
	s := newTestScheduler(clock)
	// A hostile or buggy Retry-After value must not stall the category.
	s.cooldown(categoryV2List, 24*time.Hour)
	s.mu.Lock()
	until := s.cool[categoryV2List]
	s.mu.Unlock()
	if got := until.Sub(clock.current); got != maxCategoryCooldown {
		t.Fatalf("cooldown = %s, want %s", got, maxCategoryCooldown)
	}
}
