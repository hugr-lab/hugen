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
metadata:
  hugen:
    tier_compatibility: [root, mission, worker]
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
	state := fixture.NewTestSessionState("ses-test").WithDepth(2)
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
		"skill:load":          false,
		"skill:unload":        false,
		"skill:save":          false,
		"skill:files":         false,
		"skill:ref":           false,
		"skill:tools_catalog": false,
	}
	// Every tool's ArgSchema must conform to the cross-provider
	// chat-completion subset (see pkg/tool/validate.go). Gemini in
	// particular rejects additionalProperties / $ref / oneOf /
	// anyOf / allOf — surfaced as a 400 from the API and a hard
	// stream_error in the harness. Validate at unit-test time so
	// the regression breaks the build instead of leaking to a
	// scenario run.
	for _, tl := range tools {
		if err := tool.ValidateLLMSchema(tl.ArgSchema); err != nil {
			t.Errorf("%s schema invalid: %v", tl.Name, err)
		}
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

// inlineWorkerSkillManifest declares tier_compatibility: [worker]
// — used by tier_forbidden tests to verify a root session is
// refused with the structured envelope.
const inlineWorkerSkillManifest = `---
name: worker-only
description: Worker-tier-only skill for tier_forbidden tests.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
metadata:
  hugen:
    tier_compatibility: [worker]
---
body
`

// newTierFixture wires the extension over a manager that knows
// both alpha (loadable everywhere — default [worker], but the
// test only uses it via direct Manifest.LoadableInTier) and
// worker-only (tier_compatibility: [worker]). Returns the
// session at the supplied depth so tests can pick the caller
// tier deterministically.
func newTierFixture(t *testing.T, depth int) (*Extension, *fixture.TestSessionState) {
	t.Helper()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"worker-only": []byte(inlineWorkerSkillManifest),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-tier")
	state := fixture.NewTestSessionState("ses-tier").WithDepth(depth)
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	return ext, state
}

// TestCallLoad_TierForbidden_Root verifies a root-tier session
// trying to load a worker-only skill gets the structured envelope
// tool_error{code:"tier_forbidden"} with the alternative-path hint
// pointing at spawn_subagent. Phase 4.2.2 §3.3.3.
func TestCallLoad_TierForbidden_Root(t *testing.T) {
	ext, state := newTierFixture(t, 0) // depth 0 → root tier
	out, err := ext.Call(newCallCtx(state), "skill:load", json.RawMessage(`{"name":"worker-only"}`))
	if err != nil {
		t.Fatalf("Call returned error %v; want JSON envelope", err)
	}
	if !strings.Contains(string(out), `"tier_forbidden"`) {
		t.Errorf("envelope missing tier_forbidden code: %s", out)
	}
	if !strings.Contains(string(out), "spawn_subagent") {
		t.Errorf("envelope missing root-tier hint (spawn_subagent): %s", out)
	}
}

// TestCallLoad_TierAllowed_Worker verifies a worker-tier session
// can load the same skill — proves the gate is selective, not
// blanket-reject.
func TestCallLoad_TierAllowed_Worker(t *testing.T) {
	ext, state := newTierFixture(t, 2) // depth 2 → worker tier
	out, err := ext.Call(newCallCtx(state), "skill:load", json.RawMessage(`{"name":"worker-only"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"loaded":true`) {
		t.Errorf("out = %s, want loaded:true", out)
	}
}

// TestCallLoad_TierForbidden_Mission verifies the mission-tier
// hint points at spawning a worker (rather than at spawn_mission,
// which is root's path).
func TestCallLoad_TierForbidden_Mission(t *testing.T) {
	ext, state := newTierFixture(t, 1) // depth 1 → mission tier
	out, err := ext.Call(newCallCtx(state), "skill:load", json.RawMessage(`{"name":"worker-only"}`))
	if err != nil {
		t.Fatalf("Call returned error %v; want JSON envelope", err)
	}
	if !strings.Contains(string(out), `"tier_forbidden"`) {
		t.Errorf("envelope missing tier_forbidden code: %s", out)
	}
	if !strings.Contains(string(out), "spawn a worker") {
		t.Errorf("envelope missing mission-tier hint (spawn a worker): %s", out)
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
	state := fixture.NewTestSessionState("ses-save").WithDepth(2)
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
	// Auto-load after overwrite must surface the v2 manifest, not
	// the cached v1 — the SkillManager generation token is the
	// invalidation seam for downstream snapshot caches.
	loaded, err := FromState(state).LoadedSkill(context.Background(), "ow")
	if err != nil {
		t.Fatalf("LoadedSkill after overwrite: %v", err)
	}
	if loaded.Manifest.Description != "v2." {
		t.Errorf("after overwrite: loaded skill description = %q, want \"v2.\" (cache may not be invalidating)", loaded.Manifest.Description)
	}
}

// TestCallSave_AutoloadFailureSurfacesPartialSuccess proves that
// when the post-Publish auto-load fails (most common cause:
// requires_skills resolves to an unknown name), the tool returns
// an actionable error referencing the skill name + the manual
// recovery path. The skill stays on disk.
func TestCallSave_AutoloadFailureSurfacesPartialSuccess(t *testing.T) {
	ext, state, _, localRoot := newSaveFixture(t)
	args := json.RawMessage(`{"skill_md": "---\nname: orphan\ndescription: missing dep.\nlicense: MIT\nmetadata:\n  hugen:\n    requires_skills: [definitely-not-a-real-skill]\n---\n"}`)
	_, err := ext.Call(newCallCtx(state), "skill:save", args)
	if err == nil {
		t.Fatal("expected error from auto-load with missing dep, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "orphan") {
		t.Errorf("error should reference skill name: %q", msg)
	}
	if !strings.Contains(msg, "/skill load") {
		t.Errorf("error should suggest manual recovery via /skill load: %q", msg)
	}
	// Skill file persisted on disk despite the auto-load failure.
	if _, statErr := os.Stat(filepath.Join(localRoot, "orphan", "SKILL.md")); statErr != nil {
		t.Errorf("orphan SKILL.md missing on disk: %v", statErr)
	}
}

// TestCallSave_ErrorSentinelsPropagate locks down the
// errors.Is chain through fmt.Errorf wrapping in callSave —
// session.go's dispatch error handler maps these sentinels to
// typed ToolError codes (ToolErrorSkillExists / SkillBadManifest /
// SkillBadPath / SkillAutoload). If a future refactor of the
// wrap chain breaks errors.Is, the typed-code mapping silently
// degrades to "io" — this test prevents that regression.
func TestCallSave_ErrorSentinelsPropagate(t *testing.T) {
	ext, state, _, _ := newSaveFixture(t)

	// autoload:true → ErrAutoloadReserved
	_, err := ext.Call(newCallCtx(state), "skill:save",
		json.RawMessage(`{"skill_md": "---\nname: a\ndescription: x.\nlicense: MIT\nmetadata:\n  hugen:\n    autoload: true\n---\n"}`))
	if !errors.Is(err, skillpkg.ErrAutoloadReserved) {
		t.Errorf("autoload err = %v, want errors.Is ErrAutoloadReserved", err)
	}

	// invalid path → ErrInvalidPath
	_, err = ext.Call(newCallCtx(state), "skill:save",
		json.RawMessage(`{"skill_md": "---\nname: b\ndescription: x.\nlicense: MIT\n---\n","references":{"../escape.md":"x"}}`))
	if !errors.Is(err, skillpkg.ErrInvalidPath) {
		t.Errorf("path err = %v, want errors.Is ErrInvalidPath", err)
	}

	// invalid manifest (no description) → ErrManifestInvalid
	_, err = ext.Call(newCallCtx(state), "skill:save",
		json.RawMessage(`{"skill_md": "---\nname: c\nlicense: MIT\n---\n"}`))
	if !errors.Is(err, skillpkg.ErrManifestInvalid) {
		t.Errorf("manifest err = %v, want errors.Is ErrManifestInvalid", err)
	}

	// collision: first save OK, second without overwrite →
	// ErrSkillExists. Sentinel must survive the action-oriented
	// wrapping in callSave.
	_, err = ext.Call(newCallCtx(state), "skill:save",
		json.RawMessage(`{"skill_md": "---\nname: dup\ndescription: x.\nlicense: MIT\n---\n"}`))
	if err != nil {
		t.Fatalf("first dup save: %v", err)
	}
	_, err = ext.Call(newCallCtx(state), "skill:save",
		json.RawMessage(`{"skill_md": "---\nname: dup\ndescription: y.\nlicense: MIT\n---\n"}`))
	if !errors.Is(err, skillpkg.ErrSkillExists) {
		t.Errorf("collision err = %v, want errors.Is ErrSkillExists", err)
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
	state := fixture.NewTestSessionState("ses-test").WithDepth(2)
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
	state := fixture.NewTestSessionState("ses-test").WithDepth(2)
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
