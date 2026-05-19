package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// synthesisWaveLabel is the synthetic wave name the runtime uses
// when synthesis.role is declared on the mission manifest. Kept
// distinct from operator-authored wave labels (prefixed with "_")
// so analysts inspecting the Plan can tell synthesis apart from
// PDCA waves at a glance.
const synthesisWaveLabel = "_synthesis"

// RunMission implements [extension.MissionAutoRunner]. Called by
// pkg/session.spawn_mission AFTER the mission session has been
// opened and its state initialisers fired. Phase A scope —
// kicks the inline-plan executor on a goroutine; returns nil
// immediately so spawn_mission's tool dispatch can continue.
//
// The mission session itself does NOT run an LLM supervisor in
// Phase A — the executor drives the entire mission. When the
// last wave (plus optional synthesis worker) completes,
// driveMission emits the mission's final AgentMessage; the
// parent's pump projects that as a SubagentResult and the parent
// fires SessionClose back at the mission, tearing it down.
//
// Errors during kickoff (unknown skill, no inline plan, no
// SessionSpawner on the mission state) surface synchronously;
// errors during wave execution are logged + recorded on the
// mission's PlanState (visible via StatusReporter and the event
// log).
func (e *Extension) RunMission(ctx context.Context, mission extension.SessionState, skill, goal string, inputs any) error {
	if mission == nil {
		return errors.New("mission: RunMission: mission state is nil")
	}
	if e.catalog == nil {
		return errors.New("mission: RunMission: no catalog wired")
	}
	manifest, err := e.catalog.LookupMission(ctx, skill)
	if err != nil {
		return fmt.Errorf("mission: RunMission: catalog lookup: %w", err)
	}
	if manifest == nil {
		return fmt.Errorf("mission: RunMission: skill %q is not a PDCA mission", skill)
	}
	if manifest.Plan.ExperimentalInline == nil || len(manifest.Plan.ExperimentalInline.Waves) == 0 {
		return fmt.Errorf("mission: RunMission: skill %q has no executable plan (Phase A requires plan.experimental_inline)", skill)
	}
	spawner, ok := mission.(extension.SessionSpawner)
	if !ok {
		return errors.New("mission: RunMission: mission state does not satisfy extension.SessionSpawner")
	}

	go e.driveMission(mission, spawner, *manifest, skill, goal, inputs)
	return nil
}

// driveMission is the goroutine body: runs every declared wave,
// optionally spawns a synthesis worker, then emits the mission's
// terminal AgentMessage so the parent's pump projects a
// SubagentResult and tears the mission session down. Each wave's
// completion is published as a mission:wave_complete ExtensionFrame
// on the mission session for observability / recovery.
func (e *Extension) driveMission(mission extension.SessionState, spawner extension.SessionSpawner, manifest MissionManifest, missionSkill, goal string, inputs any) {
	executor := NewExecutor(func(ctx context.Context, parent extension.SessionState, req SpawnRequest) (SpawnResult, error) {
		child, err := spawner.SpawnChild(ctx, extension.SpawnSpec{
			Name:       req.Name,
			Skill:      req.Skill,
			Role:       req.Role,
			Task:       req.Task,
			Inputs:     req.Inputs,
			RenderMode: req.RenderMode,
		})
		if err != nil {
			return SpawnResult{}, err
		}
		first := protocol.NewUserMessage(child.SessionID(), agentParticipant(mission, e.agentID), req.Task)
		settled := child.Submit(context.Background(), first)
		return SpawnResult{SessionID: child.SessionID(), Settled: settled}, nil
	}, e.logger)

	ctx := context.Background()
	_ = inputs
	_ = goal

	// Run every declared wave. Failure on any wave halts the
	// pipeline; the mission still terminates with whatever recap
	// the partial run can produce.
	aborted := false
	for _, waveDecl := range manifest.Plan.ExperimentalInline.Waves {
		status, _, err := executor.RunWave(ctx, mission, waveDecl, RunWaveOptions{})
		e.emitWaveComplete(mission, waveDecl.Label, status, err)
		if err != nil || status == WaveStatusFailed {
			e.logger.Warn("mission: driveMission: wave failed",
				"mission_session", mission.SessionID(),
				"wave", waveDecl.Label, "status", status, "err", err)
			aborted = true
			break
		}
	}

	// Synthesis worker (optional). Phase A treats it as a one-worker
	// "_synthesis" wave so it reuses the executor's spawn + handoff
	// pipeline. Its handoff body becomes the mission's terminal text.
	var synthText string
	if !aborted && manifest.Synthesis.Role != "" {
		text, err := e.runSynthesis(ctx, executor, mission, manifest.Synthesis.Role, missionSkill, goal)
		if err != nil {
			e.logger.Warn("mission: driveMission: synthesis failed",
				"mission_session", mission.SessionID(), "err", err)
		} else {
			synthText = text
		}
	}

	final := buildFinalText(mission, synthText, aborted)
	e.finishMission(ctx, mission, final)
}

