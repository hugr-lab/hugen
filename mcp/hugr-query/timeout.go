package main

import (
	"os"
	"strconv"
	"time"
)

// timeoutConfig holds the per-call deadline knobs. Both are read
// from env at boot; LLM-supplied timeout_ms is clamped against
// MaxMS at dispatch time.
type timeoutConfig struct {
	DefaultMS int
	MaxMS     int
}

func loadTimeoutConfig() timeoutConfig {
	return timeoutConfig{
		DefaultMS: intEnv("HUGR_QUERY_TIMEOUT_MS", 3_600_000),     // 1h
		MaxMS:     intEnv("HUGR_QUERY_MAX_TIMEOUT_MS", 86_400_000), // 24h
	}
}

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// effectiveDeadline returns the duration to apply to a single call.
// Rules: zero or negative LLM input → DefaultMS; positive LLM input
// → min(input, MaxMS). MaxMS itself acts as the hard ceiling — the
// runtime contract says the LLM can never raise the budget above
// the operator's ceiling, only lower it.
func (c timeoutConfig) effectiveDeadline(llmTimeoutMS int) time.Duration {
	ms := c.DefaultMS
	if llmTimeoutMS > 0 {
		ms = llmTimeoutMS
	}
	if ms > c.MaxMS {
		ms = c.MaxMS
	}
	return time.Duration(ms) * time.Millisecond
}
