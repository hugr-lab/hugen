package skill

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// TestCallLoad_EmitsExtensionFrame asserts the load tool path
// pushes an OpLoad ExtensionFrame onto SessionState.Emit so
// Recovery on a later restart can replay it.
func TestCallLoad_EmitsExtensionFrame(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha": []byte(inlineAlphaManifest),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-emit")
	state := fixture.NewTestSessionState("ses-emit").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if _, err := ext.Call(newCallCtx(state), "skill:load", json.RawMessage(`{"name":"alpha"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	emitted := state.Emitted()
	if len(emitted) != 1 {
		t.Fatalf("emitted = %d, want 1", len(emitted))
	}
	ef, ok := emitted[0].(*protocol.ExtensionFrame)
	if !ok {
		t.Fatalf("frame type = %T, want *ExtensionFrame", emitted[0])
	}
	if ef.Payload.Extension != providerName {
		t.Errorf("extension = %q, want %q", ef.Payload.Extension, providerName)
	}
	if ef.Payload.Op != OpLoad {
		t.Errorf("op = %q, want %q", ef.Payload.Op, OpLoad)
	}
	if ef.Payload.Category != protocol.CategoryOp {
		t.Errorf("category = %q, want %q", ef.Payload.Category, protocol.CategoryOp)
	}
	var data LoadOpData
	if err := json.Unmarshal(ef.Payload.Data, &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data.Name != "alpha" {
		t.Errorf("data.Name = %q", data.Name)
	}
	if ef.Author().ID != "agent-emit" {
		t.Errorf("author = %+v", ef.Author())
	}
}

// TestCallUnload_EmitsExtensionFrame asserts the unload path
// pushes the matching unload op.
func TestCallUnload_EmitsExtensionFrame(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha": []byte(inlineAlphaManifest),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-u").WithDepth(2)
	_ = ext.InitState(ctx, state)
	_, _ = ext.Call(newCallCtx(state), "skill:load", json.RawMessage(`{"name":"alpha"}`))
	if _, err := ext.Call(newCallCtx(state), "skill:unload", json.RawMessage(`{"name":"alpha"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	emitted := state.Emitted()
	if len(emitted) != 2 {
		t.Fatalf("emitted = %d, want 2", len(emitted))
	}
	ef, ok := emitted[1].(*protocol.ExtensionFrame)
	if !ok {
		t.Fatalf("frame type = %T", emitted[1])
	}
	if ef.Payload.Op != OpUnload {
		t.Errorf("op = %q, want %q", ef.Payload.Op, OpUnload)
	}
}

// TestRecover_LoadsThenUnloadsRebuildsState replays a load+unload
// sequence into a fresh SkillManager session and asserts the
// final loaded set matches.
func TestRecover_LoadsThenUnloadsRebuildsState(t *testing.T) {
	ctx := context.Background()
	mgrSt := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha": []byte(inlineAlphaManifest),
	}})
	mgr := skillpkg.NewSkillManager(mgrSt, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-rec").WithDepth(2)
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	rows := []store.EventRow{
		newSkillEventRow(t, OpLoad, "alpha"),
	}
	if err := ext.Recover(ctx, state, rows); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	loaded := FromState(state).LoadedNames(ctx)
	if len(loaded) != 1 || loaded[0] != "alpha" {
		t.Fatalf("loaded = %v, want [alpha]", loaded)
	}

	// Now replay an unload event — final state should be empty.
	rows = append(rows, newSkillEventRow(t, OpUnload, "alpha"))
	state2 := fixture.NewTestSessionState("ses-rec2").WithDepth(2)
	_ = ext.InitState(ctx, state2)
	if err := ext.Recover(ctx, state2, rows); err != nil {
		t.Fatalf("Recover 2: %v", err)
	}
	if got := FromState(state2).LoadedNames(ctx); len(got) != 0 {
		t.Errorf("after replay, loaded = %v, want empty", got)
	}
}

