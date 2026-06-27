package artifact

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

func discardLog(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newExtFixture builds an extension over a fresh store + a root session
// whose artifact handle points at a temp workspace dir.
func newExtFixture(t *testing.T) (*Extension, *fixture.TestSessionState, string) {
	t.Helper()
	store, _ := newStore(t, 0, 0)
	e := NewExtension(store, "agent-x", discardLog(t))
	ws := t.TempDir()
	state := fixture.NewTestSessionState("ses-root")
	state.SetValue(StateKey, &sessionArtifacts{rootID: "ses-root", workspaceDir: ws})
	return e, state, ws
}

func call(t *testing.T, e *Extension, state extension.SessionState, name string, args any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := e.Call(extension.WithSessionState(context.Background(), state), name, raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var m map[string]any
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("%s result not json: %v (%s)", name, uerr, out)
	}
	return m
}

func errCode(m map[string]any) string {
	e, ok := m["error"].(map[string]any)
	if !ok {
		return ""
	}
	c, _ := e["code"].(string)
	return c
}

func wsWrite(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInitState wires the real workspace extension + a parent chain to
// verify the artifact handle snapshots the ROOT scope (depth-0 walk)
// and the session's workspace dir.
func TestInitState(t *testing.T) {
	wsRoot := t.TempDir()
	wsExt := wsext.NewExtension(wsRoot, discardLog(t))
	store, _ := newStore(t, 0, 0)
	artExt := NewExtension(store, "agent-x", discardLog(t))

	root := fixture.NewTestSessionState("ses-root")
	mission := fixture.NewTestSessionState("ses-mission").WithParent(root)
	for _, s := range []*fixture.TestSessionState{root, mission} {
		if err := wsExt.InitState(context.Background(), s); err != nil {
			t.Fatalf("workspace init: %v", err)
		}
	}
	if err := artExt.InitState(context.Background(), mission); err != nil {
		t.Fatalf("artifact init: %v", err)
	}
	sa := FromState(mission)
	if sa == nil {
		t.Fatal("no artifact handle after InitState")
	}
	if sa.rootID != "ses-root" {
		t.Errorf("rootID = %q want ses-root (depth-0 walk)", sa.rootID)
	}
	if sa.workspaceDir == "" {
		t.Error("workspaceDir not snapshotted from workspace ext")
	}
}

func TestPublishListCopyRoundTrip(t *testing.T) {
	e, state, ws := newExtFixture(t)
	wsWrite(t, ws, "report.md", "# Road report\nbody")

	pub := call(t, e, state, "artifact:publish", map[string]any{"path": "report.md"})
	if errCode(pub) != "" {
		t.Fatalf("publish error: %v", pub)
	}
	art, _ := pub["artifact"].(map[string]any)
	if art["id"] != "report.md" {
		t.Errorf("published id = %v", art["id"])
	}

	lst := call(t, e, state, "artifact:list", map[string]any{})
	arts, _ := lst["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("list = %d artifacts, want 1", len(arts))
	}

	cp := call(t, e, state, "artifact:copy", map[string]any{"id": "report.md", "path": "in/copy.md"})
	if errCode(cp) != "" {
		t.Fatalf("copy error: %v", cp)
	}
	if cp["path"] != filepath.Clean("in/copy.md") {
		t.Errorf("copy path = %v", cp["path"])
	}
	b, err := os.ReadFile(filepath.Join(ws, "in", "copy.md"))
	if err != nil || string(b) != "# Road report\nbody" {
		t.Errorf("copied bytes wrong: %q err=%v", b, err)
	}
}

// countProducedFrames returns how many artifact OpProduced
// ExtensionFrames a fixture recorded (Emit + OutboxOnly land in the
// same slice).
func countProducedFrames(frames []protocol.Frame) int {
	n := 0
	for _, f := range frames {
		ef, ok := f.(*protocol.ExtensionFrame)
		if !ok {
			continue
		}
		if ef.Payload.Extension == providerName && ef.Payload.Op == OpProduced {
			n++
		}
	}
	return n
}

// TestPublish_SubmitsRootMarkerFromSubTier is the F2 delivery half: a
// mission / worker publish records its own emit on its OWN stream AND
// hands a marker to the ROOT via Submit (root's inbox) — the cross-tier
// hop. The marker goes straight to root; the intermediate tier is
// untouched. (Surfacing the inbox marker onto the root outbox is
// HandleFrame's job, covered separately — the fixture runs no Run loop.)
func TestPublish_SubmitsRootMarkerFromSubTier(t *testing.T) {
	store, _ := newStore(t, 0, 0)
	e := NewExtension(store, "agent-x", discardLog(t))
	ws := t.TempDir()
	wsWrite(t, ws, "report.html", "<h1>modules</h1>")

	root := fixture.NewTestSessionState("ses-root")
	mission := fixture.NewTestSessionState("ses-mission").WithParent(root)
	worker := fixture.NewTestSessionState("ses-worker").WithParent(mission)
	worker.SetValue(StateKey, &sessionArtifacts{rootID: "ses-root", workspaceDir: ws})
	if worker.Depth() != 2 {
		t.Fatalf("worker depth = %d, want 2 (root→mission→worker)", worker.Depth())
	}

	pub := call(t, e, worker, "artifact:publish", map[string]any{"path": "report.html"})
	if errCode(pub) != "" {
		t.Fatalf("publish error: %v", pub)
	}

	if got := countProducedFrames(worker.Emitted()); got != 1 {
		t.Errorf("worker own emit = %d produced frames, want 1", got)
	}
	if got := countProducedFrames(root.Inbox()); got != 1 {
		t.Errorf("root inbox = %d produced frames, want 1 (the Submit cross-tier hop)", got)
	}
	if got := countProducedFrames(mission.Inbox()); got != 0 {
		t.Errorf("mission inbox = %d produced frames, want 0 (marker submitted straight to root)", got)
	}
	// The marker frame is attributed to root so Runtime.fanout delivers it
	// to the root subscriber once HandleFrame re-emits it.
	for _, f := range root.Inbox() {
		if ef, ok := f.(*protocol.ExtensionFrame); ok && ef.Payload.Op == OpProduced && ef.SessionID() != "ses-root" {
			t.Errorf("root marker attributed to %q, want ses-root", ef.SessionID())
		}
	}
}

// TestHandleFrame_SurfacesProducedMarkerOnRootOutbox is the F2 consumer
// half: the root's route loop hands a Submitted publish marker to
// HandleFrame, which re-emits it on the root outbox (OutboxOnly) so the
// adapter renders the 📎. A non-publish op is ignored.
func TestHandleFrame_SurfacesProducedMarkerOnRootOutbox(t *testing.T) {
	e, root, _ := newExtFixture(t)
	ref := protocol.ArtifactRef{ID: "report.html", Name: "report.html"}

	produced, _ := e.frameFor(root.SessionID(), OpProduced, ref)
	if err := e.HandleFrame(context.Background(), root, produced); err != nil {
		t.Fatalf("HandleFrame(produced): %v", err)
	}
	if got := countProducedFrames(root.Emitted()); got != 1 {
		t.Errorf("HandleFrame surfaced %d produced frames on the outbox, want 1", got)
	}

	// A non-publish op must NOT be surfaced.
	uploaded, _ := e.frameFor(root.SessionID(), OpUploaded, ref)
	if err := e.HandleFrame(context.Background(), root, uploaded); err != nil {
		t.Fatalf("HandleFrame(uploaded): %v", err)
	}
	if got := countProducedFrames(root.Emitted()); got != 1 {
		t.Errorf("HandleFrame surfaced a non-publish op; produced count = %d, want still 1", got)
	}
}

// TestPublish_RootTierNoSubmit guards against a double 📎: a root-tier
// publish already lands on the root outbox via its own emit, so it must
// NOT also Submit a cross-tier marker.
func TestPublish_RootTierNoSubmit(t *testing.T) {
	e, state, ws := newExtFixture(t)
	wsWrite(t, ws, "r.md", "x")
	if c := errCode(call(t, e, state, "artifact:publish", map[string]any{"path": "r.md"})); c != "" {
		t.Fatalf("publish: %s", c)
	}
	if got := countProducedFrames(state.Emitted()); got != 1 {
		t.Errorf("root publish = %d produced emits, want exactly 1 (own emit)", got)
	}
	if got := countProducedFrames(state.Inbox()); got != 0 {
		t.Errorf("root publish submitted %d markers to its own inbox, want 0 (no cross-tier hop at depth 0)", got)
	}
}

func TestPublish_CollisionAndOverwriteGuard(t *testing.T) {
	e, state, ws := newExtFixture(t)
	wsWrite(t, ws, "x.md", "one")
	if c := errCode(call(t, e, state, "artifact:publish", map[string]any{"path": "x.md"})); c != "" {
		t.Fatalf("first publish: %s", c)
	}
	// same name, no overwrite → exists
	if c := errCode(call(t, e, state, "artifact:publish", map[string]any{"path": "x.md"})); c != "exists" {
		t.Fatalf("collision code = %q want exists", c)
	}
	// overwrite WITHOUT having read the list → blocked
	if c := errCode(call(t, e, state, "artifact:publish", map[string]any{"path": "x.md", "overwrite": true})); c != "read_list_first" {
		t.Fatalf("overwrite-before-list code = %q want read_list_first", c)
	}
	// read the list → arms the guard → overwrite allowed
	_ = call(t, e, state, "artifact:list", map[string]any{})
	res := call(t, e, state, "artifact:publish", map[string]any{"path": "x.md", "overwrite": true})
	if errCode(res) != "" {
		t.Fatalf("overwrite after list: %v", res)
	}
	if res["note"] == nil {
		t.Error("overwrite should advise reviewing the list")
	}
}

func TestPublish_PathEscape(t *testing.T) {
	e, state, _ := newExtFixture(t)
	if c := errCode(call(t, e, state, "artifact:publish", map[string]any{"path": "../escape.md"})); c != "bad_request" {
		t.Errorf("escape code = %q want bad_request", c)
	}
	if c := errCode(call(t, e, state, "artifact:publish", map[string]any{"path": "/abs/path.md"})); c != "bad_request" {
		t.Errorf("absolute code = %q want bad_request", c)
	}
}

func TestCopy_NotFound(t *testing.T) {
	e, state, _ := newExtFixture(t)
	if c := errCode(call(t, e, state, "artifact:copy", map[string]any{"id": "ghost.md"})); c != "not_found" {
		t.Errorf("copy-missing code = %q want not_found", c)
	}
}

func TestDelete(t *testing.T) {
	e, state, ws := newExtFixture(t)
	wsWrite(t, ws, "z.md", "x")
	_ = call(t, e, state, "artifact:publish", map[string]any{"path": "z.md"})
	if c := errCode(call(t, e, state, "artifact:delete", map[string]any{"id": "z.md"})); c != "" {
		t.Fatalf("delete: %s", c)
	}
	if c := errCode(call(t, e, state, "artifact:delete", map[string]any{"id": "z.md"})); c != "not_found" {
		t.Errorf("re-delete code = %q want not_found", c)
	}
}

func TestCloseSession_ReapsOnlyOnRoot(t *testing.T) {
	store, _ := newStore(t, 0, 0)
	e := NewExtension(store, "agent-x", discardLog(t))
	if _, err := store.Register("ses-root", src(t, "a.md", "x"), "", "", false); err != nil {
		t.Fatal(err)
	}

	// A mission (depth 1) under the root closing → must NOT reap.
	root := fixture.NewTestSessionState("ses-root")
	mission := fixture.NewTestSessionState("ses-mission").WithParent(root)
	if err := e.CloseSession(context.Background(), mission); err != nil {
		t.Fatalf("mission close: %v", err)
	}
	if refs, _ := store.List("ses-root"); len(refs) != 1 {
		t.Fatalf("mission close reaped the root's artifacts")
	}

	// The root (depth 0) closing → reaps.
	if err := e.CloseSession(context.Background(), root); err != nil {
		t.Fatalf("root close: %v", err)
	}
	if refs, _ := store.List("ses-root"); len(refs) != 0 {
		t.Errorf("root close did not reap: %d left", len(refs))
	}
}

func TestList_Tools(t *testing.T) {
	e, _, _ := newExtFixture(t)
	tools, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"artifact:list": true, "artifact:copy": true, "artifact:publish": true, "artifact:delete": true}
	for _, tl := range tools {
		delete(want, tl.Name)
		if tl.Provider != "artifact" {
			t.Errorf("%s provider = %q", tl.Name, tl.Provider)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}
