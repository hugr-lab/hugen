package models

import "sync"

// TokenEstimator tracks context token usage with a heuristic estimate
// that calibrates itself from real LLM usage metadata via EMA. Lives
// here rather than in pkg/agent because the compactor (BeforeModelCallback)
// and the memory-hint injector (InstructionProvider decorator) both
// need to read the same calibrated estimate.
type TokenEstimator struct {
	mu    sync.RWMutex
	ratio float64 // chars-to-tokens ratio (calibrated via EMA)
	alpha float64 // EMA smoothing factor

	lastPromptTokens     int
	lastCompletionTokens int
	calibrated           bool
}

// NewTokenEstimator creates a TokenEstimator with a default heuristic ratio.
// The default ratio of 0.25 (≈ 4 chars per token) is a good starting point
// for English text; it calibrates after the first LLM response.
func NewTokenEstimator() *TokenEstimator {
	return &TokenEstimator{
		ratio: 0.25, // 1 token ≈ 4 chars
		alpha: 0.3,  // EMA smoothing: 30% weight to new observation
	}
}

// Estimate returns an approximate token count for the given text.
func (t *TokenEstimator) Estimate(text string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := int(float64(len(text)) * t.ratio)
	if n == 0 && len(text) > 0 {
		return 1
	}
	return n
}

// Calibrate updates the ratio from real LLM usage metadata.
// promptChars is the approximate character count of the prompt that produced
// promptTokens actual tokens.
func (t *TokenEstimator) Calibrate(promptChars, promptTokens, completionTokens int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastPromptTokens = promptTokens
	t.lastCompletionTokens = completionTokens

	if promptChars > 0 && promptTokens > 0 {
		observed := float64(promptTokens) / float64(promptChars)
		if t.calibrated {
			t.ratio = t.alpha*observed + (1-t.alpha)*t.ratio
		} else {
			t.ratio = observed
			t.calibrated = true
		}
	}
}

// Source returns "measured" if the estimator has been calibrated, "estimated" otherwise.
func (t *TokenEstimator) Source() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.calibrated {
		return "measured"
	}
	return "estimated"
}

// LastUsage returns the most recent LLM usage metadata.
func (t *TokenEstimator) LastUsage() (promptTokens, completionTokens int) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastPromptTokens, t.lastCompletionTokens
}
