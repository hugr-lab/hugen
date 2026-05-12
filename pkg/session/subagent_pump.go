package session

import (
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// asyncGoalMaxLen caps SubagentResultPayload.Goal so the async-
// notify render template (`interrupts/async_mission_completed.tmpl`)
// has a predictable prompt budget. Goals longer than this surface
// truncated; the model can `session:subagent_runs(...)` for full
// context.
const asyncGoalMaxLen = 200

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// consumeChildOutbox is the parent-side adapter to a sub-agent. Spawn
// starts one goroutine per child right after child.Start(ctx); the
// pump reads child.Outbox(), projects cross-session-relevant frames
// into parent's pipeline via parent.Submit, and drains the rest. The
// range loop exits when child closes its outbox in Run's defer chain
// — same lifecycle signal an HTTP/SSE adapter's pump uses on a root
// session (see manager/runtime.go::startSessionPump).
//
// Phase-4.1c contract: child emits standard kinds only — same shape
// as a root session — and constructs no cross-session frame. Parent
// observes and projects. Pump's switch is kind-level; default path
// drains so new kinds in pkg/protocol never accidentally leak into
// parent's transcript.
//
// Fire-and-forget. No WaitGroup, no synchronisation with parent's
// teardown — channel close IS the lifecycle signal. Parent-close
// race: projectToParent's IsClosed guard drops the projection on
// the floor; restart's settleDanglingSubagents reconciles the
// dangling child via the same path that recovers from kill -9.
//
// If the range loop exits without the pump ever projecting a terminal
// SubagentResult (cancel cascade / hard ceiling / panic — paths where
// child's handleExit writes session_terminated direct-to-store and
// closes outbox before any terminal frame reaches the pump), the
// post-loop finalizer synthesises a SubagentResult{reason:"abnormal_close"}
// so wait_subagents on the parent side never blocks indefinitely.
func (s *Session) consumeChildOutbox(child *Session) {
	state := childPumpState{}
	for f := range child.Outbox() {
		s.projectChildFrame(child, f, &state)
	}
	if !state.projected {
		s.projectAbnormalClose(child, state.consolidatedSeen)
	}
}

// childPumpState is the per-child pump cursor. Single-writer (the
// pump goroutine) so plain fields suffice — no atomic, no mutex.
//
//   - projected flips true the first time the pump emits a
//     SubagentResult for this child. Subsequent terminal-bearing
//     kinds are drained.
//   - consolidatedSeen counts every AgentMessage{Consolidated:true}
//     observed on the outbox; serves as the TurnsUsed approximation
//     when projecting (parent has no direct access to child's own
//     turn counter).
type childPumpState struct {
	projected        bool
	consolidatedSeen int
}

// projectChildFrame is the kind-level dispatcher. Closed switch —
// every concrete protocol.Kind defaults to drain unless explicitly
// listed here. Three project paths today:
//
//   - AgentMessage with Final && Consolidated → "result" (turn-end
//     with no further tool calls — child's standard answer signal).
//   - Error → "terminal error" (any Error in a subagent context is
//     terminal: a subagent has no human user to retry against, so
//     even a Recoverable=true error like stream_error / 429 leaves
//     the child idle forever from parent's POV. Retries belong in
//     the model layer; once Error reaches session.emit the model
//     has already given up. Parent's LLM decides next steps from
//     the projected SubagentResult. Roots keep their existing
//     "stay idle on recoverable error" semantics — only the pump's
//     subagent-side projection is opinionated here.)
//   - SessionTerminated → fallback projection if no prior result
//     reached the pump (handleExit emits this on outbox best-effort
//     for non-cancel paths via the SessionClosed-side recover'd push;
//     see session.go's emitClose path).
//
// Future Phase-5 HITL kinds add one case here when those frames land
// in pkg/protocol.
func (s *Session) projectChildFrame(child *Session, f protocol.Frame, st *childPumpState) {
	switch v := f.(type) {
	case *protocol.AgentMessage:
		if v.Payload.Consolidated {
			st.consolidatedSeen++
		}
		if !st.projected && v.Payload.Final && v.Payload.Consolidated {
			sr := protocol.NewSubagentResult(s.id, child.id, s.agent.Participant(),
				protocol.SubagentResultPayload{
					SessionID:  child.id,
					Reason:     protocol.TerminationCompleted,
					Result:     v.Payload.Text,
					TurnsUsed:  st.consolidatedSeen,
					Goal:       truncate(child.mission, asyncGoalMaxLen),
					RenderMode: child.asyncSpawnMode,
				})
			s.projectToParent(sr)
			st.projected = true
		}
	case *protocol.Error:
		if !st.projected {
			sr := protocol.NewSubagentResult(s.id, child.id, s.agent.Participant(),
				protocol.SubagentResultPayload{
					SessionID:  child.id,
					Reason:     "error: " + v.Payload.Code,
					Result:     v.Payload.Message,
					TurnsUsed:  st.consolidatedSeen,
					Goal:       truncate(child.mission, asyncGoalMaxLen),
					RenderMode: child.asyncSpawnMode,
				})
			s.projectToParent(sr)
			st.projected = true
		}
	case *protocol.SessionTerminated:
		if !st.projected {
			turns := v.Payload.TurnsUsed
			if turns == 0 {
				turns = st.consolidatedSeen
			}
			sr := protocol.NewSubagentResult(s.id, child.id, s.agent.Participant(),
				protocol.SubagentResultPayload{
					SessionID:  child.id,
					Reason:     v.Payload.Reason,
					Result:     v.Payload.Result,
					TurnsUsed:  turns,
					Goal:       truncate(child.mission, asyncGoalMaxLen),
					RenderMode: child.asyncSpawnMode,
				})
			s.projectToParent(sr)
			st.projected = true
		}
	default:
		// Drain. Streaming chunks (Final=false or Consolidated=false),
		// reasoning, tool_call/result, recoverable errors, status
		// markers, opened/closed lifecycle events, system_marker,
		// extension_frame — all local to child's session.
	}
}

// projectAbnormalClose synthesises a terminal SubagentResult when
// child's outbox closed without the pump ever projecting one. Covers
// cancel cascade / hard ceiling / panic / restart_died — paths where
// child's handleExit writes session_terminated direct-to-store and
// closes outbox before any terminal frame reaches the pump.
//
// The synthetic frame's reason ("abnormal_close") is informational;
// wait_subagents observes it and unblocks. Child's own session_
// terminated row in its own store carries the real reason for
// post-hoc inspection.
func (s *Session) projectAbnormalClose(child *Session, consolidatedSeen int) {
	sr := protocol.NewSubagentResult(s.id, child.id, s.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID: child.id,
			Reason:    "abnormal_close",
			TurnsUsed: consolidatedSeen,
		})
	s.projectToParent(sr)
}

