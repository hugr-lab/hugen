package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// asyncSummaryPrompt is the synthetic user-role prompt the runtime
// stamps onto the session when one or more async missions complete
// and root has no in-flight turn to consume their
// [system:subagent_result] inject. The model sees both the inject(s)
// (folded into s.history just above this synthetic message) and the
// directive below, and surfaces the result to the user without
// requiring them to type a follow-up first.
//
// Wording kept terse — the inject already carries the mission goal +
// rendered result via `interrupts/async_mission_completed.tmpl`.
const asyncSummaryPrompt = "[system:async_summary] One or more async missions delivered a result since the previous user message. " +
	"The [system:async_mission_completed] inject(s) above carry the results — quote them into the user-facing reply now. " +
	"Each inject names the mission's lifecycle status: " +
	"a TERMINATED mission is gone (do NOT re-spawn the same one and do NOT call session:notify_subagent for it — there is no live session to notify); " +
	"a PARKED mission is still alive in awaiting_dismissal (the inject explicitly says so) — you may session:notify_subagent it for a continuation OR session:subagent_dismiss it when the work is final, but do NOT re-spawn a fresh duplicate of it. " +
	"If the result is incomplete or ambiguous, ask the user a clarifying question instead of looping on tool calls."

// kickAsyncSummaryTurn starts a system-initiated turn driven by a
// synthetic UserMessage authored by the agent participant. Phase
// 5.1c.cancel-ux follow-up — the model sees the just-folded
// [system:subagent_result] inject(s) in its history and produces a
// user-facing reply summarising the mission(s) outcome.
//
// Safe to call when:
//   - The session is not closed / closing.
//   - There is no in-flight turn (turnState == nil).
//
// When a turn is already in flight the caller is expected to re-arm
// pendingAsyncSummary so the end-of-turn boundary fires the kick.
// The TUI suppresses inbound UserMessage frames (they are echoed on
// submit), so the synthetic prompt is invisible to the operator.
//
// The synthetic message IS persisted to the event log — replay /
// restart will see the same system-initiated trigger and reproduce
// the same turn. Author=agent.Participant() distinguishes it from
// real user input.
func (s *Session) kickAsyncSummaryTurn(ctx context.Context) {
	if s == nil || s.IsClosed() || s.IsClosing() {
		return
	}
	if s.turnState != nil {
		// Another turn started between the caller's idle check and
		// this entry — re-arm so the end-of-turn boundary handles
		// it. Avoids re-entering startTurn while one is already in
		// flight.
		s.pendingAsyncSummary.Store(true)
		return
	}
	synth := protocol.NewUserMessage(s.id, s.agent.Participant(), asyncSummaryPrompt)
	s.startTurn(ctx, synth)
}
