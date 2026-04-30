package main

import (
	"testing"
	"time"
)

func TestEffectiveDeadline(t *testing.T) {
	cfg := timeoutConfig{DefaultMS: 1_000, MaxMS: 5_000}

	cases := []struct {
		in   int
		want time.Duration
		why  string
	}{
		{0, 1 * time.Second, "zero falls back to default"},
		{-7, 1 * time.Second, "negative falls back to default"},
		{500, 500 * time.Millisecond, "below ceiling honoured"},
		{4_999, 4_999 * time.Millisecond, "just below ceiling honoured"},
		{5_000, 5 * time.Second, "at-ceiling honoured"},
		{10_000, 5 * time.Second, "above ceiling clamped down"},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			got := cfg.effectiveDeadline(tc.in)
			if got != tc.want {
				t.Fatalf("in=%d got=%v want=%v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIntEnv(t *testing.T) {
	t.Setenv("X_INT_OK", "42")
	t.Setenv("X_INT_BAD", "notanumber")
	t.Setenv("X_INT_NEG", "-1")
	t.Setenv("X_INT_ZERO", "0")
	if got := intEnv("X_INT_OK", 7); got != 42 {
		t.Fatalf("ok: got %d", got)
	}
	if got := intEnv("X_INT_BAD", 7); got != 7 {
		t.Fatalf("bad: got %d want fallback", got)
	}
	if got := intEnv("X_INT_NEG", 7); got != 7 {
		t.Fatalf("neg: got %d want fallback (>0 only)", got)
	}
	if got := intEnv("X_INT_ZERO", 7); got != 7 {
		t.Fatalf("zero: got %d want fallback (>0 only)", got)
	}
	if got := intEnv("X_INT_MISSING", 7); got != 7 {
		t.Fatalf("missing: got %d want fallback", got)
	}
}
