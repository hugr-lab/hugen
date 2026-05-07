package skill

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakePerms is a perm.Service stub for the skill:files gate. rules
// are keyed by "object:field"; default outcome is allow.
type fakePerms struct {
	rules map[string]perm.Permission
}

func (f *fakePerms) Resolve(_ context.Context, object, field string) (perm.Permission, error) {
	if p, ok := f.rules[object+":"+field]; ok {
		return p, nil
	}
	return perm.Permission{}, nil
}
func (f *fakePerms) Refresh(context.Context) error { return nil }
func (f *fakePerms) Subscribe(context.Context) (<-chan perm.RefreshEvent, error) {
	return nil, nil
}

// inlineAlphaManifest is a minimal SKILL.md a SkillStore.Inline
// load reads as the body of an inline skill named "alpha".
const inlineAlphaManifest = `---
name: alpha
description: alpha skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
body
`

// newCallCtx builds a dispatch ctx with the session state attached
// the same way Session.Dispatch does live.
func newCallCtx(state extension.SessionState) context.Context {
	return extension.WithSessionState(context.Background(), state)
}

// newAlphaFixture wires the skill extension over an inline-only
// SkillStore + a TestSessionState. Tests that need a loaded skill
// pre-load alpha after the fixture builds.
func newAlphaFixture(t *testing.T, perms perm.Service) (*Extension, *fixture.TestSessionState, *skillpkg.SkillManager) {
	t.Helper()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha": []byte(inlineAlphaManifest),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, perms, "agent-test")
	state := fixture.NewTestSessionState("ses-test")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	return ext, state, mgr
}

// ---------- ToolProvider surface ----------

func TestExtension_Name(t *testing.T) {
	ext := NewExtension(nil, nil, "a1")
	if got := ext.Name(); got != "skill" {
		t.Errorf("Name = %q, want skill", got)
	}
}

func TestExtension_List_AllFiveTools(t *testing.T) {
	ext := NewExtension(nil, nil, "a1")
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]bool{
		"skill:load":    false,
		"skill:unload":  false,
		"skill:publish": false,
		"skill:files":   false,
		"skill:ref":     false,
	}
	for _, tt := range tools {
		if _, ok := want[tt.Name]; ok {
			want[tt.Name] = true
		}
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("%s not in catalogue", n)
		}
	}
}

// ---------- skill:load ----------

