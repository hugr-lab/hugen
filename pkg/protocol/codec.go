package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnknownKind is returned by DecodeFrame when the wire kind does
// not match a registered variant.
var ErrUnknownKind = errors.New("protocol: unknown frame kind")

// ErrInvalidPayload is returned when the variant payload fails to
// unmarshal or fails post-unmarshal validation.
var ErrInvalidPayload = errors.New("protocol: invalid payload")

// Codec handles JSON encoding/decoding of Frames.
//
// EncodeFrame produces the canonical envelope (frame_id, session_id,
// kind, author, occurred_at, payload). DecodeFrame is the reverse.
//
// EncodePayload / DecodePayload are used by the persistence facade
// to round-trip a Frame through a session_events row whose envelope
// columns are stored separately from the variant payload.
type Codec struct{}

// NewCodec returns a Codec. The zero value is also usable; the
// constructor exists to make the dependency wiring explicit.
func NewCodec() *Codec { return &Codec{} }

// rawEnvelope mirrors BaseFrame on the wire and carries the raw
// payload bytes for variant-specific decoding.
type rawEnvelope struct {
	BaseFrame
	Payload json.RawMessage `json:"payload"`
}

type wireFrame struct {
	BaseFrame
	Payload any `json:"payload"`
}

// EncodeFrame serialises a Frame to the canonical wire form.
func (c *Codec) EncodeFrame(f Frame) ([]byte, error) {
	if err := Validate(f); err != nil {
		return nil, err
	}
	w := wireFrame{
		BaseFrame: BaseFrame{
			ID:              f.FrameID(),
			Session:         f.SessionID(),
			K:               f.Kind(),
			Auth:            f.Author(),
			At:              f.OccurredAt(),
			FromSession:     f.FromSessionID(),
			FromParticipant: f.FromParticipantID(),
			RequestID:       f.RequestIDValue(),
		},
		Payload: f.payload(),
	}
	return json.Marshal(w)
}

// EncodePayload returns just the payload bytes — used when the
// envelope columns are stored separately from the JSON payload column
// (the *RuntimeStoreLocal layout).
func (c *Codec) EncodePayload(f Frame) ([]byte, error) {
	if err := Validate(f); err != nil {
		return nil, err
	}
	return json.Marshal(f.payload())
}

// DecodeFrame parses the canonical wire form back into a typed Frame.
func (c *Codec) DecodeFrame(data []byte) (Frame, error) {
	var env rawEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: envelope: %v", ErrInvalidPayload, err)
	}
	return c.materialise(env.BaseFrame, env.Payload)
}

// DecodePayload reconstructs a Frame given an envelope and the raw
// payload bytes (the persistence-row code path).
func (c *Codec) DecodePayload(base BaseFrame, payload []byte) (Frame, error) {
	return c.materialise(base, payload)
}

func (c *Codec) materialise(base BaseFrame, payload []byte) (Frame, error) {
	switch base.K {
	case KindUserMessage:
		var p UserMessagePayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &UserMessage{BaseFrame: base, Payload: p}, nil
	case KindAgentMessage:
		var p AgentMessagePayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &AgentMessage{BaseFrame: base, Payload: p}, nil
	case KindReasoning:
		var p ReasoningPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &Reasoning{BaseFrame: base, Payload: p}, nil
	case KindToolCall:
		var p ToolCallPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &ToolCall{BaseFrame: base, Payload: p}, nil
	case KindToolResult:
		var p ToolResultPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &ToolResult{BaseFrame: base, Payload: p}, nil
	case KindSlashCommand:
		var p SlashCommandPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &SlashCommand{BaseFrame: base, Payload: p}, nil
	case KindCancel:
		var p CancelPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &Cancel{BaseFrame: base, Payload: p}, nil
	case KindSessionOpened:
		var p SessionOpenedPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &SessionOpened{BaseFrame: base, Payload: p}, nil
	case KindSessionClosed:
		var p SessionClosedPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &SessionClosed{BaseFrame: base, Payload: p}, nil
	case KindHeartbeat:
		var p HeartbeatPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &Heartbeat{BaseFrame: base, Payload: p}, nil
	case KindError:
		var p ErrorPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &Error{BaseFrame: base, Payload: p}, nil
	case KindSystemMarker:
		var p SystemMarkerPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &SystemMarker{BaseFrame: base, Payload: p}, nil
	case KindSubagentStarted:
		var p SubagentStartedPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &SubagentStarted{BaseFrame: base, Payload: p}, nil
	case KindSubagentResult:
		var p SubagentResultPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &SubagentResult{BaseFrame: base, Payload: p}, nil
	case KindPlanOp:
		var p PlanOpPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &PlanOp{BaseFrame: base, Payload: p}, nil
	case KindWhiteboardOp:
		var p WhiteboardOpPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &WhiteboardOp{BaseFrame: base, Payload: p}, nil
	case KindWhiteboardMessage:
		var p WhiteboardMessagePayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &WhiteboardMessage{BaseFrame: base, Payload: p}, nil
	case KindSessionTerminated:
		var p SessionTerminatedPayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &SessionTerminated{BaseFrame: base, Payload: p}, nil
	case KindSystemMessage:
		var p SystemMessagePayload
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return &SystemMessage{BaseFrame: base, Payload: p}, nil
	}
	// Phase 2 (R-Plan-26 / FR-024): non-empty unknown kinds round-trip
	// as *OpaqueFrame so future-phase variants survive the wire even
	// when the running binary doesn't act on them. An empty kind is
	// still treated as a malformed envelope.
	if base.K == "" {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, base.K)
	}
	return newOpaqueFrame(base, string(base.K), append(json.RawMessage(nil), payload...)), nil
}

func unmarshalPayload(raw []byte, into any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}
	return nil
}
