package models

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestNewRetryPolicy_NormalisesNegatives(t *testing.T) {
	p := newRetryPolicy(-3, -10*time.Second)
	if p.maxAttempts != 0 {
		t.Errorf("maxAttempts = %d, want 0 for negative input", p.maxAttempts)
	}
	if p.initialBackoff != 0 {
		t.Errorf("initialBackoff = %v, want 0 for negative input", p.initialBackoff)
	}
}

func TestNextBackoff_Schedule(t *testing.T) {
	p := newRetryPolicy(10, 500*time.Millisecond)
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 0},                       // initial try, no wait
		{1, 500 * time.Millisecond},  // first retry — 500ms × 2^0
		{2, 1 * time.Second},         // 500ms × 2^1
		{3, 2 * time.Second},         // 500ms × 2^2
		{4, 4 * time.Second},         // 500ms × 2^3
		{5, 8 * time.Second},         // 500ms × 2^4
		{6, 16 * time.Second},        // 500ms × 2^5
		{7, retryMaxBackoff},         // 500ms × 2^6 = 32s, capped at 30s
		{20, retryMaxBackoff},        // saturated
	}
	for _, c := range cases {
		got := p.nextBackoff(c.attempt)
		if got != c.want {
			t.Errorf("nextBackoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestNextBackoff_ZeroInitial(t *testing.T) {
	p := newRetryPolicy(5, 0)
	for _, attempt := range []int{0, 1, 5, 10} {
		if got := p.nextBackoff(attempt); got != 0 {
			t.Errorf("nextBackoff(%d) with zero initial = %v, want 0", attempt, got)
		}
	}
}

func TestSleepBackoff_RespectsCancel(t *testing.T) {
	p := newRetryPolicy(3, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := p.sleepBackoff(ctx, 1)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("sleepBackoff returned nil err on cancelled ctx, want ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("sleepBackoff err = %v, want context.Canceled", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("sleepBackoff returned after %v, expected near-immediate cancel", elapsed)
	}
}

func TestSleepBackoff_ZeroAttempt(t *testing.T) {
	p := newRetryPolicy(3, 1*time.Hour)
	start := time.Now()
	if err := p.sleepBackoff(context.Background(), 0); err != nil {
		t.Fatalf("sleepBackoff(0) err = %v, want nil", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("sleepBackoff(0) waited too long: %v", time.Since(start))
	}
}

func TestIsRetryableSubscribeErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		{
			name: "anthropic 429",
			err:  fmt.Errorf("Anthropic API error (status 429): rate_limit_error"),
			want: true,
		},
		{
			name: "rate limit human-readable",
			err:  fmt.Errorf("upstream returned: rate limit exceeded, retry after 60s"),
			want: true,
		},
		{
			name: "openai server overloaded",
			err:  fmt.Errorf("status 503: server overloaded"),
			want: true,
		},
		{
			name: "gateway timeout",
			err:  fmt.Errorf("status 504: gateway timeout"),
			want: true,
		},
		{
			name: "connection refused",
			err:  fmt.Errorf("dial tcp 127.0.0.1:18070: connection refused"),
			want: true,
		},
		{
			name: "EOF on subscription",
			err:  fmt.Errorf("read: EOF"),
			want: true,
		},
		{
			name: "deadline exceeded text",
			err:  fmt.Errorf("read: context deadline exceeded after 30s"),
			want: true,
		},
		{
			name: "invalid auth (not retryable)",
			err:  fmt.Errorf("status 401: unauthorized"),
			want: false,
		},
		{
			name: "model not found (not retryable)",
			err:  fmt.Errorf("status 404: model 'gemma-omega' not found"),
			want: false,
		},
		{
			name: "schema mismatch (not retryable)",
			err:  fmt.Errorf("hugrmodel: convert tools: invalid schema"),
			want: false,
		},
	}
	for _, c := range cases {
		got := isRetryableSubscribeErr(c.err)
		if got != c.want {
			t.Errorf("%s: isRetryableSubscribeErr(%v) = %v, want %v",
				c.name, c.err, got, c.want)
		}
	}
}