// runSynthesis spawns a single worker under synthesisWaveLabel with
// the manifest's synthesis.role; its handoff body is the synthesis
// result. The synthesis worker loads its prompt + tools from the
// mission's own skill (same convention as a regular wave subagent).
func (e *Extension) runSynthesis(ctx context.Context, executor *Executor, mission extension.SessionState, role, missionSkill, goal string) (string, error) {
	task, err := buildSynthesisTask(mission, goal)
	if err != nil {
		return "", fmt.Errorf("synthesis: build task: %w", err)
	}
	wave := Wave{
		Label: synthesisWaveLabel,
		Subagents: []SubagentSpec{{
			Name:  "synthesizer",
			Skill: missionSkill,
			Role:  role,
			Task:  task,
		}},
	}
	status, _, err := executor.RunWave(ctx, mission, wave, RunWaveOptions{})
	e.emitWaveComplete(mission, synthesisWaveLabel, status, err)
	if err != nil {
		return "", err
	}
	if status == WaveStatusFailed {
		return "", fmt.Errorf("synthesis wave failed")
	}
	m := FromState(mission)
	if m == nil {
		return "", fmt.Errorf("synthesis: no MissionState on session")
	}
	ref, refErr := MakeRef("synthesizer", synthesisWaveLabel)
	if refErr != nil {
		return "", refErr
	}
	h, ok := m.Handoffs.Get(ref)
	if !ok {
		return "", fmt.Errorf("synthesis: no handoff under %q", ref)
	}
	switch body := h.Body.(type) {
	case string:
		return body, nil
	case nil:
		return "", fmt.Errorf("synthesis: handoff body is nil")
	default:
		b, mErr := json.Marshal(body)
		if mErr != nil {
			return "", fmt.Errorf("synthesis: marshal body: %w", mErr)
		}
		return string(b), nil
	}
}

// synthesisHandoffView is the per-handoff projection the
// mission/synthesis_task template iterates. Mirrors the fields the
// runtime stores on a Handoff but drops the SubagentRef envelope
// so the template body stays terse — Role/Skill are pulled up
// directly. Body is reduced to its string form when the worker's
// output is structured; non-string bodies are rendered as JSON.
type synthesisHandoffView struct {
	Ref           string
	Role          string
	Skill         string
	Status        string
	MemorySummary string
	Body          string
}

// buildSynthesisTask renders the synthesizer's first-message body
// via the bundled `mission/synthesis_task` template. The template
// teaches the canonical kind=synthesis handoff fence — synthesis
// workers can be spawned under any dispatching skill, so the fence
// instruction has to come from the runtime rather than from a
// skill-specific prompt. Returns an error when the prompts renderer
// is unavailable or the template fails — there is no inline fallback
// by design ([[feedback-prompts-in-assets]]).
func buildSynthesisTask(mission extension.SessionState, goal string) (string, error) {
	data := struct {
		Goal     string
		Handoffs []synthesisHandoffView
	}{Goal: goal}
	if m := FromState(mission); m != nil {
		for _, h := range m.Handoffs.List() {
			data.Handoffs = append(data.Handoffs, synthesisHandoffView{
				Ref:           h.Ref,
				Role:          h.Subagent.Role,
				Skill:         h.Subagent.Skill,
				Status:        h.Status,
				MemorySummary: h.MemorySummary,
				Body:          synthesisHandoffBody(h.Body),
			})
		}
	}
	renderer := mission.Prompts()
	if renderer == nil {
		return "", fmt.Errorf("mission: synthesis task: no prompts renderer on session")
	}
	return renderer.Render("mission/synthesis_task", data)
}

