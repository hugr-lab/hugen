package tui

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// digestPayloadShape mirrors pkg/extension/compactor.DigestPayload
// just enough to round-trip a digest_set body in TUI tests.
type digestPayloadShape struct {
	Version         int              `json:"version"`
	Iteration       int              `json:"iteration"`
	CutoffSeq       int64            `json:"cutoff_seq"`
	KeptVerbatim    []map[string]any `json:"kept_verbatim,omitempty"`
	SummaryBlocks   []map[string]any `json:"summary_blocks,omitempty"`
	UIMarkerEnabled *bool            `json:"ui_marker_enabled,omitempty"`
}

func mustMarshalDigest(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestFormatCompactorMarker_HappyPath asserts the rendered marker
// line shape — the spec mandates "history compacted (iter N, M
// msgs)" where M is the kept_verbatim count.
func TestFormatCompactorMarker_HappyPath(t *testing.T) {
	payload := digestPayloadShape{
		Version:   1,
		Iteration: 2,
		KeptVerbatim: []map[string]any{
			{"kind": "user_message", "seq": 1, "text": "hi"},
			{"kind": "user_message", "seq": 3, "text": "follow"},
			{"kind": "agent_message", "seq": 4, "text": "ok"},
		},
	}
	got := formatCompactorMarker(mustMarshalDigest(t, payload))
	if !strings.Contains(got, "iter 2") {
		t.Errorf("missing iter 2 in marker: %q", got)
	}
	if !strings.Contains(got, "3 msgs") {
		t.Errorf("missing 3 msgs in marker: %q", got)
	}
	if !strings.HasPrefix(got, "─") {
		t.Errorf("marker should start with horizontal-rule glyph; got %q", got)
	}
}

// TestFormatCompactorMarker_FallsBackToSummaryBlocks asserts the
// edge case where the digest has no kept_verbatim (e.g. after
// cap-driven collapse rolled everything into summary blocks).
func TestFormatCompactorMarker_FallsBackToSummaryBlocks(t *testing.T) {
	payload := digestPayloadShape{
		Version:   1,
		Iteration: 5,
		SummaryBlocks: []map[string]any{
			{"iter": 1}, {"iter": 2}, {"iter": 3}, {"iter": 4}, {"iter": 5},
		},
	}
	got := formatCompactorMarker(mustMarshalDigest(t, payload))
	if !strings.Contains(got, "5 msgs") {
		t.Errorf("fallback count not used; got %q", got)
	}
}

// TestFormatCompactorMarker_InvalidJSONReturnsEmpty verifies the
// decoder is non-fatal — adapters skip the marker on malformed
// input rather than crashing.
func TestFormatCompactorMarker_InvalidJSONReturnsEmpty(t *testing.T) {
	if got := formatCompactorMarker([]byte("not json")); got != "" {
		t.Fatalf("invalid JSON should yield empty marker; got %q", got)
	}
	if got := formatCompactorMarker(nil); got != "" {
		t.Fatalf("nil data should yield empty marker; got %q", got)
	}
}

// TestCompactorMarkerEnabledFromStatus_DefaultsTrue verifies the
// default-on discipline: a status without an extensions block (no
// digest fired yet) keeps the marker enabled.
func TestCompactorMarkerEnabledFromStatus_DefaultsTrue(t *testing.T) {
	if !compactorMarkerEnabledFromStatus(nil) {
		t.Errorf("nil status: marker should default to enabled")
	}
	if !compactorMarkerEnabledFromStatus(&liveviewStatus{}) {
		t.Errorf("empty status: marker should default to enabled")
	}
	if !compactorMarkerEnabledFromStatus(&liveviewStatus{
		Extensions: map[string]json.RawMessage{"plan": []byte(`{"foo":"bar"}`)},
	}) {
		t.Errorf("status without compactor entry: marker should default to enabled")
	}
}

// TestCompactorMarkerEnabledFromStatus_OperatorDisabled verifies
// that a status frame carrying ui_marker_enabled=false flips the
// adapter into suppression mode.
func TestCompactorMarkerEnabledFromStatus_OperatorDisabled(t *testing.T) {
	st := &liveviewStatus{
		Extensions: map[string]json.RawMessage{
			"compactor": json.RawMessage(`{"iteration":1,"ui_marker_enabled":false}`),
		},
	}
	if compactorMarkerEnabledFromStatus(st) {
		t.Errorf("ui_marker_enabled=false: marker should be suppressed")
	}
}

func TestCompactorMarkerEnabledFromStatus_OperatorEnabled(t *testing.T) {
	st := &liveviewStatus{
		Extensions: map[string]json.RawMessage{
			"compactor": json.RawMessage(`{"iteration":1,"ui_marker_enabled":true}`),
		},
	}
	if !compactorMarkerEnabledFromStatus(st) {
		t.Errorf("ui_marker_enabled=true: marker should render")
	}
}

// TestTab_HandleDigestSetFrame_AppendsSystemSpan verifies the
// end-to-end TUI path: a compactor/digest_set ExtensionFrame
// produces a system span carrying the marker text in the chat
// buffer. Mirrors the SystemMarker rendering discipline so the
// marker renders inline with the rest of the transcript.
func TestTab_HandleDigestSetFrame_AppendsSystemSpan(t *testing.T) {
	tb := newTab("ses-marker", protocol.ParticipantInfo{ID: "u"},
		func(protocol.Frame) error { return nil }, slog.Default())
	tb.applyGeometry(80, 20, 100)

	payload := digestPayloadShape{
		Version:   1,
		Iteration: 1,
		KeptVerbatim: []map[string]any{
			{"kind": "user_message", "seq": 1, "text": "hi"},
		},
	}
	frame := protocol.NewExtensionFrame(
		"ses-marker", protocol.ParticipantInfo{},
		"compactor", protocol.CategoryOp, "digest_set",
		mustMarshalDigest(t, payload),
	)
	tb.handleFrame(frame)

	// A system span should have landed on the buffer.
	var marker string
	for _, sp := range tb.chat.spans {
		if sp.kind == spanSystem && strings.Contains(sp.text, "history compacted") {
			marker = sp.text
			break
		}
	}
	if marker == "" {
		t.Fatalf("digest_set did not append a system marker span; spans=%+v", tb.chat.spans)
	}
	if !strings.Contains(marker, "iter 1") || !strings.Contains(marker, "1 msgs") {
		t.Errorf("marker text wrong: %q", marker)
	}
}

// TestTab_HandleDigestSetFrame_SuppressedByPayload verifies the
// authoritative path: the digest_set payload itself carries
// `ui_marker_enabled: false` (operator set
// `compactor.ui_marker.enabled: false` in agent_config.yaml; the
// compactor extension echoes the resolved flag into every
// DigestPayload). The marker must NOT appear in the chat buffer,
// independent of any liveview/status state — the payload is the
// single source of truth so the very first compaction respects
// the operator preference without a status round trip.
func TestTab_HandleDigestSetFrame_SuppressedByPayload(t *testing.T) {
	tb := newTab("ses-suppressed", protocol.ParticipantInfo{ID: "u"},
		func(protocol.Frame) error { return nil }, slog.Default())
	tb.applyGeometry(80, 20, 100)

	falseVal := false
	payload := digestPayloadShape{
		Version:   1,
		Iteration: 1,
		KeptVerbatim: []map[string]any{
			{"kind": "user_message", "seq": 1, "text": "hi"},
		},
		UIMarkerEnabled: &falseVal,
	}
	digestFrame := protocol.NewExtensionFrame(
		"ses-suppressed", protocol.ParticipantInfo{},
		"compactor", protocol.CategoryOp, "digest_set",
		mustMarshalDigest(t, payload),
	)
	tb.handleFrame(digestFrame)

	for _, sp := range tb.chat.spans {
		if strings.Contains(sp.text, "history compacted") {
			t.Fatalf("marker drawn despite ui_marker_enabled=false: %q", sp.text)
		}
	}
}
