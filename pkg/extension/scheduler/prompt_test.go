package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func TestAdvertiseSystemPrompt_NonCronReturnsEmpty(t *testing.T) {
	ext := newExt(t)
	state := newFakeState("ses-root")
	// No SchedulerFireStateKey stamp.
	if got := ext.AdvertiseSystemPrompt(context.Background(), state); got != "" {
		t.Errorf("non-cron session must skip prompt; got %q", got)
	}
}

func TestAdvertiseSystemPrompt_CronSessionEmitsContract(t *testing.T) {
	resetCronPromptCacheForTest()
	defer resetCronPromptCacheForTest()

	ext := newExt(t)
	state := newFakeState("ses-cron-1")
	state.SetValue(protocol.SchedulerFireStateKey, &protocol.FireContext{
		TaskID:    "tsk_dash_eu",
		FireSeq:   2,
		PlannedAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		Goal:      "summarise EU dashboard",
		Inputs: map[string]any{
			"region":   "EU",
			"language": "en",
		},
		AllowedTools: []string{"hugr:query", "notepad:append"},
	})

	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if out == "" {
		t.Fatal("cron session must produce a prompt block")
	}
	wantFragments := []string{
		"[Cron fire]",
		"tsk_dash_eu",
		"fire_seq: 2",
		"2026-06-01 09:00:00 UTC",
		"region: EU",
		"language: en",
		"hugr:query",
		"notepad:append",
		"HEADLESS",
		"Do NOT call `session:inquire`",
		"denied_no_operator",
	}
	for _, want := range wantFragments {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing fragment %q\n— full output —\n%s", want, out)
		}
	}
	if strings.Contains(out, "Previous fire") {
		t.Errorf("first fire should not render PrevFire block; got:\n%s", out)
	}
}

func TestAdvertiseSystemPrompt_PrevFireRendered(t *testing.T) {
	resetCronPromptCacheForTest()
	defer resetCronPromptCacheForTest()

	ext := newExt(t)
	state := newFakeState("ses-cron-2")
	state.SetValue(protocol.SchedulerFireStateKey, &protocol.FireContext{
		TaskID:    "tsk_with_prev",
		FireSeq:   5,
		PlannedAt: time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC),
		AllowedTools: []string{"hugr:query"},
		PrevFire: &protocol.PrevFireOutcome{
			FiredAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
			Summary: "EU dashboard summarised, 3 anomalies flagged",
			Body:    "Region EU saw a 12% conversion drop on 2026-05-30…",
		},
	})

	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(out, "Previous fire") {
		t.Errorf("PrevFire block missing from prompt:\n%s", out)
	}
	if !strings.Contains(out, "3 anomalies flagged") {
		t.Errorf("PrevFire summary missing:\n%s", out)
	}
}

func TestAdvertiseSystemPrompt_EmptyAllowedToolsReadable(t *testing.T) {
	resetCronPromptCacheForTest()
	defer resetCronPromptCacheForTest()

	ext := newExt(t)
	state := newFakeState("ses-cron-readonly")
	state.SetValue(protocol.SchedulerFireStateKey, &protocol.FireContext{
		TaskID:       "tsk_readonly",
		FireSeq:      1,
		AllowedTools: nil,
	})

	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(out, "this fire is read-only") {
		t.Errorf("read-only marker missing for empty allow-list:\n%s", out)
	}
}
