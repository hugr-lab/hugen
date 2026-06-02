package models

import (
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
)

// TestInheritDeadlines pins that a named route treats the top-level
// models.{first,inter}_batch_deadline as the base it overrides — the
// fix for a route silently falling to the package default and ignoring
// the operator's global setting (a slow reasoning-route backend stalling
// at the hardcoded 2m though the operator raised the global to 4m).
func TestInheritDeadlines(t *testing.T) {
	base := config.ModelsConfig{
		FirstBatchDeadline: 3 * time.Minute,
		InterBatchDeadline: 4 * time.Minute,
	}

	t.Run("unset route inherits the top-level base", func(t *testing.T) {
		got := inheritDeadlines(config.ModelsConfig{}, base)
		if got.FirstBatchDeadline != 3*time.Minute {
			t.Errorf("FirstBatchDeadline = %v, want inherited 3m", got.FirstBatchDeadline)
		}
		if got.InterBatchDeadline != 4*time.Minute {
			t.Errorf("InterBatchDeadline = %v, want inherited 4m", got.InterBatchDeadline)
		}
	})

	t.Run("explicit route value overrides the base", func(t *testing.T) {
		route := config.ModelsConfig{
			FirstBatchDeadline: 90 * time.Second,
			InterBatchDeadline: 6 * time.Minute,
		}
		got := inheritDeadlines(route, base)
		if got.FirstBatchDeadline != 90*time.Second || got.InterBatchDeadline != 6*time.Minute {
			t.Errorf("route values must be kept, got first=%v inter=%v",
				got.FirstBatchDeadline, got.InterBatchDeadline)
		}
	})

	t.Run("explicit negative (disable) is preserved, not inherited", func(t *testing.T) {
		route := config.ModelsConfig{FirstBatchDeadline: -1, InterBatchDeadline: -1}
		got := inheritDeadlines(route, base)
		if got.FirstBatchDeadline != -1 || got.InterBatchDeadline != -1 {
			t.Errorf("explicit disable (-1) must survive, got first=%v inter=%v",
				got.FirstBatchDeadline, got.InterBatchDeadline)
		}
	})

	t.Run("both unset stays zero so buildOptsFor applies the package default", func(t *testing.T) {
		got := inheritDeadlines(config.ModelsConfig{}, config.ModelsConfig{})
		if got.FirstBatchDeadline != 0 || got.InterBatchDeadline != 0 {
			t.Errorf("both-unset must stay zero, got first=%v inter=%v",
				got.FirstBatchDeadline, got.InterBatchDeadline)
		}
	})
}