// TestRecover_IgnoresNonSkillEvents asserts Recovery skips
// ExtensionFrame rows whose Extension is not "skill", as well as
// non-ExtensionFrame rows. The skill set must stay clean.
func TestRecover_IgnoresNonSkillEvents(t *testing.T) {
	ctx := context.Background()
	mgrSt := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{}})
	mgr := skillpkg.NewSkillManager(mgrSt, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-skip").WithDepth(2)
	_ = ext.InitState(ctx, state)

	rows := []store.EventRow{
		// other extension, same op string — must be skipped
		{
			EventType: string(protocol.KindExtensionFrame),
			Metadata: map[string]any{
				"extension": "plan",
				"category":  string(protocol.CategoryOp),
				"op":        OpLoad,
				"data":      json.RawMessage(`{"name":"alpha"}`),
			},
		},
		// non-ExtensionFrame — must be skipped
		{EventType: string(protocol.KindUserMessage)},
	}
	if err := ext.Recover(ctx, state, rows); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := FromState(state).LoadedNames(ctx); len(got) != 0 {
		t.Errorf("expected nothing loaded, got %v", got)
	}
}

// TestCloseSession_DeregistersFromManagerBroadcast asserts
// CloseSession deregisters the handle from the manager's
// broadcast list — Refresh after close stops calling into this
// session's OnSkillRefreshed. The loaded set on the handle is
// not erased (it's memory that GCs with the SessionState); the
// teardown contract is "stop receiving cross-session events".
func TestCloseSession_DeregistersFromManagerBroadcast(t *testing.T) {
	ctx := context.Background()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha": []byte(inlineAlphaManifest),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-cl").WithDepth(2)
	_ = ext.InitState(ctx, state)
	if err := FromState(state).Load(ctx, "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := ext.CloseSession(ctx, state); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	// Refresh after CloseSession must NOT update the handle's copy
	// (it deregistered from the broadcast list); pre-close gen is
	// preserved.
	preGen := FromState(state).gen
	if _, err := mgr.Refresh(ctx, "alpha"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if FromState(state).gen != preGen {
		t.Errorf("handle gen mutated after close: %d → %d (deregister failed)", preGen, FromState(state).gen)
	}
}

// TestDecodeSkillOp_RoundTripsCodecShape asserts the decoder
// understands the EventRow shape session.emit (via
// store.FrameToEventRow) actually writes. Goes through the live
// codec instead of the hand-built Metadata map the other tests
// use, so the recovery decoder stays in lockstep with the
// persistence path.
func TestDecodeSkillOp_RoundTripsCodecShape(t *testing.T) {
	frame, err := newLoadFrame("ses-rt", protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent}, "alpha")
	if err != nil {
		t.Fatalf("newLoadFrame: %v", err)
	}
	row, _, err := store.FrameToEventRow(frame, "a1")
	if err != nil {
		t.Fatalf("FrameToEventRow: %v", err)
	}
	op, name, ok := decodeSkillOp(row)
	if !ok {
		t.Fatalf("decodeSkillOp: not ok; row.Metadata = %#v", row.Metadata)
	}
	if op != OpLoad || name != "alpha" {
		t.Errorf("decodeSkillOp = (%q, %q), want (%q, %q)", op, name, OpLoad, "alpha")
	}
}

// newSkillEventRow builds an EventRow that mirrors what
// session.emit → store.FrameToEventRow produces for an
// ExtensionFrame: flat metadata keys mirroring
// ExtensionFramePayload fields.
func newSkillEventRow(t *testing.T, op, name string) store.EventRow {
	t.Helper()
	var data []byte
	var err error
	switch op {
	case OpLoad:
		data, err = json.Marshal(LoadOpData{Name: name})
	case OpUnload:
		data, err = json.Marshal(UnloadOpData{Name: name})
	}
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return store.EventRow{
		EventType: string(protocol.KindExtensionFrame),
		Metadata: map[string]any{
			"extension": providerName,
			"category":  string(protocol.CategoryOp),
			"op":        op,
			"data":      json.RawMessage(data),
		},
	}
}