// projectToParent delivers a parent-constructed SubagentResult into
// parent's inbox via Submit so it follows the standard routeInbound
// → handleSubagentResult (issues SessionClose to child + waits
// child.Done() + cleanup) → RouteToolFeed (wait_subagents feed)
// pipeline, identical to today's flow with the producer changed.
//
// Fire-and-forget — pump does not wait on the returned settled
// channel. The pump goroutine emits at most ONE SubagentResult per
// child (st.projected gate), so there is no in-pump ordering to
// preserve, and waiting would couple pump's progress to the parent's
// teardown of *this same child*: handleSubagentResult on the parent
// blocks on child.Done(), which (for graceful exits) needs
// child.handleExit to push SessionTerminated to outbox via outboxOnly,
// which blocks if the outbox buffer is full and the pump — its only
// consumer — is itself stuck in this Submit. Decoupling avoids that
// latent deadlock; Submit's own goroutine handles delivery + closed-
// channel recovery without our intervention.
//
// Closed-parent path: drop the projection. Once parent.IsClosed() we
// have no live consumer; the dangling child surfaces on the next
// restart via Manager.RestoreActive → settleDanglingSubagents (same
// path that recovers from kill -9).
func (s *Session) projectToParent(sr *protocol.SubagentResult) {
	if s.IsClosed() {
		return
	}
	_ = s.Submit(s.ctx, sr)
}
