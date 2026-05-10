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

func TestExtension_List_CoreTools(t *testing.T) {
	ext := NewExtension(nil, nil, "a1")
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]bool{
		"skill:load":           false,
		"skill:unload":         false,
		"skill:save":           false,
		"skill:files":          false,
		"skill:ref":            false,
		"skill:tools_catalog":  false,
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

// ---------- skill:save ----------

// newSaveFixture wires the skill extension over a SkillStore that
// has both an Inline alpha (so Load tests still work) AND a
// writable LocalRoot under t.TempDir() (so Publish can persist).
// Returns the LocalRoot path for assertions on the on-disk shape.
func newSaveFixture(t *testing.T) (*Extension, *fixture.TestSessionState, *skillpkg.SkillManager, string) {
	t.Helper()
	localRoot := t.TempDir()
	store := skillpkg.NewSkillStore(skillpkg.Options{
		LocalRoot: localRoot,
		Inline: map[string][]byte{
			"alpha": []byte(inlineAlphaManifest),
		},
	})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-save")
	state := fixture.NewTestSessionState("ses-save")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	return ext, state, mgr, localRoot
}

func decodeSaveResult(t *testing.T, raw json.RawMessage) saveResult {
	t.Helper()
	var r saveResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode saveResult: %v\nraw: %s", err, raw)
	}
	return r
}

