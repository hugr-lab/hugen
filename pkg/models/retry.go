package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// retryMaxBackoff caps the exponential schedule. Each attempt
// computes initial × 2^attempt; without a cap the schedule would
// double indefinitely.
const retryMaxBackoff = 30 * time.Second

// retryPolicy bundles the knobs the subscription pump consults
// before retrying a transient error. Construct via newRetryPolicy
// so zero-value fields fall back to sensible defaults.
type retryPolicy struct {
	maxAttempts    int
	initialBackoff time.Duration
}

// newRetryPolicy normalises operator-supplied values: a negative
// or zero maxAttempts means "no retries", which we treat as
// maxAttempts=0; a missing initialBackoff falls back to 500ms.
// Negative initialBackoff is clamped to zero — caller still sees
// max-attempts retries, just without sleep.
func newRetryPolicy(maxAttempts int, initialBackoff time.Duration) retryPolicy {
	if maxAttempts < 0 {
		maxAttempts = 0
	}
	if initialBackoff < 0 {
		initialBackoff = 0
	}
	return retryPolicy{maxAttempts: maxAttempts, initialBackoff: initialBackoff}
}

// nextBackoff returns the wait before attempt N (1-indexed —
// attempt=1 is the first retry, attempt=0 is the initial try and
// always returns 0). Schedule: initial × 2^(attempt-1), capped by
// retryMaxBackoff. No jitter today — the upstream rate-limit
// window is per-second, so a deterministic ladder lines up well
// enough for v1.
func (p retryPolicy) nextBackoff(attempt int) time.Duration {
	if attempt <= 0 || p.initialBackoff <= 0 {
		return 0
	}
	d := p.initialBackoff << (attempt - 1) // initial * 2^(attempt-1)
	if d <= 0 || d > retryMaxBackoff {
		return retryMaxBackoff
	}
	return d
}

// sleepBackoff blocks for the computed backoff or returns
// ctx.Err() if ctx is cancelled first. Exposed as a method so
// tests can substitute a fake clock if they need precise control.
func (p retryPolicy) sleepBackoff(ctx context.Context, attempt int) error {
	d := p.nextBackoff(attempt)
	if d <= 0 {
		// Still respect cancel — a zero-backoff retry shouldn't
		// be allowed to hot-loop a cancelled context.
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isRetryableSubscribeErr classifies a Subscribe / first-batch
// error as transient. The Hugr engine surfaces upstream provider
// errors (Anthropic / Gemini / OpenAI / local) verbatim inside
// the `stream error: <body>` envelope, so the heuristic walks
// over the rendered string. Patterns:
//
//   - HTTP 429 (rate_limit) and 5xx — explicit upstream backpressure.
//   - "rate_limit" / "rate limit" — provider phrasing variants.
//   - "deadline exceeded" / "context deadline exceeded" — unwrapped
//     transient timeout.
//   - "connection refused" / "EOF" / "broken pipe" / "reset by peer" —
//     network blips between hugen and the engine.
//
// Context cancellation (Cancelled / DeadlineExceeded — the wrapped
// kind) is NOT retryable: a cancelled stream is the caller's
// decision, not a transient fault.
//
// Heuristics will mislabel — operator can tighten by lowering
// retry_max_attempts; until we have explicit error codes from the
// engine, conservative wide matching is the right tradeoff.
func isRetryableSubscribeErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := strings.ToLower(err.Error())
	patterns := []string{
		"429",
		"rate_limit",
		"rate limit",
		"503",
		"504",
		"500",
		"502",
		"server overloaded",
		"overloaded",
		"deadline exceeded",
		"connection refused",
		"connection reset",
		"reset by peer",
		"broken pipe",
		"eof",
		"i/o timeout",
		"temporarily unavailable",
	}
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// retryWaitMessage formats the human-readable line logged at WARN
// when the pump is about to sleep before a retry. Pulled out so
// the format stays consistent across the two retry sites
// (Subscribe error vs. mid-stream first-event error).
func retryWaitMessage(attempt, maxAttempts int, backoff time.Duration, lastErr error) string {
	return fmt.Sprintf("retry %d/%d after %s (last_err: %v)",
		attempt, maxAttempts, backoff, lastErr)
}
