package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
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
func (s *Session) reloadSoftWarningFlag(rows []EventRow) {
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
			if rows, err := s.store.ListEvents(ctx, s.parent.id, ListEventsOpts{}); err == nil {
				for _, ev := range rows {
					if ev.EventType != string(protocol.KindSubagentStarted) {
						continue
					}
					childID, _ := ev.Metadata["child_session_id"].(string)
					if childID != s.id {
						continue
					}
					if t, _ := ev.Metadata["task"].(string); t != "" {
						task = t
					}
					break
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
	canSpawnDeeper := s.deps == nil || s.deps.maxDepth <= 0 ||
		s.depth+1 <= s.deps.maxDepth
	text := softWarningText(role, task, st.iter, canSpawnDeeper)
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
// Kept in one place so the wording is reviewable as a unit and the
// tests can pin substring expectations cheaply.
//
// canSpawnDeeper toggles the "you can fan out further" advice on the
// sub-agent branch — for a root the spawn-subagents nudge always
// applies; for a sub-agent we only suggest spawning when the runtime
// max-depth still allows another level. Otherwise the sub-agent gets
// only the "return / give up / change tack" branches.
func softWarningText(role, task string, turns int, canSpawnDeeper bool) string {
	if role == "root" {
		return fmt.Sprintf("You've used %d turns on this task. This is just a soft signal — the loop may be productive. Before the next call, consider: are you still on the right path? Would breaking the remaining work into focused sub-agents (`spawn_subagent`) make progress more reliable? If you're confident in the current approach, continue.", turns)
	}
	taskClause := ""
	if task != "" {
		taskClause = fmt.Sprintf(" inside task %q", task)
	}
	tail := "You can return what you have, give up cleanly explaining what blocked you, or change tack."
	if canSpawnDeeper {
		tail += " You can also fan out via `spawn_subagent` if the remaining work splits cleanly."
	} else {
		tail += " Sub-sub-agents are not available at this depth."
	}
	return fmt.Sprintf("You've used %d turns%s. The loop may be productive — but consider stopping to think: is the current approach still right? %s", turns, taskClause, tail)
}

// triggerHardCeiling invokes the §8.2 termination path. The session
// goroutine calls s.terminate(reason="hard_ceiling") on itself
// (mirroring Manager.Terminate's body without the root-only lookup);
// the deferred teardown sequence in Session.Run writes the canonical
// session_terminated{reason:"hard_ceiling"} event and, for sub-agents,
// surfaces a subagent_result{reason:"hard_ceiling"} Frame to the
// parent. A system_marker{subject:"hard_ceiling_hit"} also lands so
// the adapter / operator dashboards surface the event without parsing
// the terminal record.
func (s *Session) triggerHardCeiling(runCtx context.Context, turnsUsed int) {
	mk := protocol.NewSystemMarker(s.id, s.agent.Participant(),
		protocol.SubjectHardCeilingHit,
		map[string]any{"turns_used": turnsUsed})
	if err := s.emit(runCtx, mk); err != nil {
		s.logger.Warn("session: emit hard_ceiling_hit marker",
			"session", s.id, "err", err)
	}
	s.terminate(&terminationCause{
		reason:    protocol.TerminationHardCeiling,
		emitClose: true,
		writeCtx:  runCtx,
	})
}
