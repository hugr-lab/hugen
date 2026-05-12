package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// turn_ceiling.go implements phase-4-spec §8.1 + §8.2: a soft-warning
// rising-edge nudge fires once when the model crosses metadata.hugen
// .max_turns; the hard ceiling at metadata.hugen.max_turns_hard
// terminates the session via the explicit-cancel teardown path so the
// parent observes a clean subagent_result{reason:"hard_ceiling"} on
// next Turn build.

// reloadSoftWarningFlag resets s.softWarningDone from the session's
// events on materialise. The runtime treats the flag as derived state
// (event-sourcing rule §4.3) — a restart that loses the in-memory
// state must rebuild it from events to keep the once-per-session
// invariant honest. Cheap: events are already loaded for replay.
func (s *Session) reloadSoftWarningFlag(rows []store.EventRow) {
	for _, ev := range rows {
		if ev.EventType != string(protocol.KindSystemMessage) {
			continue
		}
		if k, _ := ev.Metadata["kind"].(string); k == protocol.SystemMessageSoftWarning {
			s.softWarningDone.Store(true)
			return
		}
	}
	s.softWarningDone.Store(false)
}

// roleAndTaskForNudge teases out the (role, task) pair the soft-
// warning + stuck nudges weave into their text. Roots return
// ("root", ""); sub-agents return their spawn_role + the Task field
// captured from SubagentStarted.
func (s *Session) roleAndTaskForNudge(ctx context.Context) (role, task string) {
	if s.parent == nil {
		return "root", ""
	}
	// Sub-agents: read role from session metadata (spawn_role) and
	// task from the parent's subagent_started event for this child.
	role = "subagent"
	if s.store != nil {
		if row, err := s.store.LoadSession(ctx, s.id); err == nil {
			if r, _ := row.Metadata["spawn_role"].(string); r != "" {
				role = r
			}
		}
		if s.parent.id != "" {
			// Tight DB-side scan: parent's subagent_started row whose
			// metadata.child_session_id == this session's id. The
			// metadata.contains filter compiles to PostgreSQL `@>`,
			// so the result set is at most one row.
			rows, err := s.store.ListEvents(ctx, s.parent.id, store.ListEventsOpts{
				Kinds:            []string{string(protocol.KindSubagentStarted)},
				MetadataContains: map[string]any{"child_session_id": s.id},
				Limit:            1,
			})
			if err == nil && len(rows) > 0 {
				if t, _ := rows[0].Metadata["task"].(string); t != "" {
					task = t
				}
			}
		}
	}
	return role, task
}

// maybeInjectSoftWarning emits the role-conditioned soft-warning Frame
// the first time st.iter crosses st.cap. Idempotent across the
// session's lifetime — guarded by softWarningDone (in-memory) and the
// system_message_injected event (durable). Caller invokes this AT a
// turn boundary, after pendingInbound drain and BEFORE startModelIteration,
// so the next prompt build sees the nudge in s.history.
//
// The nudge is BOTH persisted in session_events AND folded into
// s.history. Sub-agents do not propagate it to the parent — phase-4-
// spec §11 keeps soft-warning / stuck-nudge entries inside the
// originating session's events.
func (s *Session) maybeInjectSoftWarning(runCtx context.Context) {
	st := s.turnState
	if st == nil || st.cap <= 0 || st.iter < st.cap {
		return
	}
	if s.softWarningDone.Load() {
		return
	}
	role, task := s.roleAndTaskForNudge(runCtx)
	canSpawnDeeper := s.deps == nil || s.deps.MaxDepth <= 0 ||
		s.depth+1 <= s.deps.MaxDepth
	text := softWarningText(s.deps.Prompts, role, task, st.iter, canSpawnDeeper)
	frame := protocol.NewSystemMessage(s.id, s.agent.Participant(),
		protocol.SystemMessageSoftWarning, text)
	if err := s.emit(runCtx, frame); err != nil {
		s.logger.Warn("session: emit soft_warning",
			"session", s.id, "err", err)
		return
	}
	s.softWarningDone.Store(true)
	s.history = append(s.history, model.Message{
		Role:    model.RoleUser,
		Content: fmt.Sprintf("[system: %s] %s", protocol.SystemMessageSoftWarning, text),
	})
}

// softWarningText renders the role-conditioned phrase from spec §8.1.
// The wording lives in two templates (interrupts/soft_warning_root
// for roots, interrupts/soft_warning_subagent for everything else)
// so the prose is reviewable as text-only diffs and tests can pin
// substring expectations cheaply.
//
// canSpawnDeeper toggles the "you can fan out further" advice on the
// sub-agent branch — for a root the spawn-subagents nudge always
// applies; for a sub-agent we only suggest spawning when the runtime
// max-depth still allows another level. Otherwise the sub-agent gets
// only the "return / give up / change tack" branches.
func softWarningText(r *prompts.Renderer, role, task string, turns int, canSpawnDeeper bool) string {
	if role == "root" {
		return strings.TrimRight(r.MustRender(
			"interrupts/soft_warning_root",
			map[string]any{"Turns": turns},
		), "\n")
	}
	return strings.TrimRight(r.MustRender(
		"interrupts/soft_warning_subagent",
		map[string]any{
			"Turns":          turns,
			"Task":           task,
			"CanSpawnDeeper": canSpawnDeeper,
		},
	), "\n")
}

// triggerHardCeiling invokes the §8.2 termination path. The session
// goroutine calls requestClose(reason="hard_ceiling") on itself; for
// roots the OnCloseRequest hook spawns Manager.Terminate which
// Submits SessionClose, and for subagents requestClose self-Submits
// SessionClose (phase 4.1c). Either way the deferred teardown in
// Session.Run writes the canonical
// session_terminated{reason:"hard_ceiling"} event.
//
// Phase 4.1b-pre stage B (O8) drops the standalone hard_ceiling_hit
// system_marker emit — requestClose already records the close trigger
// via close_requested{reason:"hard_ceiling"}, the unified
// observability signal. The exact iteration count at which the
// ceiling fired is recoverable from the events log
// (count tool_call rows in the active turn); not threaded into the
// marker today since no consumer depends on it.
func (s *Session) triggerHardCeiling(runCtx context.Context) {
	s.requestClose(runCtx, protocol.TerminationHardCeiling)
}
