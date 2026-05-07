package protocol

import (
	"encoding/json"
	"testing"
)

// TestCodec_RoundTrip_ExtensionFrame asserts the new envelope
// encodes + decodes losslessly: Extension/Category/Op + payload
// bytes survive the trip. Payload is opaque to the codec — it
// stays a json.RawMessage on both sides.
func TestCodec_RoundTrip_ExtensionFrame(t *testing.T) {
	codec := NewCodec()

	t.Run("typed_payload", func(t *testing.T) {
		// Extension's typed payload — codec doesn't know this struct;
		// the test marshals it before constructing the Frame, parses
		// it back after decode.
		type planSet struct {
			Text        string `json:"text"`
			CurrentStep string `json:"current_step"`
		}
		body, err := json.Marshal(planSet{Text: "investigate", CurrentStep: "scope"})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		f := NewExtensionFrame("s1", testAgent, "plan", CategoryOp, "set", body)
		f.RequestID = "req-42"

		data, err := codec.EncodeFrame(f)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		out, err := codec.DecodeFrame(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, ok := out.(*ExtensionFrame)
		if !ok {
			t.Fatalf("decoded type %T, want *ExtensionFrame", out)
		}

		if got.Kind() != KindExtensionFrame {
			t.Errorf("kind = %q, want %q", got.Kind(), KindExtensionFrame)
		}
		if got.Payload.Extension != "plan" {
			t.Errorf("extension = %q, want plan", got.Payload.Extension)
		}
		if got.Payload.Category != CategoryOp {
			t.Errorf("category = %q, want op", got.Payload.Category)
		}
		if got.Payload.Op != "set" {
			t.Errorf("op = %q, want set", got.Payload.Op)
		}
		if got.RequestIDValue() != "req-42" {
			t.Errorf("request_id lost in round-trip: %q", got.RequestIDValue())
		}

		// Decode the inner payload and confirm it's intact.
		var back planSet
		if err := json.Unmarshal(got.Payload.Data, &back); err != nil {
			t.Fatalf("unmarshal inner: %v", err)
		}
		if back.Text != "investigate" || back.CurrentStep != "scope" {
			t.Errorf("inner payload corrupted: %+v", back)
		}
	})

	t.Run("empty_payload", func(t *testing.T) {
		// whiteboard:stop carries no data — Data is nil; codec must
		// not blow up on that path.
		f := NewExtensionFrame("s1", testAgent, "whiteboard", CategoryOp, "stop", nil)

		data, err := codec.EncodeFrame(f)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		out, err := codec.DecodeFrame(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, ok := out.(*ExtensionFrame)
		if !ok {
			t.Fatalf("decoded type %T, want *ExtensionFrame", out)
		}
		if got.Payload.Extension != "whiteboard" || got.Payload.Op != "stop" {
			t.Errorf("envelope corrupted: %+v", got.Payload)
		}
		if len(got.Payload.Data) != 0 {
			t.Errorf("expected empty data, got %s", got.Payload.Data)
		}
	})

	t.Run("from_session_for_cross_session_emit", func(t *testing.T) {
		// Cross-session whiteboard write: child session emits an
		// ExtensionFrame addressed to the host parent. FromSession is
		// the child's id (envelope-level field); SessionID is the
		// host's. Both must round-trip.
		f := NewExtensionFrame("host-1", testAgent, "whiteboard", CategoryOp, "write", json.RawMessage(`{"text":"found auth_logs"}`))
		f.FromSession = "child-2"

		data, err := codec.EncodeFrame(f)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		out, err := codec.DecodeFrame(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.SessionID() != "host-1" {
			t.Errorf("session_id = %q, want host-1", out.SessionID())
		}
		if out.FromSessionID() != "child-2" {
			t.Errorf("from_session = %q, want child-2", out.FromSessionID())
		}
	})
}
