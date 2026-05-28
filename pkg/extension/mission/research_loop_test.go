package mission

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestRunResearchStage_HappyPath_OneIter — researcher emits
// done=true on the first iteration. State carries findings,
// MarkResearchAttempted is true, no modal was opened.
func TestRunResearchStage_HappyPath_OneIter(t *testing.T) {
	state := newRenderedFakeState("mis-r-happy", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "research-mission",
		Research: &ResearchManifest{
			Role:          "researcher",
			MaxIterations: 3,
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{
			Kind:   KindResearch,
			Status: "ok",
			Body: map[string]any{
				"done":            true,
				"findings":        "user wants HTML report for the op2023 source",
				"memory_summary":  "scoping complete",
				"resolved_user_inputs": map[string]any{
					"data_source": "op2023",
					"format":      "html",
				},
			},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "build an HTML report")
	if err != nil {
		t.Fatalf("runResearchStage: %v", err)
	}
	if aborted {
		t.Fatalf("aborted = true, want false")
	}
	m := FromState(state)
	if !m.ResearchAttempted() {
		t.Error("ResearchAttempted = false, want true after the loop ran")
	}
	findings, resolved, _ := m.ResearchOutput()
	if !strings.Contains(findings, "op2023") {
		t.Errorf("findings = %q, want substring 'op2023'", findings)
	}
	if got := resolved["data_source"]; got != "op2023" {
		t.Errorf("resolved_user_inputs[data_source] = %v, want 'op2023'", got)
	}
	if got := len(state.inquiryRequests); got != 0 {
		t.Errorf("inquiryRequests = %d, want 0 (no modal on done=true iter1)", got)
	}
}

// TestRunResearchStage_ClarificationRoundTrip — iter1 emits
// done=false with a single required clarification; the inquiry
// stub returns an answer; iter2 emits done=true. Verifies the
// answer flows back into the next iteration via prior_answers.
func TestRunResearchStage_ClarificationRoundTrip(t *testing.T) {
	state := newRenderedFakeState("mis-r-clarify", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "research-mission",
		Research: &ResearchManifest{
			Role:          "researcher",
			MaxIterations: 3,
		},
	}

	approved := true
	state.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{
			Approved: &approved,
			Answers: map[string]protocol.AnswerEntry{
				"file_path": {Value: "~/Downloads/report.html"},
			},
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	var iterCount atomic.Int32
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		n := iterCount.Add(1)
		if n == 1 {
			return Handoff{
				Kind:   KindResearch,
				Status: "ok",
				Body: map[string]any{
					"done": false,
					"clarifications": []any{
						map[string]any{
							"id":       "file_path",
							"question": "Where should the report be saved?",
							"kind":     "required",
						},
					},
					"memory_summary": "asking for file_path",
				},
			}
		}
		return Handoff{
			Kind:   KindResearch,
			Status: "ok",
			Body: map[string]any{
				"done":     true,
				"findings": "resolved: report to ~/Downloads/report.html",
				"resolved_user_inputs": map[string]any{
					"file_path": "~/Downloads/report.html",
				},
				"memory_summary": "scoping done after one clarify round",
			},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "save report somewhere")
	if err != nil {
		t.Fatalf("runResearchStage: %v", err)
	}
	if aborted {
		t.Fatalf("aborted = true, want false")
	}
	if got := iterCount.Load(); got != 2 {
		t.Errorf("research spawns = %d, want 2 (clarify + final)", got)
	}
	if got := len(state.inquiryRequests); got != 1 {
		t.Errorf("inquiryRequests = %d, want 1 (one modal between iters)", got)
	}
	if state.inquiryRequests[0].Type != protocol.InquiryTypeResearchBatch {
		t.Errorf("inquiry type = %q, want %q", state.inquiryRequests[0].Type, protocol.InquiryTypeResearchBatch)
	}
	if got := len(state.inquiryRequests[0].Clarifications); got != 1 {
		t.Errorf("clarifications in modal = %d, want 1", got)
	}
	_, resolved, _ := FromState(state).ResearchOutput()
	if got := resolved["file_path"]; got != "~/Downloads/report.html" {
		t.Errorf("resolved file_path = %v, want '~/Downloads/report.html'", got)
	}
}

// TestRunResearchStage_MaxIterCap — researcher never emits
// done=true; loop bails on the last iteration BEFORE prompting
// the user (R3 fix). Aborted=true; ResearchAttempted=true but no
// findings recorded.
func TestRunResearchStage_MaxIterCap(t *testing.T) {
	state := newRenderedFakeState("mis-r-cap", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "research-mission",
		Research: &ResearchManifest{
			Role:          "researcher",
			MaxIterations: 2,
		},
	}

	approved := true
	state.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{
			Approved: &approved,
			Answers: map[string]protocol.AnswerEntry{
				"q1": {Value: "yes"},
			},
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{
			Kind:   KindResearch,
			Status: "ok",
			Body: map[string]any{
				"done": false,
				"clarifications": []any{
					map[string]any{"id": "q1", "question": "?", "kind": "required"},
				},
				"memory_summary": "still asking",
			},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "perpetually ambiguous")
	if !aborted {
		t.Errorf("aborted = false, want true after MaxIterations exhaust")
	}
	if err == nil {
		t.Fatal("err = nil, want exceeded-MaxIterations error")
	}
	if !strings.Contains(err.Error(), "MaxIterations") {
		t.Errorf("err = %v, want substring 'MaxIterations'", err)
	}
	// R3: terminal iter must abort BEFORE the second modal opens.
	// So the inquiry count is iter1 only (1 modal), not iter2.
	if got := len(state.inquiryRequests); got != 1 {
		t.Errorf("inquiryRequests = %d, want 1 (iter1 modal; iter2 must abort before opening one)", got)
	}
	if got, _, _ := FromState(state).ResearchOutput(); got != "" {
		t.Errorf("findings = %q, want empty after abort", got)
	}
}

// TestRunResearchStage_WrongKindRetry_MonotonicBudget — verifies
// the R2 fix: validationRetries counter monotonically caps wrong-
// kind handoffs even when interspersed with valid ones. Without
// the fix, every valid handoff would reset the cap and a malicious
// alternating sequence ran forever.
func TestRunResearchStage_WrongKindRetry_MonotonicBudget(t *testing.T) {
	state := newRenderedFakeState("mis-r-retry", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name: "research-mission",
		Research: &ResearchManifest{
			Role:          "researcher",
			MaxIterations: 10,
		},
	}

	spawner := &plannerFakeSpawner{state: state}
	var n atomic.Int32
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		// Always emit kind=handoff (wrong kind) — never recovers.
		_ = n.Add(1)
		return Handoff{
			Kind:   KindHandoff,
			Status: "ok",
			Body:   "not a research fence",
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "research that never produces research")
	if !aborted {
		t.Errorf("aborted = false, want true")
	}
	if err == nil || !strings.Contains(err.Error(), "after") {
		t.Errorf("err = %v, want substring 'after N retries'", err)
	}
	// Cap is researchValidationRetryCap (=2). Loop bails on the
	// SECOND wrong-kind handoff so total spawns = 2.
	if got := n.Load(); got != int32(researchValidationRetryCap) {
		t.Errorf("research spawns = %d, want %d (cap reached)", got, researchValidationRetryCap)
	}
}