func TestCallLoad_Happy(t *testing.T) {
	ext, state, _ := newAlphaFixture(t, nil)
	out, err := ext.Call(newCallCtx(state), "skill:load", json.RawMessage(`{"name":"alpha"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"loaded":true`) {
		t.Errorf("out = %s", out)
	}
	if _, err := FromState(state).LoadedSkill(context.Background(), "alpha"); err != nil {
		t.Errorf("LoadedSkill: %v", err)
	}
}

func TestCallLoad_NameRequired(t *testing.T) {
	ext, state, _ := newAlphaFixture(t, nil)
	_, err := ext.Call(newCallCtx(state), "skill:load", json.RawMessage(`{}`))
	if !errors.Is(err, tool.ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}

func TestCallLoad_NoSessionInContext(t *testing.T) {
	ext, _, _ := newAlphaFixture(t, nil)
	_, err := ext.Call(context.Background(), "skill:load", json.RawMessage(`{"name":"alpha"}`))
	if !errors.Is(err, tool.ErrSystemUnavailable) {
		t.Errorf("err = %v, want ErrSystemUnavailable", err)
	}
}

// ---------- skill:unload ----------

func TestCallUnload_Idempotent(t *testing.T) {
	ext, state, _ := newAlphaFixture(t, nil)
	if _, err := ext.Call(newCallCtx(state), "skill:unload", json.RawMessage(`{"name":"missing"}`)); err != nil {
		t.Errorf("Call: %v", err)
	}
}

// ---------- skill:publish ----------

func TestCallPublish_DeferredStub(t *testing.T) {
	ext, state, _ := newAlphaFixture(t, nil)
	_, err := ext.Call(newCallCtx(state), "skill:publish", json.RawMessage(`{"name":"x","body":"y"}`))
	if !errors.Is(err, tool.ErrSystemUnavailable) {
		t.Errorf("err = %v, want ErrSystemUnavailable", err)
	}
}

// ---------- skill:ref ----------

func TestCallRef_InlineSkillHasNoFS(t *testing.T) {
	ext, state, _ := newAlphaFixture(t, nil)
	if err := FromState(state).Load(context.Background(), "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err := ext.Call(newCallCtx(state), "skill:ref", json.RawMessage(`{"skill":"alpha","ref":"x.md"}`))
	if err == nil {
		t.Fatalf("expected error (inline skill has no body fs)")
	}
	if !strings.Contains(err.Error(), "no body fs") {
		t.Errorf("err = %v", err)
	}
}

// ---------- skill:files ----------

// gammaRoot writes a minimal on-disk skill named `gamma` with
// SKILL.md and two reference files under root/gamma. Returns the
// absolute path of the gamma skill root for assertions.
func gammaRoot(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "gamma")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `---
name: gamma
description: Test skill for skill:files.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
# Gamma
`
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references/attach.md"), []byte("attach body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references/query.md"), []byte("query body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(dir)
	return abs
}

func newGammaFixture(t *testing.T, perms perm.Service) (*Extension, *fixture.TestSessionState, *skillpkg.SkillManager) {
	t.Helper()
	root := t.TempDir()
	gammaRoot(t, root)
	store := skillpkg.NewSkillStore(skillpkg.Options{LocalRoot: root})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, perms, "agent-test")
	state := fixture.NewTestSessionState("ses-test")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if err := FromState(state).Load(context.Background(), "gamma"); err != nil {
		t.Fatalf("Load gamma: %v", err)
	}
	return ext, state, mgr
}

func TestCallFiles_Happy(t *testing.T) {
	ext, state, _ := newGammaFixture(t, nil)
	raw, err := ext.Call(newCallCtx(state), "skill:files", json.RawMessage(`{"name":"gamma"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got filesResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Skill != "gamma" {
		t.Errorf("Skill = %q", got.Skill)
	}
	if got.Truncated {
		t.Errorf("Truncated = true on tiny fixture")
	}
	wantRels := []string{"SKILL.md", "references/attach.md", "references/query.md"}
	if len(got.Files) != len(wantRels) {
		t.Fatalf("len(files) = %d, want %d (%+v)", len(got.Files), len(wantRels), got.Files)
	}
	for i, f := range got.Files {
		if f.Rel != wantRels[i] {
			t.Errorf("[%d] rel = %q, want %q", i, f.Rel, wantRels[i])
		}
	}
}

func TestCallFiles_PathEscape(t *testing.T) {
	ext, state, _ := newGammaFixture(t, nil)
	_, err := ext.Call(newCallCtx(state), "skill:files",
		json.RawMessage(`{"name":"gamma","subdir":"../etc"}`))
	if !errors.Is(err, tool.ErrPathEscape) {
		t.Fatalf("err = %v, want ErrPathEscape", err)
	}
}

func TestCallFiles_PermissionDenied(t *testing.T) {
	denied := &fakePerms{rules: map[string]perm.Permission{
		"hugen:command:skill_files:gamma": {Disabled: true, FromConfig: true},
	}}
	ext, state, _ := newGammaFixture(t, denied)
	_, err := ext.Call(newCallCtx(state), "skill:files", json.RawMessage(`{"name":"gamma"}`))
	if !errors.Is(err, tool.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestCallFiles_NotLoaded(t *testing.T) {
	root := t.TempDir()
	store := skillpkg.NewSkillStore(skillpkg.Options{LocalRoot: root})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "a1")
	state := fixture.NewTestSessionState("ses-test")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	_, err := ext.Call(newCallCtx(state), "skill:files", json.RawMessage(`{"name":"gamma"}`))
	if !errors.Is(err, tool.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCallFiles_BadGlob(t *testing.T) {
	ext, state, _ := newGammaFixture(t, nil)
	_, err := ext.Call(newCallCtx(state), "skill:files",
		json.RawMessage(`{"name":"gamma","glob":"["}`))
	if !errors.Is(err, tool.ErrArgValidation) {
		t.Fatalf("err = %v, want ErrArgValidation", err)
	}
}

// ---------- unknown op ----------

func TestCall_UnknownOp(t *testing.T) {
	ext, state, _ := newAlphaFixture(t, nil)
	if _, err := ext.Call(newCallCtx(state), "skill:nope", json.RawMessage(`{}`)); !errors.Is(err, tool.ErrUnknownTool) {
		t.Errorf("err = %v, want ErrUnknownTool", err)
	}
}
