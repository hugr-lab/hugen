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
func (e *Extension) Recover(_ context.Context, state extension.SessionState, events []store.EventRow) error {
	s := FromState(state)
	if s == nil {
		return nil
	}
	var (
		latest  *DigestPayload
		entries = make([]HistoryEntry, 0, len(events))
	)
	renderer := state.Prompts()
	for i := range events {
		r := events[i]
		if protocol.Kind(r.EventType) == protocol.KindExtensionFrame {
			ext, _ := r.Metadata["extension"].(string)
			if ext == providerName {
				op, _ := r.Metadata["op"].(string)
				switch op {
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
