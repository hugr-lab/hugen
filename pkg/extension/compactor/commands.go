package compactor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// emptyJSONObject is the digest_clear payload — recovery only
// inspects metadata.op, but the codec needs a non-nil
// json.RawMessage so the round-trip stays well-formed.
var emptyJSONObject = json.RawMessage(`{}`)

// Compile-time assertion: compactor participates in the
// Commander pipeline so the runtime registers /compactor on
// every session.
var _ extension.Commander = (*Extension)(nil)

// Commands implements [extension.Commander]. The compactor
// contributes one top-level command — /compactor — with three
// sub-commands: status, reset, compact. The dispatch happens in
// the handler instead of registering three separate top-level
// names so the user types `/compactor <sub>` (matching the spec
// §9 table) without polluting the global /help listing.
func (e *Extension) Commands() []extension.Command {
	return []extension.Command{{
		Name:        "compactor",
		Description: "compactor controls: /compactor <status|reset|compact>",
		Handler:     e.cmdCompactor,
	}}
}

// cmdCompactor dispatches on the first arg to the matching
// sub-handler. Empty / unknown sub-commands return a system-
// marker `command_error` frame so adapters render a one-line
// "usage:" hint to the user.
func (e *Extension) cmdCompactor(ctx context.Context, state extension.SessionState, env extension.CommandContext, args []string) ([]protocol.Frame, error) {
	sessionID := state.SessionID()
	if len(args) == 0 {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "compactor_usage",
				"usage: /compactor <status|reset|compact>", false),
		}, nil
	}
	switch args[0] {
	case "status":
		return e.cmdCompactorStatus(ctx, state, env)
	case "reset":
		return e.cmdCompactorReset(ctx, state, env)
	case "compact":
		return e.cmdCompactorCompact(ctx, state, env)
	default:
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "compactor_usage",
				fmt.Sprintf("unknown sub-command %q; usage: /compactor <status|reset|compact>", args[0]),
				false),
		}, nil
	}
}

// cmdCompactorStatus prints a one-line summary of the active
// digest projection: iteration, cutoff seq, summary block count,
// kept-verbatim count, last build time. Lands on the session
// transcript as a system_message so the user (and adapter
// transcripts) see it inline.
func (e *Extension) cmdCompactorStatus(_ context.Context, state extension.SessionState, env extension.CommandContext) ([]protocol.Frame, error) {
	sessionID := state.SessionID()
	s := FromState(state)
	if s == nil {
		return []protocol.Frame{
			protocol.NewSystemMessage(sessionID, env.AgentAuthor, "compactor_status",
				"compactor: not initialised on this session"),
		}, nil
	}
	d := s.Digest()
	if d == nil {
		return []protocol.Frame{
			protocol.NewSystemMessage(sessionID, env.AgentAuthor, "compactor_status",
				fmt.Sprintf(
					"compactor: no digest built yet · %d boundaries observed · %d est tokens",
					s.BoundaryCount(), s.EstimatedPromptTokens())),
		}, nil
	}
	built := d.BuiltAt
	var builtTxt string
	if built.IsZero() {
		builtTxt = "n/a"
	} else {
		builtTxt = built.UTC().Format(time.RFC3339)
	}
	line := fmt.Sprintf(
		"compactor: iteration %d · cutoff seq %d · %d blocks · %d kept · last built %s",
		d.Iteration, d.CutoffSeq, len(d.SummaryBlocks), len(d.KeptVerbatim), builtTxt,
	)
	return []protocol.Frame{
		protocol.NewSystemMessage(sessionID, env.AgentAuthor, "compactor_status", line),
	}, nil
}

// cmdCompactorReset emits a digest_clear ExtensionFrame and
// drops the in-memory state. Next turn re-triggers compaction
// if the configured thresholds still hold.
func (e *Extension) cmdCompactorReset(ctx context.Context, state extension.SessionState, env extension.CommandContext) ([]protocol.Frame, error) {
	sessionID := state.SessionID()
	s := FromState(state)
	if s == nil {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "compactor_reset",
				"compactor: not initialised on this session", false),
		}, nil
	}
	// Emit digest_clear so Recovery on restart sees the wipe.
	// digest_clear payload carries no body — Recovery only
	// inspects metadata.op.
	frame := protocol.NewExtensionFrame(
		sessionID,
		agentParticipant(e.deps.AgentID),
		providerName,
		protocol.CategoryOp,
		OpDigestClear,
		emptyJSONObject,
	)
	if err := state.Emit(ctx, frame); err != nil {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "compactor_reset",
				"compactor: emit digest_clear: "+err.Error(), true),
		}, nil
	}
	s.ClearDigest()
	// η.2 — rebuild history from the event log so the live
	// model view sees the full transcript again; reuse the
	// Recovery logic so reset and restart share one code path
	// (boundary tracker + post-cutoff projection + strategy
	// post-trim). /compactor reset is user-triggered and runs
	// once per invocation; a full ListEvents scan here is fine.
	if e.deps.Store != nil {
		events, err := e.deps.Store.ListEvents(ctx, sessionID, store.ListEventsOpts{Limit: 100_000})
		if err != nil {
			e.logger.Warn("compactor reset: list events for rebuild failed",
				"session", sessionID, "err", err)
		} else if err := e.Recover(ctx, state, events); err != nil {
			e.logger.Warn("compactor reset: rebuild via Recover failed",
				"session", sessionID, "err", err)
		}
	}
	return []protocol.Frame{
		protocol.NewSystemMarker(sessionID, env.AgentAuthor, "compactor_reset", nil),
	}, nil
}

// cmdCompactorCompact force-fires a compaction at the current
// boundary, bypassing the trigger predicate. Used for dogfood +
// debug. Returns a status-style system_message once the
// pipeline returns (success or hard-fallback marker), so the
// user sees that the request was honoured.
//
// When the pipeline short-circuits (e.g., not enough turns past
// the preserved window yet) the iteration counter doesn't move;
// we emit a dedicated system_message explaining why instead of
// the regular status line so the user isn't left wondering why
// nothing happened.
func (e *Extension) cmdCompactorCompact(ctx context.Context, state extension.SessionState, env extension.CommandContext) ([]protocol.Frame, error) {
	sessionID := state.SessionID()
	s := FromState(state)
	beforeIter := 0
	if s != nil {
		if d := s.Digest(); d != nil {
			beforeIter = d.Iteration
		}
	}
	if err := e.compact(ctx, state); err != nil {
		return []protocol.Frame{
			protocol.NewError(sessionID, env.AgentAuthor, "compactor_compact",
				"compactor: force-compact failed: "+err.Error(), true),
		}, nil
	}
	afterIter := 0
	if s != nil {
		if d := s.Digest(); d != nil {
			afterIter = d.Iteration
		}
	}
	if afterIter == beforeIter && s != nil {
		cfg := e.resolveTierConfig(ctx, state)
		boundary := s.BoundaryCount()
		reason := "compactor: force-compact ran but produced no digest"
		if boundary <= cfg.PreservedRecentTurns {
			reason = fmt.Sprintf(
				"compactor: force-compact no-op — %d boundaries observed; need more than preserved_recent_turns (%d)",
				boundary, cfg.PreservedRecentTurns)
		}
		return []protocol.Frame{
			protocol.NewSystemMessage(sessionID, env.AgentAuthor, "compactor_compact", reason),
		}, nil
	}
	// Re-render status so the user immediately sees the new
	// iteration / cutoff line.
	return e.cmdCompactorStatus(ctx, state, env)
}
