package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// runCmd dispatches the named slash command on the extension.
func runCmd(t *testing.T, e *Extension, state extension.SessionState, name string, args []string) []protocol.Frame {
	t.Helper()
	for _, c := range e.Commands() {
		if c.Name != name {
			continue
		}
		env := extension.CommandContext{AgentAuthor: extension.AgentParticipant("agent-x")}
		frames, err := c.Handler(context.Background(), state, env, args)
		if err != nil {
			t.Fatalf("/%s: %v", name, err)
		}
		return frames
	}
	t.Fatalf("command %q not registered", name)
	return nil
}

func sysContent(t *testing.T, f protocol.Frame) string {
	t.Helper()
	sm, ok := f.(*protocol.SystemMessage)
	if !ok {
		t.Fatalf("frame %T is not a SystemMessage", f)
	}
	return sm.Payload.Content
}

func TestCmdArtifacts_EmptyThenPopulated(t *testing.T) {
	e, state, _ := newExtFixture(t)
	out := runCmd(t, e, state, "artifacts", nil)
	if !strings.Contains(sysContent(t, out[0]), "No artifacts") {
		t.Errorf("empty listing = %q", sysContent(t, out[0]))
	}
	// Publish one directly through the store, then list shows it + a path.
	sa := FromState(state)
	if _, err := e.store.Register(sa.rootID, src(t, "road.md", "x"), "", "", false); err != nil {
		t.Fatal(err)
	}
	out = runCmd(t, e, state, "artifacts", nil)
	body := sysContent(t, out[0])
	if !strings.Contains(body, "road.md") || !strings.Contains(body, "/open") {
		t.Errorf("populated listing missing id/hint: %q", body)
	}
}

func TestCmdAttach(t *testing.T) {
	e, state, _ := newExtFixture(t)
	// A host file outside the workspace — /attach takes any host path.
	host := filepath.Join(t.TempDir(), "upload.csv")
	if err := os.WriteFile(host, []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runCmd(t, e, state, "attach", []string{host})
	if len(out) != 2 {
		t.Fatalf("attach returned %d frames, want 2 (uploaded frame + confirmation)", len(out))
	}
	ef, ok := out[0].(*protocol.ExtensionFrame)
	if !ok || ef.Payload.Extension != "artifact" || ef.Payload.Op != OpUploaded {
		t.Fatalf("first frame is not an artifact_uploaded ExtensionFrame: %#v", out[0])
	}
	if !strings.Contains(sysContent(t, out[1]), "upload.csv") {
		t.Errorf("confirmation missing filename: %q", sysContent(t, out[1]))
	}
	// The artifact landed in the store.
	sa := FromState(state)
	refs, _ := e.store.List(sa.rootID)
	if len(refs) != 1 || refs[0].ID != "upload.csv" {
		t.Errorf("attach did not register the file: %+v", refs)
	}
}

func TestCmdAttach_Usage(t *testing.T) {
	e, state, _ := newExtFixture(t)
	out := runCmd(t, e, state, "attach", nil)
	if _, ok := out[0].(*protocol.Error); !ok {
		t.Errorf("missing path should yield an Error frame, got %T", out[0])
	}
}

// TestCmdAttach_ExpandsVar verifies /attach expands a $VAR before
// resolving (F9) — the path reaches Ingest fully resolved, not as a
// literal "$VAR/…" joined onto the process cwd.
func TestCmdAttach_ExpandsVar(t *testing.T) {
	e, state, _ := newExtFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "v.csv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTIFACT_ATTACH_DIR", dir)
	out := runCmd(t, e, state, "attach", []string{"$ARTIFACT_ATTACH_DIR/v.csv"})
	if _, ok := out[0].(*protocol.ExtensionFrame); !ok {
		t.Fatalf("attach via $VAR did not ingest, got %T", out[0])
	}
	sa := FromState(state)
	if refs, _ := e.store.List(sa.rootID); len(refs) != 1 || refs[0].ID != "v.csv" {
		t.Errorf("attach via $VAR did not register: %+v", refs)
	}
}

// TestExpandPath covers the F9 path-expansion helper: a leading ~ /
// ~/ → home, $VAR / ${VAR} → env, absolute + relative pass through,
// and a mid-path ~ stays literal.
func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	t.Setenv("ARTIFACT_TEST_VAR", "/tmp/xyz")
	cases := []struct{ in, want string }{
		{"/abs/path", "/abs/path"},
		{"rel/path", "rel/path"},
		{"~", home},
		{"~/sub/file", filepath.Join(home, "sub", "file")},
		{"$ARTIFACT_TEST_VAR/f", "/tmp/xyz/f"},
		{"${ARTIFACT_TEST_VAR}/f", "/tmp/xyz/f"},
		{"a~b", "a~b"},
	}
	for _, c := range cases {
		if got := expandPath(c.in); got != c.want {
			t.Errorf("expandPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCmdOpen(t *testing.T) {
	e, state, _ := newExtFixture(t)
	sa := FromState(state)
	if _, err := e.store.Register(sa.rootID, src(t, "report.md", "hi"), "", "", false); err != nil {
		t.Fatal(err)
	}
	// Existing artifact → a SystemMessage (either "Opening…" or the
	// graceful "could not auto-open" fallback — never an Error).
	out := runCmd(t, e, state, "open", []string{"report.md"})
	if _, ok := out[0].(*protocol.SystemMessage); !ok {
		t.Errorf("open existing should be a SystemMessage, got %T", out[0])
	}
	// Missing artifact → Error frame.
	out = runCmd(t, e, state, "open", []string{"ghost.md"})
	if _, ok := out[0].(*protocol.Error); !ok {
		t.Errorf("open missing should be an Error, got %T", out[0])
	}
}

func TestIngest(t *testing.T) {
	store, _ := newStore(t, 0, 0)
	e := NewExtension(store, "agent-x", discardLog(t))
	ref, err := e.Ingest("ses-root", src(t, "data.bin", "one"), "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if ref.ID != "data.bin" {
		t.Errorf("ref id = %q", ref.ID)
	}
	// Re-ingest the same name overwrites in place (uploads aren't guarded).
	ref2, err := e.Ingest("ses-root", src(t, "data.bin", "longer-second"), "")
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if ref2.Size != int64(len("longer-second")) {
		t.Errorf("re-ingest size = %d", ref2.Size)
	}
}
