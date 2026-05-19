package mission

import (
	"context"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// RunMission implements [extension.MissionAutoRunner]. Called by
// pkg/session.spawn_mission AFTER the mission session has been
// opened and its state initialisers fired. Phase A scope —
// kicks the inline-plan executor on a goroutine; returns nil
// immediately so spawn_mission's tool dispatch can continue.
//
// The mission session itself does NOT run an LLM supervisor in
// Phase A — the executor drives the entire mission. When the
// last wave completes and synthesis lands, the executor fires
// the closing AgentMessage on the mission session and the
// session's own goroutine tears down.
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

	// Kick the executor on a goroutine — RunMission is the
	// non-blocking trigger. The executor calls back into the
	// mission session via SessionSpawner; the mission's
	// ChildFrameObserver (this very extension) collects
	// handoffs into the per-session MissionState.
	go e.driveMission(mission, spawner, *manifest, goal, inputs)
	return nil
}

// driveMission is the goroutine body: runs every wave, then the
// synthesis role, then closes the mission session.
func (e *Extension) driveMission(mission extension.SessionState, spawner extension.SessionSpawner, manifest MissionManifest, goal string, inputs any) {
	// Phase A: a per-mission Executor. Spawner adapter translates
	// mission.SpawnRequest -> extension.SpawnSpec + delivers the
	// task as the worker's first user message.
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
		// Deliver the task as the child's first message so its turn
		// loop has something to run on.
		first := protocol.NewUserMessage(child.SessionID(), agentParticipant(mission, e.agentID), req.Task)
		settled := child.Submit(context.Background(), first)
		return SpawnResult{SessionID: child.SessionID(), Settled: settled}, nil
	}, e.logger)

	ctx := context.Background()
	// Inputs is unused in Phase A's inline plan (templates +
	// dynamic substitution land Phase B). Pass through to make
	// the contract clear.
	_ = inputs
	_ = goal

	// Translate the manifest's inline plan into the executor's
	// in-flight Wave AST and run each in sequence.
	for _, waveDecl := range manifest.Plan.ExperimentalInline.Waves {
		status, _, err := executor.RunWave(ctx, mission, waveDecl, RunWaveOptions{})
		if err != nil {
			e.logger.Error("mission: driveMission: wave failed",
				"mission_session", mission.SessionID(),
				"wave", waveDecl.Label, "err", err)
			return
		}
		if status == WaveStatusFailed {
			e.logger.Warn("mission: driveMission: wave failed with status=failed",
				"mission_session", mission.SessionID(),
				"wave", waveDecl.Label)
			return
		}
	}

	// Phase A: synthesis is optional. When declared, runtime
	// would spawn a `synthesis.role` worker fed every prior
	// wave's handoffs. Postponed to a later commit on this
	// branch (α.3 wires the kickoff; synthesis lands α.3 follow-up
	// or Phase A α.4).
	if manifest.Synthesis.Role != "" {
		e.logger.Debug("mission: driveMission: synthesis role declared but unimplemented in α.3 kickoff",
			"role", manifest.Synthesis.Role)
	}

	e.logger.Info("mission: driveMission: completed",
		"mission_session", mission.SessionID(),
		"waves", len(manifest.Plan.ExperimentalInline.Waves))
}

// agentParticipant builds the ParticipantInfo mission ext stamps
// on synthetic messages it injects into the mission session.
// AgentID is the runtime's stable agent identifier.
func agentParticipant(_ extension.SessionState, agentID string) protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: agentID, Kind: protocol.ParticipantAgent, Name: "hugen"}
}

