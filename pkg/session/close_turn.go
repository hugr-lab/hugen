package session

import (
	"context"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Phase 4.2.3 ε — deterministic close turn.
//
// The runtime fires one constrained model turn between a session's
// final main-task turn and the persisted session_terminated row.
// The turn has a narrow system prompt ("review your work; record
// findings to the notepad") and a filtered tool surface (typically
// `notepad:append` only). Weak models reliably persist hypotheses
// in this slot even when they forgot to do so during the main
// task; the surface narrows their choices to a single useful
// action.
//
// Flow (worker example):
//
//  1. Main task wraps up; model emits AgentMessage{Final, Consolidated}.
//  2. Parent's wait_subagents observes the result, Submits
//     SessionClose to the child.
//  3. Child's routeInbound sees SessionClose, asks the
//     [extension.CloseTurnLookup]-implementing extensions for a
//     CloseTurnBlock matching its spawn skill/role.
//  4. If non-empty AND the close reason is not in the skip-list
//     (cancel cascade / hard ceiling / restart_died / abnormal
//     close), the runtime synthesises a UserMessage with the
//     close-turn prompt and calls startTurn directly. The tool
//     snapshot is narrowed (modelToolsForSession respects
//     closeTurnActive) and the turn cap is overridden
//     (resolveToolIterCap respects closeTurnActive).
//  5. The synthetic turn runs through the standard machinery —
//     model generates tool calls → dispatcher runs them → next
//     iteration up to the close-turn cap → eventual final
//     AgentMessage with no tool calls retires the turn.
//  6. foldAssistantAndMaybeDispatch's no-tool-calls branch
//     detects closeTurnActive and sets forceExit + closeReason.
//  7. Run's loop body checks forceExit after every iteration,
//     calls s.teardown(ctx), and returns nil. teardown sees the
//     reason and writes the terminal session_terminated row as
//     usual — the close turn is just one extra iteration before
//     the existing teardown sequence.
//
// Idempotency: closeTurnActive is set once on entry and cleared
// after the close turn completes. A nested SessionClose during
// the close turn (defensive) hits the gate and falls through to
// the regular signal path.

// closeTurnState holds the runtime-resolved close-turn config the
// session honours during the synthetic turn. Lives on the Session
// struct as a *closeTurnState pointer — nil means "not in close
// turn"; the field is owned by the Run goroutine (set in
// routeInbound, cleared in foldAssistantAndMaybeDispatch).
type closeTurnState struct {
	// AllowedTools narrows the tool snapshot for this turn. Empty
	// falls back to the runtime default (just "notepad:append")
	// so weak models can't drift into another tool family.
	AllowedTools []string

	// MaxTurns caps LLM iterations. Zero → default 2 (one tool
	// wave + one ack turn).
	MaxTurns int

	// PendingReason is the close reason the synthetic turn
	// finalises into the persisted session_terminated row. The
	// runtime stashes it here at routeInbound time and copies it
	// to s.closeReason when the synthetic turn retires.
	PendingReason string
}

// defaultCloseTurnAllowedTools is the surface the close turn
// runs against when the manifest didn't specify an override.
// Narrow by design — notepad:append is the only deliverable we
// expect from the model during the close window.
var defaultCloseTurnAllowedTools = []string{"notepad:append"}

// defaultCloseTurnMaxTurns is the iteration cap when the
// manifest didn't specify an override. Two iterations gives one
// tool wave (model emits notepad:append calls) plus one final
// "done" turn with no tool calls that triggers retire.
const defaultCloseTurnMaxTurns = 2

// resolveCloseTurnBlock walks the session's deps.Extensions for
// the first [extension.CloseTurnLookup] that returns a non-empty
// block. Returns an empty block (IsEmpty()==true) when no
// extension opts in — caller short-circuits.
//
// spawnSkill and spawnRole come from session metadata: they're
// the skill / role this session was spawned with (empty for
// root sessions). The lookup uses them to pick per-role overrides
// from sub_agents[i].on_close.
func (s *Session) resolveCloseTurnBlock(ctx context.Context) extension.CloseTurnBlock {
	if s.deps == nil {
		return extension.CloseTurnBlock{}
	}
	spawnSkill, spawnRole := s.spawnSkillAndRole()
	for _, ext := range s.deps.Extensions {
		l, ok := ext.(extension.CloseTurnLookup)
		if !ok {
			continue
		}
		b, err := l.ResolveCloseTurn(ctx, s, spawnSkill, spawnRole)
		if err != nil {
			s.logger.Warn("session: close-turn lookup failed",
				"session", s.id, "extension", ext.Name(), "err", err)
			continue
		}
		if !b.IsEmpty() {
			return b
		}
	}
	return extension.CloseTurnBlock{}
}

// spawnSkillAndRole returns the session's spawn metadata
// (stashed on the Session struct at construction time by
// spawn.go::Session.Spawn). Root sessions have empty values for
// both; only sub-agents carry meaningful skill/role names.
func (s *Session) spawnSkillAndRole() (skill, role string) {
	return s.spawnSkill, s.spawnRole
}

// shouldRunCloseTurn gates teardown's step-0 close turn. Root
// sessions skip (no parent → no Block B context across missions
// to consume their findings). Skip-list reasons (cancel cascade,
// hard ceiling, restart_died, abnormal_close, empty) also skip —
// the session is being torn down under conditions where running
// another model call is counterproductive or unsafe.
func (s *Session) shouldRunCloseTurn() bool {
	if s.depth <= 0 {
		return false
	}
	return !closeTurnSkipReason(s.closeReason)
}

// closeTurnSkipReason reports whether a SessionClose with the
// given reason should bypass the close turn entirely. Skipped
// flows: cancel cascade (parent forced termination — corrupt
// state, nothing distinct to record), hard ceiling (the session
// was already past its turn budget — adding another turn is
// counterproductive), restart_died (the previous process
// crashed; we don't know if the current state is consistent),
// abnormal_close (catch-all for pump-side synthesised
// terminations).
//
// Returns true when the reason matches a skip case.
func closeTurnSkipReason(reason string) bool {
	switch reason {
	case "",
		protocol.TerminationCancelCascade,
		"parent_cascade",
		"hard_ceiling",
		"restart_died",
		"abnormal_close":
		return true
	}
	// Phase 5.1c.cancel-ux — the operator's `/mission` cancel and
	// the Esc-Esc panic gesture stamp this prefix; running a
	// findings-recording close turn after the operator explicitly
	// asked to stop is counterproductive (the model would consume
	// budget on a wrap-up summary the operator does not want).
	if strings.HasPrefix(reason, protocol.TerminationUserCancelPrefix) {
		return true
	}
	return false
}

// buildCloseTurnState constructs the runtime state used while
// the synthetic turn runs. Applies extension-default fallbacks
// for AllowedTools and MaxTurns when the manifest left them
// empty/zero. PendingReason is set by the caller.
func buildCloseTurnState(b extension.CloseTurnBlock, reason string) *closeTurnState {
	st := &closeTurnState{
		AllowedTools:  b.AllowedTools,
		MaxTurns:      b.MaxTurns,
		PendingReason: reason,
	}
	if len(st.AllowedTools) == 0 {
		st.AllowedTools = defaultCloseTurnAllowedTools
	}
	if st.MaxTurns <= 0 {
		st.MaxTurns = defaultCloseTurnMaxTurns
	}
	return st
}

// closeTurnPromptOrDefault returns the system prompt the close
// turn surfaces as the synthetic UserMessage. When the manifest
// didn't override Prompt, falls back to a runtime default that
// references the notepad surface generically.
func closeTurnPromptOrDefault(b extension.CloseTurnBlock) string {
	if b.SystemPrompt != "" {
		return b.SystemPrompt
	}
	return defaultCloseTurnPrompt
}

// defaultCloseTurnPrompt is the generic close-turn instruction
// the runtime injects when no skill provides a domain-specific
// override. Phrased to nudge weak models toward stable
// categories (schema-finding, query-pattern, user-preference)
// and away from caching live values (counts, timestamps).
const defaultCloseTurnPrompt = `Your session is wrapping up. Before you close, review what you worked on this turn and decide whether anything is worth recording in the conversation notepad so the NEXT mission in this same root conversation can build on it.

Append a notepad entry for each distinct stable observation:
- ` + "`schema-finding`" + ` — table structures, soft-delete columns, naming conventions, FK shapes.
- ` + "`query-pattern`" + ` — a validated query template (shape only — not its current result).
- ` + "`user-preference`" + ` — a stated user preference (region, currency, time zone, format).
- ` + "`data-quality-issue`" + ` — anomalies, nulls, suspicious cardinalities you observed.
- ` + "`deferred-question`" + ` — an open question worth answering in a follow-up mission.

DO NOT append live values (counts, sums, top-N, current timestamps, latest record ids) — they go stale between turns; the next mission will re-run when it needs them.

Call ` + "`notepad:append`" + ` once per finding with a one-line ` + "`content`" + `. If you have nothing worth recording, reply "done" and emit no tool calls.`