func TestCallSave_HappyPath_MinimalBundle(t *testing.T) {
	ext, state, _, localRoot := newSaveFixture(t)
	args := json.RawMessage(`{"skill_md": "---\nname: minimal\ndescription: minimal smoke.\nlicense: MIT\n---\nbody"}`)
	out, err := ext.Call(newCallCtx(state), "skill:save", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	res := decodeSaveResult(t, out)
	if res.Name != "minimal" {
		t.Errorf("Name = %q, want minimal", res.Name)
	}
	if res.Directory == "" || !strings.HasPrefix(res.Directory, localRoot) {
		t.Errorf("Directory = %q, want under %q", res.Directory, localRoot)
	}
	// Only SKILL.md should be in the bundle (no extra categories).
	if len(res.Files) != 1 || res.Files[0] != "SKILL.md" {
		t.Errorf("Files = %v, want [SKILL.md]", res.Files)
	}
	// SKILL.md should exist on disk.
	if _, err := os.Stat(filepath.Join(localRoot, "minimal", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing on disk: %v", err)
	}
	// Auto-loaded in current session.
	if _, err := FromState(state).LoadedSkill(context.Background(), "minimal"); err != nil {
		t.Errorf("auto-load failed: %v", err)
	}
}

func TestCallSave_HappyPath_FullBundle(t *testing.T) {
	ext, state, _, localRoot := newSaveFixture(t)
	args := json.RawMessage(`{
		"skill_md": "---\nname: full\ndescription: full bundle.\nlicense: MIT\n---\nbody",
		"references": {"howto.md": "how to use", "deep/dive.md": "details"},
		"scripts":    {"query.py": "print('q')", "render.py": "print('r')"},
		"assets":     {"template.html": "<html/>"}
	}`)
	out, err := ext.Call(newCallCtx(state), "skill:save", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	res := decodeSaveResult(t, out)
	want := []string{
		"SKILL.md",
		"assets/template.html",
		"references/deep/dive.md",
		"references/howto.md",
		"scripts/query.py",
		"scripts/render.py",
	}
	if len(res.Files) != len(want) {
		t.Fatalf("Files = %v\nwant %v", res.Files, want)
	}
	for i, w := range want {
		if res.Files[i] != w {
			t.Errorf("Files[%d] = %q, want %q", i, res.Files[i], w)
		}
	}
	// Spot-check a script and a nested reference landed on disk.
	body, err := os.ReadFile(filepath.Join(localRoot, "full", "scripts", "query.py"))
	if err != nil || string(body) != "print('q')" {
		t.Errorf("script content = %q (err=%v), want print('q')", body, err)
	}
	body, err = os.ReadFile(filepath.Join(localRoot, "full", "references", "deep", "dive.md"))
	if err != nil || string(body) != "details" {
		t.Errorf("nested ref content = %q (err=%v), want details", body, err)
	}
}

func TestCallSave_RejectsAutoload(t *testing.T) {
	ext, state, _, _ := newSaveFixture(t)
	args := json.RawMessage(`{"skill_md": "---\nname: bad-autoload\ndescription: x.\nlicense: MIT\nmetadata:\n  hugen:\n    autoload: true\n---\n"}`)
	_, err := ext.Call(newCallCtx(state), "skill:save", args)
	if !errors.Is(err, skillpkg.ErrAutoloadReserved) {
		t.Errorf("err = %v, want ErrAutoloadReserved", err)
	}
}

func TestCallSave_CollisionWithoutOverwrite(t *testing.T) {
	ext, state, _, _ := newSaveFixture(t)
	args := json.RawMessage(`{"skill_md": "---\nname: collide\ndescription: first.\nlicense: MIT\n---\n"}`)
	if _, err := ext.Call(newCallCtx(state), "skill:save", args); err != nil {
		t.Fatalf("first Call: %v", err)
	}
	// Second save under same name must fail.
	args2 := json.RawMessage(`{"skill_md": "---\nname: collide\ndescription: second.\nlicense: MIT\n---\n"}`)
	_, err := ext.Call(newCallCtx(state), "skill:save", args2)
	if !errors.Is(err, skillpkg.ErrSkillExists) {
		t.Errorf("err = %v, want ErrSkillExists", err)
	}
}

func TestCallSave_OverwriteReplacesContents(t *testing.T) {
	ext, state, _, localRoot := newSaveFixture(t)
	v1 := json.RawMessage(`{
		"skill_md":   "---\nname: ow\ndescription: v1.\nlicense: MIT\n---\n",
		"references": {"v1.md": "first version"}
	}`)
	if _, err := ext.Call(newCallCtx(state), "skill:save", v1); err != nil {
		t.Fatalf("first Call: %v", err)
	}
	v2 := json.RawMessage(`{
		"skill_md":   "---\nname: ow\ndescription: v2.\nlicense: MIT\n---\n",
		"references": {"v2.md": "second version"},
		"overwrite":  true
	}`)
	if _, err := ext.Call(newCallCtx(state), "skill:save", v2); err != nil {
		t.Fatalf("overwrite Call: %v", err)
	}
	// v1 file removed by overwrite (full directory replace).
	if _, err := os.Stat(filepath.Join(localRoot, "ow", "references", "v1.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("v1.md lingered after overwrite: %v", err)
	}
	// v2 file present.
	if _, err := os.Stat(filepath.Join(localRoot, "ow", "references", "v2.md")); err != nil {
		t.Errorf("v2.md missing after overwrite: %v", err)
	}
}

func TestCallSave_PathTraversalRejected(t *testing.T) {
	ext, state, _, _ := newSaveFixture(t)
	cases := []struct {
		name string
		args string
	}{
		{
			name: "references_parent_escape",
			args: `{"skill_md":"---\nname: p1\ndescription: x.\nlicense: MIT\n---\n","references":{"../escape.md":"x"}}`,
		},
		{
			name: "scripts_absolute",
			args: `{"skill_md":"---\nname: p2\ndescription: x.\nlicense: MIT\n---\n","scripts":{"/etc/passwd":"x"}}`,
		},
		{
			name: "assets_hidden",
			args: `{"skill_md":"---\nname: p3\ndescription: x.\nlicense: MIT\n---\n","assets":{".env":"x"}}`,
		},
		{
			name: "references_backslash",
			args: `{"skill_md":"---\nname: p4\ndescription: x.\nlicense: MIT\n---\n","references":{"foo\\bar.md":"x"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ext.Call(newCallCtx(state), "skill:save", json.RawMessage(tc.args))
			if !errors.Is(err, skillpkg.ErrInvalidPath) {
				t.Errorf("err = %v, want ErrInvalidPath", err)
			}
		})
	}
}

func TestCallSave_EmptySkillMD(t *testing.T) {
	ext, state, _, _ := newSaveFixture(t)
	_, err := ext.Call(newCallCtx(state), "skill:save", json.RawMessage(`{"skill_md": "   "}`))
	if !errors.Is(err, tool.ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}

func TestCallSave_InvalidManifest(t *testing.T) {
	ext, state, _, _ := newSaveFixture(t)
	// Missing description → ErrManifestInvalid.
	args := json.RawMessage(`{"skill_md": "---\nname: incomplete\nlicense: MIT\n---\n"}`)
	_, err := ext.Call(newCallCtx(state), "skill:save", args)
	if !errors.Is(err, skillpkg.ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
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
