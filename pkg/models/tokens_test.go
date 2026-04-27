package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTokenEstimator_Heuristic(t *testing.T) {
	te := NewTokenEstimator()
	assert.Equal(t, "estimated", te.Source())

	// ~4 chars per token by default.
	got := te.Estimate("hello world") // 11 chars → ~2-3 tokens
	assert.InDelta(t, 2, got, 1)

	// Empty string.
	assert.Equal(t, 0, te.Estimate(""))

	// Single char → at least 1.
	assert.Equal(t, 1, te.Estimate("x"))
}

func TestTokenEstimator_Calibrate(t *testing.T) {
	te := NewTokenEstimator()

	// First calibration replaces the ratio entirely.
	te.Calibrate(100, 50, 10) // 50/100 = 0.5
	assert.Equal(t, "measured", te.Source())
	pt, ct := te.LastUsage()
	assert.Equal(t, 50, pt)
	assert.Equal(t, 10, ct)

	// After calibration, 100 chars → ~50 tokens.
	got := te.Estimate("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx") // 100 chars
	assert.InDelta(t, 50, got, 5)

	// Second calibration uses EMA.
	te.Calibrate(200, 80, 20) // observed: 80/200 = 0.4, EMA: 0.3*0.4 + 0.7*0.5 = 0.47
	got2 := te.Estimate("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	assert.InDelta(t, 47, got2, 5)
}

func TestTokenEstimator_NoZeroDivision(t *testing.T) {
	te := NewTokenEstimator()

	// Zero chars or tokens should not panic or change ratio.
	te.Calibrate(0, 0, 0)
	assert.Equal(t, "estimated", te.Source())

	te.Calibrate(100, 0, 0)
	assert.Equal(t, "estimated", te.Source())
}