// synthesisHandoffBody reduces the handoff's `body` field to the
// string form the template iterates. The output_contract parser
// stores `body` as `any` (string for kind=handoff/synthesis, struct
// for kind=plan/verdict); the synthesizer only needs a single
// human-readable rendering.
func synthesisHandoffBody(body any) string {
	switch v := body.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// emitWaveComplete publishes a mission:wave_complete ExtensionFrame
// on the mission session's event log. Status replays into PlanState
// on Recovery (Phase B once recovery lands); for Phase A it's
// primarily observability — scenario harnesses assert on its
// presence as a wave-boundary marker.
func (e *Extension) emitWaveComplete(mission extension.SessionState, label string, status WaveStatus, runErr error) {
	payload := struct {
		Label  string     `json:"label"`
		Status WaveStatus `json:"status"`
		Error  string     `json:"error,omitempty"`
	}{Label: label, Status: status}
	if runErr != nil {
		payload.Error = runErr.Error()
	}
	data, err := json.Marshal(payload)
	if err != nil {
		e.logger.Warn("mission: emitWaveComplete: marshal failed",
			"mission_session", mission.SessionID(), "wave", label, "err", err)
		return
	}
	frame := protocol.NewExtensionFrame(
		mission.SessionID(),
		agentParticipant(mission, e.agentID),
		providerName,
		protocol.CategoryOp,
		"wave_complete",
		data,
	)
	if err := mission.Emit(context.Background(), frame); err != nil {
		e.logger.Warn("mission: emitWaveComplete: emit failed",
			"mission_session", mission.SessionID(), "wave", label, "err", err)
	}
}

// buildFinalText is the recap rendered into the mission's terminal
// AgentMessage when no synthesis worker ran (or it failed). Synth
// text wins when non-empty; otherwise we summarise wave outcomes
// so the parent's history projection has something concrete.
func buildFinalText(mission extension.SessionState, synthText string, aborted bool) string {
	if synthText != "" {
		return synthText
	}
	var b strings.Builder
	if aborted {
		b.WriteString("Mission aborted after wave failure.\n\n")
	} else {
		b.WriteString("Mission completed.\n\n")
	}
	if m := FromState(mission); m != nil {
		fmt.Fprintf(&b, "Waves: %d, handoffs: %d.\n", len(m.Plan.Done), m.Handoffs.Len())
		for _, w := range m.Plan.Done {
			fmt.Fprintf(&b, "- %s: %s (%d subagent(s))\n", w.Label, w.Status, len(w.Subagents))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// finishMission emits the mission session's terminal AgentMessage —
// Final + Consolidated so the parent's subagent_pump.projectChildFrame
// treats it as the worker's final answer and constructs the
// SubagentResult. The parent then fires SessionClose at the mission
// (handleSubagentResult) and the mission's Run loop exits.
//
// Errors are logged but not surfaced; the mission session may have
// closed under us (parent Cancel, hard ceiling, etc.) in which case
// Emit becomes a best-effort no-op.
func (e *Extension) finishMission(ctx context.Context, mission extension.SessionState, text string) {
	frame := protocol.NewAgentMessageConsolidated(
		mission.SessionID(),
		agentParticipant(mission, e.agentID),
		text,
		0,
		true,
		nil,
		"",
		"",
	)
	if err := mission.Emit(ctx, frame); err != nil {
		e.logger.Warn("mission: finishMission: emit final message failed",
			"mission_session", mission.SessionID(), "err", err)
	}
}

// agentParticipant builds the ParticipantInfo mission ext stamps
// on synthetic messages it injects into the mission session.
// AgentID is the runtime's stable agent identifier.
func agentParticipant(_ extension.SessionState, agentID string) protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: agentID, Kind: protocol.ParticipantAgent, Name: "hugen"}
}
