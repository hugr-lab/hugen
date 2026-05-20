package console

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// digestPayloadShape mirrors pkg/extension/compactor.DigestPayload
// just enough to round-trip a digest_set frame body in tests. We
// intentionally keep the shape narrow: the adapter only reads the
// fields named here, so a divergence in the producer would surface
// as a test failure at the count assertion rather than a missing
// field at runtime.
type digestPayloadShape struct {
	Version       int             `json:"version"`
	Iteration     int             `json:"iteration"`
	CutoffSeq     int64           `json:"cutoff_seq"`
	KeptVerbatim  []map[string]any `json:"kept_verbatim,omitempty"`
	SummaryBlocks []map[string]any `json:"summary_blocks,omitempty"`
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newTestAdapter(out *bytes.Buffer) *Adapter {
	return &Adapter{out: out, err: out, closed: make(chan struct{})}
}

// TestRenderCompactorDigestSet_DrawsMarker verifies the happy
// path: a compactor/digest_set frame with N kept_verbatim entries
// produces the "─── history compacted (iter X, N msgs) ───" line.
func TestRenderCompactorDigestSet_DrawsMarker(t *testing.T) {
	var buf bytes.Buffer
	a := newTestAdapter(&buf)

	payload := digestPayloadShape{
		Version:   1,
		Iteration: 2,
		CutoffSeq: 42,
		KeptVerbatim: []map[string]any{
			{"kind": "user_message", "seq": 1, "text": "hi"},
			{"kind": "user_message", "seq": 3, "text": "follow"},
		},
		SummaryBlocks: []map[string]any{
			{"iter": 1, "from": 0, "to": 20, "text": "block1"},
		},
	}
	frame := protocol.NewExtensionFrame(
		"ses-1", protocol.ParticipantInfo{},
		"compactor", protocol.CategoryOp, "digest_set",
		mustMarshal(t, payload),
	)
	a.render(frame)

	got := buf.String()
	if !strings.Contains(got, "history compacted") {
		t.Fatalf("output missing marker text; got %q", got)
	}
	if !strings.Contains(got, "iter 2") {
		t.Errorf("output missing iter 2; got %q", got)
	}
	if !strings.Contains(got, "2 msgs") {
		t.Errorf("output missing 2 msgs (kept_verbatim count); got %q", got)
	}
}

// TestRenderCompactorDigestSet_FallsBackToBlocksCount verifies the
// edge case where kept_verbatim is empty (e.g. cap-driven collapse
// rolled everything into summary blocks). The marker still shows a
// non-zero count by falling back to summary_blocks.
func TestRenderCompactorDigestSet_FallsBackToBlocksCount(t *testing.T) {
	var buf bytes.Buffer
	a := newTestAdapter(&buf)

	payload := digestPayloadShape{
		Version:   1,
		Iteration: 3,
		SummaryBlocks: []map[string]any{
			{"iter": 1, "text": "b1"},
			{"iter": 2, "text": "b2"},
			{"iter": 3, "text": "b3"},
		},
	}
	frame := protocol.NewExtensionFrame(
		"ses-2", protocol.ParticipantInfo{},
		"compactor", protocol.CategoryOp, "digest_set",
		mustMarshal(t, payload),
	)
	a.render(frame)

	if got := buf.String(); !strings.Contains(got, "3 msgs") {
		t.Errorf("blocks-only payload: got %q, want fallback to 3 msgs", got)
	}
}

// TestRenderCompactorDigestSet_SuppressedWhenMarkerDisabled
// verifies the operator-toggle path: when a previous
// liveview/status frame announced ui_marker_enabled=false, the
// subsequent digest_set frame must render nothing.
func TestRenderCompactorDigestSet_SuppressedWhenMarkerDisabled(t *testing.T) {
	var buf bytes.Buffer
	a := newTestAdapter(&buf)

	// Simulate a prior liveview/status frame carrying the
	// compactor projection with ui_marker_enabled=false.
	statusBody := map[string]any{
		"extensions": map[string]any{
			"compactor": map[string]any{
				"ui_marker_enabled": false,
			},
		},
	}
	statusFrame := protocol.NewExtensionFrame(
		"ses-3", protocol.ParticipantInfo{},
		"liveview", protocol.CategoryMarker, "status",
		mustMarshal(t, statusBody),
	)
	a.render(statusFrame)

	// Then a digest_set arrives — the marker must NOT appear.
	payload := digestPayloadShape{
		Version:   1,
		Iteration: 1,
		KeptVerbatim: []map[string]any{
			{"kind": "user_message", "seq": 1, "text": "hi"},
		},
	}
	digestFrame := protocol.NewExtensionFrame(
		"ses-3", protocol.ParticipantInfo{},
		"compactor", protocol.CategoryOp, "digest_set",
		mustMarshal(t, payload),
	)
	a.render(digestFrame)

	if got := buf.String(); strings.Contains(got, "history compacted") {
		t.Errorf("marker drawn despite ui_marker_enabled=false; output = %q", got)
	}
}

// TestRenderCompactorDigestSet_IgnoresOtherExtensions verifies the
// dispatch discipline: ExtensionFrames from siblings (plan,
// whiteboard, notepad, …) do NOT trigger compactor rendering.
func TestRenderCompactorDigestSet_IgnoresOtherExtensions(t *testing.T) {
	var buf bytes.Buffer
	a := newTestAdapter(&buf)

	other := protocol.NewExtensionFrame(
		"ses-4", protocol.ParticipantInfo{},
		"plan", protocol.CategoryOp, "set",
		mustMarshal(t, map[string]any{"text": "step 1"}),
	)
	a.render(other)
	if got := buf.String(); got != "" {
		t.Errorf("plan/set should be silent; got %q", got)
	}
}

// TestUpdateCompactorMarkerFlag_DefaultsOnAbsentField verifies the
// "missing field → default true" discipline: a liveview/status
// payload without a compactor entry (no digest fired yet) leaves
// the marker enabled.
func TestUpdateCompactorMarkerFlag_DefaultsOnAbsentField(t *testing.T) {
	var buf bytes.Buffer
	a := newTestAdapter(&buf)

	statusBody := map[string]any{
		"session_id": "ses-5",
		"depth":      0,
		// no extensions block at all
	}
	statusFrame := protocol.NewExtensionFrame(
		"ses-5", protocol.ParticipantInfo{},
		"liveview", protocol.CategoryMarker, "status",
		mustMarshal(t, statusBody),
	)
	a.render(statusFrame)

	if a.compactorMarkerDisabled.Load() {
		t.Errorf("absent extensions.compactor: marker should default to enabled (not disabled)")
	}
}
