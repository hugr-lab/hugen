package notepad

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// newFixture builds an Extension + a state seeded by InitState so
// every test starts from "fresh, ready-to-call".
func newFixture(t *testing.T) (*Extension, *fixture.TestSessionState, *fixture.TestStore) {
	t.Helper()
	store := fixture.NewTestStore()
	ext := NewExtension(store, "agent-test", Config{})
	state := fixture.NewTestSessionState("ses-test")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	return ext, state, store
}

func TestExtension_Name(t *testing.T) {
	ext := NewExtension(fixture.NewTestStore(), "a1", Config{})
	if got := ext.Name(); got != "notepad" {
		t.Errorf("Name = %q, want notepad", got)
	}
}

func TestExtension_List_FourTools(t *testing.T) {
	ext := NewExtension(fixture.NewTestStore(), "a1", Config{})
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"notepad:append", "notepad:read", "notepad:search", "notepad:show"}
	if len(tools) != len(want) {
		t.Fatalf("len(tools) = %d, want %d", len(tools), len(want))
	}
	got := make(map[string]string, len(tools))
	for _, tl := range tools {
		got[tl.Name] = tl.PermissionObject
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("missing tool %q", name)
		}
	}
	if got["notepad:append"] != PermAppend {
		t.Errorf("append perm = %q, want %q", got["notepad:append"], PermAppend)
	}
}

func TestExtension_InitState_SnapshotsRootAndRole(t *testing.T) {
	store := fixture.NewTestStore()
	ext := NewExtension(store, "agent-test", Config{})
	// Construct a worker (depth 2) state with a real parent chain
	// so InitState walks to the root.
	root := fixture.NewTestSessionState("ses-root")
	mission := fixture.NewTestSessionState("ses-mission").WithParent(root)
	worker := fixture.NewTestSessionState("ses-worker").WithParent(mission)

	if err := ext.InitState(context.Background(), worker); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	np := FromState(worker)
	if np == nil {
		t.Fatal("FromState returned nil after InitState")
	}
	if np.RootID() != "ses-root" {
		t.Errorf("RootID = %q, want ses-root", np.RootID())
	}
	if np.Role() != skill.TierWorker {
		t.Errorf("Role = %q, want %q", np.Role(), skill.TierWorker)
	}
}

func TestCallAppend_Happy(t *testing.T) {
	ext, state, store := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	args, _ := json.Marshal(AppendInput{
		Content:  "remember this",
		Category: "schema-finding",
		Mission:  "exploring northwind",
	})
	out, err := ext.Call(ctx, "notepad:append", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["id"] == "" {
		t.Fatalf("empty id; out=%s", out)
	}
	if len(store.Notes) != 1 ||
		store.Notes[0].Content != "remember this" ||
		store.Notes[0].Category != "schema-finding" ||
		store.Notes[0].Mission != "exploring northwind" {
		t.Errorf("store row mismatch: %+v", store.Notes)
	}
}

func TestCallAppend_BadRequest(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	out, err := ext.Call(ctx, "notepad:append", json.RawMessage(`{not-json`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"bad_request"`) {
		t.Errorf("expected bad_request, got %s", out)
	}
}

func TestCallAppend_EmptyContent(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	args, _ := json.Marshal(AppendInput{Content: ""})
	out, err := ext.Call(ctx, "notepad:append", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"io"`) {
		t.Errorf("expected io error from empty content, got %s", out)
	}
}

func TestCallRead_ReturnsNotes(t *testing.T) {
	ext, state, store := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	// Pre-seed two notes.
	for _, content := range []string{"first", "second"} {
		args, _ := json.Marshal(AppendInput{Content: content, Category: "test"})
		if _, err := ext.Call(ctx, "notepad:append", args); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
	if len(store.Notes) != 2 {
		t.Fatalf("expected 2 seeded notes, got %d", len(store.Notes))
	}

	args, _ := json.Marshal(ReadInput{})
	out, err := ext.Call(ctx, "notepad:read", args)
	if err != nil {
		t.Fatalf("Call read: %v", err)
	}
	var got struct {
		Notes []wireNote `json:"notes"`
	}
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if len(got.Notes) != 2 {
		t.Errorf("expected 2 notes from read, got %+v", got.Notes)
	}
}

func TestCallSearch_FallbackOnNoEmbedder(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	// Seed one note.
	seedArgs, _ := json.Marshal(AppendInput{Content: "queryable hypothesis"})
	if _, err := ext.Call(ctx, "notepad:append", seedArgs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	args, _ := json.Marshal(SearchInput{Query: "anything"})
	out, err := ext.Call(ctx, "notepad:search", args)
	if err != nil {
		t.Fatalf("Call search: %v", err)
	}
	var got struct {
		Notes []wireNote `json:"notes"`
	}
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if len(got.Notes) != 1 {
		t.Errorf("expected fallback recency listing to return 1 note, got %+v", got.Notes)
	}
}

func TestCallSearch_RequiresQuery(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	args, _ := json.Marshal(SearchInput{Query: ""})
	out, err := ext.Call(ctx, "notepad:search", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"io"`) {
		t.Errorf("expected io error on empty query, got %s", out)
	}
}

func TestCallShow_FormatsForUser(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	seedArgs, _ := json.Marshal(AppendInput{Content: "hypothesis A", Category: "x"})
	if _, err := ext.Call(ctx, "notepad:append", seedArgs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	args, _ := json.Marshal(ShowInput{})
	out, err := ext.Call(ctx, "notepad:show", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]string
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if !strings.Contains(got["text"], "## x (1)") {
		t.Errorf("expected category bucket header in formatted text, got %q", got["text"])
	}
}

func TestCallAppend_NoSessionInContext(t *testing.T) {
	ext := NewExtension(fixture.NewTestStore(), "a1", Config{})
	args, _ := json.Marshal(AppendInput{Content: "x"})
	out, err := ext.Call(context.Background(), "notepad:append", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"session_gone"`) {
		t.Errorf("expected session_gone, got %s", out)
	}
}

// ---------- Advertise / Block B ----------

func TestAdvertise_RendersSnapshot(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	// Seed two notes across two categories.
	for _, in := range []AppendInput{
		{Content: "orders.deleted_at appears to soft-delete", Category: "schema-finding"},
		{Content: "EMEA region focus, EUR amounts", Category: "user-preference"},
	} {
		args, _ := json.Marshal(in)
		if _, err := ext.Call(ctx, "notepad:append", args); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if out == "" {
		t.Fatal("expected non-empty Advertise output")
	}
	for _, want := range []string{
		"## Notepad snapshot",
		"hypotheses",
		"**schema-finding** (1)",
		"**user-preference** (1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q; got:\n%s", want, out)
		}
	}
}

func TestAdvertise_EmptyWhenNoNotes(t *testing.T) {
	ext, state, _ := newFixture(t)
	if out := ext.AdvertiseSystemPrompt(context.Background(), state); out != "" {
		t.Errorf("expected empty Advertise, got %q", out)
	}
}

func TestAdvertise_NilStateHandle(t *testing.T) {
	// Bare state without InitState — FromState returns nil.
	ext := NewExtension(fixture.NewTestStore(), "a1", Config{})
	state := fixture.NewTestSessionState("ses-bare")
	if out := ext.AdvertiseSystemPrompt(context.Background(), state); out != "" {
		t.Errorf("expected empty Advertise for missing state, got %q", out)
	}
}

func TestCall_UnknownOp(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := extension.WithSessionState(context.Background(), state)

	if _, err := ext.Call(ctx, "notepad:nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for unknown op")
	}
}
