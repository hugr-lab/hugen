package scheduler_test

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension/scheduler"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
)

func TestReapStuckTaskRuns_StubReportsNotYet(t *testing.T) {
	t.Parallel()
	fn := scheduler.ReapStuckTaskRuns()
	out, err := fn(context.Background(), runner.FireMeta{Name: "task_runs_reap_stuck"})
	if err != nil {
		t.Fatalf("stub fn returned error: %v", err)
	}
	if out.Reason != "not_yet_present" {
		t.Fatalf("expected reason=not_yet_present, got %q", out.Reason)
	}
	if out.Summary == "" {
		t.Fatalf("expected non-empty summary")
	}
}
