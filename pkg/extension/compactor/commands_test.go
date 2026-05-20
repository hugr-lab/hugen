package compactor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// envForCommands builds a minimal CommandContext with stable
// participants the tests can assert against. The actual IDs do
// not matter — only that the handler stamps them onto returned
// frames.
func envForCommands() extension.CommandContext {
	return extension.CommandContext{
		Author:      protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser, Name: "u1"},
		AgentAuthor: protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent, Name: "hugen"},
	}
}

func TestCompactorCommands_RegistersOneNamedCompactor(t *testing.T) {
	e := newTestExtension(t)
	cmds := e.Commands()
	if len(cmds) != 1 {
		t.Fatalf("Commands() returned %d entries, want 1", len(cmds))
	}
	if cmds[0].Name != "compactor" {
		t.Fatalf("command name = %q, want %q", cmds[0].Name, "compactor")
	}
	if cmds[0].Description == "" {
		t.Errorf("command description should be non-empty")
	}
}

func TestCompactorCommand_NoArgs_PrintsUsage(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-cmd-1")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	frames, err := e.cmdCompactor(context.Background(), st, envForCommands(), nil)
	if err != nil {
		t.Fatalf("cmdCompactor: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	errFrame, ok := frames[0].(*protocol.Error)
	if !ok {
		t.Fatalf("frame[0] type = %T, want *protocol.Error", frames[0])
	}
	if !strings.Contains(errFrame.Payload.Message, "usage:") {
		t.Errorf("error message = %q, want usage hint", errFrame.Payload.Message)
	}
}

func TestCompactorCommand_UnknownSub_PrintsUsage(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-cmd-2")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	frames, _ := e.cmdCompactor(context.Background(), st, envForCommands(), []string{"oops"})
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	errFrame, ok := frames[0].(*protocol.Error)
	if !ok {
		t.Fatalf("frame[0] type = %T, want *protocol.Error", frames[0])
	}
	if !strings.Contains(errFrame.Payload.Message, "unknown") {
		t.Errorf("error message = %q, want 'unknown' substring", errFrame.Payload.Message)
	}
}

func TestCompactorStatus_NoDigest(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-cmd-status-empty")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	frames, err := e.cmdCompactor(context.Background(), st, envForCommands(), []string{"status"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	sys, ok := frames[0].(*protocol.SystemMessage)
	if !ok {
		t.Fatalf("frame[0] type = %T, want *protocol.SystemMessage", frames[0])
	}
	if !strings.Contains(sys.Payload.Content, "no digest") {
		t.Errorf("status line = %q, want 'no digest' substring", sys.Payload.Content)
	}
}

func TestCompactorStatus_WithDigest(t *testing.T) {
	e := newTestExtension(t)
	st := newFakeState("ses-cmd-status-full")
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	FromState(st).SetDigest(&DigestPayload{
		Version:       CurrentPayloadVersion,
		Iteration:     3,
		CutoffSeq:     157,
		SummaryBlocks: make([]SummaryBlock, 2),
		KeptVerbatim:  make([]KeptSection, 7),
		BuiltAt:       now,
	})
	frames, _ := e.cmdCompactor(context.Background(), st, envForCommands(), []string{"status"})
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	sys, ok := frames[0].(*protocol.SystemMessage)
	if !ok {
		t.Fatalf("frame[0] type = %T, want *protocol.SystemMessage", frames[0])
	}
	line := sys.Payload.Content
	for _, want := range []string{"iteration 3", "cutoff seq 157", "2 blocks", "7 kept"} {
		if !strings.Contains(line, want) {
			t.Errorf("status line = %q, missing substring %q", line, want)
		}
	}
}

// emittingStateFake extends fakeState by capturing frames the
// extension Emits via state.Emit — needed to verify the reset
// command's digest_clear ExtensionFrame goes out.
type emittingStateFake struct {
	fakeState
	emitted []protocol.Frame
}

func (s *emittingStateFake) Emit(_ context.Context, f protocol.Frame) error {
	s.emitted = append(s.emitted, f)
	return nil
}

func TestCompactorReset_EmitsDigestClearAndDropsState(t *testing.T) {
	e := newTestExtension(t)
	st := &emittingStateFake{fakeState: fakeState{id: "ses-cmd-reset"}}
	if err := e.InitState(context.Background(), st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	FromState(st).SetDigest(&DigestPayload{
		Version:   CurrentPayloadVersion,
		Iteration: 1,
		CutoffSeq: 42,
		BuiltAt:   time.Now(),
	})

	frames, err := e.cmdCompactor(context.Background(), st, envForCommands(), []string{"reset"})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	// Reset returns a system_marker; the digest_clear is on the
	// session's Emit stream.
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if _, ok := frames[0].(*protocol.SystemMarker); !ok {
		t.Fatalf("frame[0] type = %T, want *protocol.SystemMarker", frames[0])
	}

	// digest_clear ExtensionFrame emitted to the session.
	if len(st.emitted) != 1 {
		t.Fatalf("emitted = %d, want 1 (digest_clear)", len(st.emitted))
	}
	ef, ok := st.emitted[0].(*protocol.ExtensionFrame)
	if !ok {
		t.Fatalf("emitted[0] type = %T, want *protocol.ExtensionFrame", st.emitted[0])
	}
	if ef.Payload.Extension != providerName || ef.Payload.Op != OpDigestClear {
		t.Fatalf("emitted ExtensionFrame = %s/%s, want %s/%s",
			ef.Payload.Extension, ef.Payload.Op, providerName, OpDigestClear)
	}
	// Payload data must be valid JSON so the codec round-trip stays clean.
	var any map[string]any
	if err := json.Unmarshal(ef.Payload.Data, &any); err != nil {
		t.Fatalf("digest_clear data is not valid JSON: %v", err)
	}

	// In-memory state cleared.
	if FromState(st).Digest() != nil {
		t.Fatalf("digest should be nil after reset")
	}
}

func TestCompactorReset_NoStateSurfacesError(t *testing.T) {
	// State without InitState — extension reports the failure.
	e := newTestExtension(t)
	st := &emittingStateFake{fakeState: fakeState{id: "ses-cmd-reset-no-state"}}
	frames, _ := e.cmdCompactor(context.Background(), st, envForCommands(), []string{"reset"})
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if _, ok := frames[0].(*protocol.Error); !ok {
		t.Fatalf("frame[0] type = %T, want *protocol.Error", frames[0])
	}
	if len(st.emitted) != 0 {
		t.Fatalf("no Emit should have happened; got %d frames", len(st.emitted))
	}
}
