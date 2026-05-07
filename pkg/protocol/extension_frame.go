package protocol

import "encoding/json"

// KindExtensionFrame is the single discriminator every extension-
// owned event carries on the wire and in the persisted event log.
// Extension/Category/Op fields inside the payload narrow the
// dispatch further without the codec needing to know each
// extension. See [ExtensionFrame] for the envelope.
const KindExtensionFrame Kind = "extension_frame"

// Category groups extension events by the routing decision the
// runtime makes without knowing extension semantics. Visibility,
// Recovery, and the inbound Router branch on Category — Op events
// replay into projections, Message events potentially surface to
// the model, Marker events are diagnostics only, Result events
// flow through RouteToolFeed.
type Category string

const (
	// CategoryOp marks a state-change event the extension's Recovery
	// hook replays into its in-memory projection (plan_op, whiteboard_op,
	// skill_load, …). Never visible to the model directly.
	CategoryOp Category = "op"

	// CategoryMessage marks a message-shaped event that may project
	// into the model's history via the extension's Visibility hook
	// (whiteboard_message, system_message-like records owned by
	// extensions). The runtime defaults to "not visible"; the
	// extension opts in.
	CategoryMessage Category = "message"

	// CategoryMarker marks a diagnostic record meant for the audit
	// trail / adapters / operators. Never injected into the model
	// prompt, never replayed by Recovery (subagent_started moved into
	// the envelope would be a Marker; subagent itself is core today).
	CategoryMarker Category = "marker"

	// CategoryResult marks a terminal result the runtime routes via
	// the active tool feed (subagent_result analogue if subagent
	// migrates; today reserved for future extensions that mimic the
	// blocking tool-call pattern).
	CategoryResult Category = "result"
)

// ExtensionFramePayload is the wire shape of an extension-owned
// event. The codec ships this structure verbatim; the receiving
// extension parses Data into its own typed payload.
//
//   - Extension: stable name of the owning extension
//     (ExtensionFrame.Frame.SessionID stays the addressed session).
//   - Category: routing classifier (see [Category]).
//   - Op: extension-internal namespace ("set", "init", "load", …).
//     Free-form; only the owning extension interprets it.
//   - Data: the extension's typed payload, JSON-encoded. The
//     extension defines its own struct and (un)marshals at the
//     boundary.
type ExtensionFramePayload struct {
	Extension string          `json:"extension"`
	Category  Category        `json:"category"`
	Op        string          `json:"op"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// ExtensionFrame is the single Frame variant extensions emit and
// consume. The envelope inherits from BaseFrame (FrameID, session,
// author, occurred_at, seq, FromSession, RequestID); the variant
// payload is [ExtensionFramePayload].
//
// Adding a new extension requires zero changes to pkg/protocol —
// the extension defines its own Op/Data types, registers in
// pkg/runtime, and the codec round-trip just works.
type ExtensionFrame struct {
	BaseFrame
	Payload ExtensionFramePayload
}

func (f ExtensionFrame) payload() any { return f.Payload }

// NewExtensionFrame builds an ExtensionFrame addressed to
// sessionID and authored by author. Data is the extension's
// pre-marshalled payload bytes; pass nil when the op carries no
// payload (e.g. whiteboard:stop). FromSession is reserved for
// cross-session ExtensionFrames (whiteboard host→member); leave it
// empty for in-session events.
func NewExtensionFrame(sessionID string, author ParticipantInfo, extension string, category Category, op string, data json.RawMessage) *ExtensionFrame {
	return &ExtensionFrame{
		BaseFrame: newBase(sessionID, KindExtensionFrame, author),
		Payload: ExtensionFramePayload{
			Extension: extension,
			Category:  category,
			Op:        op,
			Data:      data,
		},
	}
}

