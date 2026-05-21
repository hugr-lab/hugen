package compactor

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Recover implements [extension.Recovery]. Single pass over the
// session event log: replays compactor-owned `digest_set` /
// `digest_clear` ops (latest-wins) AND rebuilds the in-memory
// history projection from every persisted frame (η.1).
//
// Rows with an unrecognised op are skipped with a debug log
// (forward-compat). Rows with Version > [CurrentPayloadVersion]
// are skipped — a forward-rolled binary may have written shapes
// this one does not understand.
//
// Best-effort by [extension.Recovery] contract: a malformed row
// logs a warning and is skipped; recovery never blocks session
// start.
func (e *Extension) Recover(ctx context.Context, state extension.SessionState, events []store.EventRow) error {
	s := FromState(state)
	if s == nil {
		return nil
	}
	// First pass: find the latest digest_set / digest_clear so
	// the history projection knows where the verbatim tail starts.
	// Events before CutoffSeq are summarised inside the digest's
	// Block C / KeptVerbatim — re-projecting them as verbatim
	// would double-feed them into the next prompt.
	var latest *DigestPayload
	for i := range events {
		r := events[i]
		if protocol.Kind(r.EventType) != protocol.KindExtensionFrame {
			continue
		}
		if ext, _ := r.Metadata["extension"].(string); ext != providerName {
			continue
		}
		switch op, _ := r.Metadata["op"].(string); op {
		case OpDigestSet:
			d, err := decodeDigest(r.Metadata)
			if err != nil {
				e.logger.Warn("compactor recovery: malformed digest_set",
					"session", state.SessionID(), "err", err)
				continue
			}
			if d.Version > CurrentPayloadVersion {
				e.logger.Debug("compactor recovery: future version skipped",
					"session", state.SessionID(),
					"version", d.Version,
					"current", CurrentPayloadVersion)
				continue
			}
			latest = d
		case OpDigestClear:
			latest = nil
		default:
			e.logger.Debug("compactor recovery: unknown op",
				"session", state.SessionID(), "op", op)
		}
	}

	// Reset boundary + history state — Recover replays from the
	// authoritative event log, replacing whatever the live emit
	// path may have appended pre-materialise.
	s.boundary.mu.Lock()
	s.boundary.userMessageSeqs = nil
	s.boundary.estimatedTokens = 0
	s.boundary.mu.Unlock()

	// Second pass: rebuild boundary tracker from ALL events
	// (we need the full user-message seq list for cutoff math
	// on the NEXT compaction) and project history entries from
	// events strictly past CutoffSeq (or all of them when no
	// digest is set).
	cutoff := int64(0)
	if latest != nil {
		cutoff = latest.CutoffSeq
	}
	renderer := state.Prompts()
	entries := make([]HistoryEntry, 0, len(events))
	for i := range events {
		r := events[i]
		// Boundary tracker mirrors live OnFrameEmit accounting.
		seq := int64(r.Seq)
		switch protocol.Kind(r.EventType) {
		case protocol.KindUserMessage:
			s.appendBoundary(seq, estimateTokens(r.Content))
		case protocol.KindAgentMessage:
			if cons, _ := metadataBool(r.Metadata, "consolidated"); cons {
				s.addTokens(estimateTokens(r.Content))
			}
		case protocol.KindToolResult:
			body := r.ToolResult
			if body == "" {
				body = r.Content
			}
			s.addTokens(estimateTokens(body))
		}
		// History projection: skip pre-cutoff frames — they
		// already live inside Block C / KeptVerbatim of the
		// restored digest. ExtensionFrames never project.
		if seq <= cutoff {
			continue
		}
		if protocol.Kind(r.EventType) == protocol.KindExtensionFrame {
			continue
		}
		if entry, ok := projectRowToEntry(renderer, &r); ok {
			entries = append(entries, entry)
		}
	}

	if latest != nil {
		s.SetDigest(latest)
	} else {
		s.ClearDigest()
	}
	s.resetHistory(entries)

	// Apply per-strategy post-restore trim. summarize already
	// has the right shape (history was filtered to post-cutoff
	// only); window enforces its FIFO cap; off leaves it alone.
	cfg := e.resolveTierConfig(ctx, state)
	if effectiveStrategy(cfg.Strategy) == StrategyWindow {
		s.pruneWindow(cfg.WindowSize)
	}
	return nil
}

// decodeDigest lifts the typed [*DigestPayload] from the
// metadata's `data` field. The persistence layer round-trips
// the ExtensionFrame payload via the metadata column; the data
// arrives as map[string]any decoded from JSON, so we re-encode +
// decode to get the typed shape back.
func decodeDigest(meta map[string]any) (*DigestPayload, error) {
	raw, ok := meta["data"]
	if !ok {
		return nil, errMissingData
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var d DigestPayload
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// errMissingData surfaces when a digest_set row carries no
// `data` field — should never happen in practice (the emit path
// always populates it).
var errMissingData = errCompactor("digest_set row missing data field")

// errCompactor is a typed string-error so the recovery path's
// debug logs are stable.
type errCompactor string

func (e errCompactor) Error() string { return string(e) }
