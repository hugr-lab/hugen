package skill

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Phase-4.1b-pre stage 3: skill extension Recovery + Closer.
//
// Recovery replays every CategoryOp ExtensionFrame addressed to
// the skill extension into the SkillManager's session state on
// materialise. Tool-path Load / Unload calls go through this code
// path on restart so the loaded set survives a process bounce.
//
// Closer drops the per-session entry from the SkillManager on
// teardown so a long-running agent doesn't accumulate dead
// session state in the manager's map.

// Compile-time assertions.
var (
	_ extension.Recovery = (*Extension)(nil)
	_ extension.Closer   = (*Extension)(nil)
)

// Recover implements [extension.Recovery]. Walks events in arrival
// order; for each ExtensionFrame whose Extension matches this
// provider name, decodes the payload and applies it to the live
// SkillManager via Load / Unload (idempotent). Errors are returned
// — the runtime logs them as warnings; recovery is best-effort.
func (e *Extension) Recover(ctx context.Context, state extension.SessionState, events []store.EventRow) error {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return nil
	}
	for _, ev := range events {
		if protocol.Kind(ev.EventType) != protocol.KindExtensionFrame {
			continue
		}
		op, name, ok := decodeSkillOp(ev)
		if !ok {
			continue
		}
		switch op {
		case OpLoad:
			if err := h.manager.Load(ctx, h.sessionID, name); err != nil {
				return fmt.Errorf("skill: recover load %q: %w", name, err)
			}
		case OpUnload:
			if err := h.manager.Unload(ctx, h.sessionID, name); err != nil {
				return fmt.Errorf("skill: recover unload %q: %w", name, err)
			}
		}
	}
	return nil
}

// CloseSession implements [extension.Closer]. Drops the per-session
// SkillManager entry so its map doesn't accumulate state for
// terminated sessions. Idempotent — Drop on a missing session is
// a no-op.
func (e *Extension) CloseSession(_ context.Context, state extension.SessionState) error {
	h := FromState(state)
	if h == nil || h.manager == nil {
		return nil
	}
	h.manager.Drop(h.sessionID)
	return nil
}

// decodeSkillOp pulls the (op, name) tuple from an ExtensionFrame
// row whose owning extension is "skill". Returns ok=false for any
// row that's not a skill op or doesn't decode cleanly — Recovery
// silently skips those rather than abort.
//
// store.FrameToEventRow flattens [protocol.ExtensionFramePayload]
// onto the metadata map (extension/category/op/data as top-level
// keys), which is the shape this decoder reads.
func decodeSkillOp(ev store.EventRow) (op, name string, ok bool) {
	if ev.Metadata == nil {
		return "", "", false
	}
	ext, _ := ev.Metadata["extension"].(string)
	if ext != providerName {
		return "", "", false
	}
	flatOp, _ := ev.Metadata["op"].(string)
	if flatOp == "" {
		return "", "", false
	}
	raw, has := ev.Metadata["data"]
	if !has {
		return "", "", false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return "", "", false
	}
	if n, got := nameFromData(b, flatOp); got {
		return flatOp, n, true
	}
	return "", "", false
}

// nameFromData decodes the payload Data bytes against the
// per-op JSON shape and returns the skill name. The payload is
// trivially small for both ops; the extra unmarshal is cheaper
// than dealing with two separate decoding helpers.
func nameFromData(data json.RawMessage, op string) (string, bool) {
	switch op {
	case OpLoad:
		var d LoadOpData
		if err := json.Unmarshal(data, &d); err != nil || d.Name == "" {
			return "", false
		}
		return d.Name, true
	case OpUnload:
		var d UnloadOpData
		if err := json.Unmarshal(data, &d); err != nil || d.Name == "" {
			return "", false
		}
		return d.Name, true
	default:
		return "", false
	}
}
