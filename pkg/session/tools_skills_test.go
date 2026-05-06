package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeSkillsPerms is a perm.Service stub for the skill_files gate.
// rules are keyed by "object:field"; default outcome is allow.
type fakeSkillsPerms struct {
	rules map[string]perm.Permission
}

func (f *fakeSkillsPerms) Resolve(_ context.Context, object, field string) (perm.Permission, error) {
	if p, ok := f.rules[object+":"+field]; ok {
		return p, nil
	}
	return perm.Permission{}, nil
}
func (f *fakeSkillsPerms) Refresh(context.Context) error { return nil }
func (f *fakeSkillsPerms) Subscribe(context.Context) (<-chan perm.RefreshEvent, error) {
	return nil, nil
}

// ---------- skill_load ----------

func TestSkillLoad_RoutesThroughCallerSession(t *testing.T) {
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
body
`),
	}})
	skills := skill.NewSkillManager(store, nil)
	parent, host, cleanup := newTestParent(t, withTestSkills(skills))
	defer cleanup()

	out, err := callSkillLoad(us1WithSession(parent), parent, host, json.RawMessage(`{"name":"alpha"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"loaded":true`) {
		t.Errorf("out = %s", out)
	}
	if _, err := skills.LoadedSkill(context.Background(), parent.ID(), "alpha"); err != nil {
		t.Errorf("LoadedSkill: %v", err)
	}
}

func TestSkillLoad_NameRequired(t *testing.T) {
	skills := skill.NewSkillManager(skill.NewSkillStore(skill.Options{}), nil)
	parent, host, cleanup := newTestParent(t, withTestSkills(skills))
	defer cleanup()

	_, err := callSkillLoad(us1WithSession(parent), parent, host, json.RawMessage(`{}`))
	if !errors.Is(err, tool.ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}

// ---------- skill_unload ----------

func TestSkillUnload_Idempotent(t *testing.T) {
	skills := skill.NewSkillManager(skill.NewSkillStore(skill.Options{}), nil)
	parent, host, cleanup := newTestParent(t, withTestSkills(skills))
	defer cleanup()

	if _, err := callSkillUnload(us1WithSession(parent), parent, host, json.RawMessage(`{"name":"missing"}`)); err != nil {
		t.Errorf("Call: %v", err)
	}
}

// ---------- skill_publish ----------

func TestSkillPublish_DeferredStub(t *testing.T) {
	_, err := callSkillPublish(context.Background(), nil, SessionToolHost{}, json.RawMessage(`{"name":"x","body":"y"}`))
	if !errors.Is(err, tool.ErrSystemUnavailable) {
		t.Errorf("err = %v, want ErrSystemUnavailable", err)
	}
}

// ---------- skill_ref ----------

func TestSkillRef_InlineSkillHasNoFS(t *testing.T) {
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
body
`),
	}})
	skills := skill.NewSkillManager(store, nil)
	parent, host, cleanup := newTestParent(t, withTestSkills(skills))
	defer cleanup()

	if err := skills.Load(context.Background(), parent.ID(), "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err := callSkillRef(us1WithSession(parent), parent, host, json.RawMessage(`{"skill":"alpha","ref":"x.md"}`))
	if err == nil {
		t.Fatalf("expected error (inline skill has no body fs)")
	}
	if !strings.Contains(err.Error(), "no body fs") {
		t.Errorf("err = %v", err)
	}
}

// ---------- skill_files ----------

// skillFixtureRoot writes a minimal on-disk skill named `gamma` with
// SKILL.md and two reference files under root/gamma. Intended to be
// called BEFORE the SkillStore reads root.
func skillFixtureRoot(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "gamma")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `---
name: gamma
description: Test skill for skill_files.
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

// newGammaFixture wires SkillManager around an on-disk `gamma`
// skill loaded into a fresh test session. Returns the parent
// session, the SessionToolHost the skill_files handler call should
// receive (with the supplied perms threaded through Host.Perms),
// the skill root, and the cleanup func.
func newGammaFixture(t *testing.T, perms perm.Service) (*Session, SessionToolHost, string, func()) {
	t.Helper()
	root := t.TempDir()
	skillFixtureRoot(t, root)
	store := skill.NewSkillStore(skill.Options{LocalRoot: root})
	skills := skill.NewSkillManager(store, nil)
	parent, host, cleanup := newTestParent(t, withTestSkills(skills), withTestPerms(perms))
	if err := skills.Load(context.Background(), parent.ID(), "gamma"); err != nil {
		cleanup()
		t.Fatalf("Load: %v", err)
	}
	return parent, host, root, cleanup
}

func TestSkillFiles_HappyPath(t *testing.T) {
	parent, host, _, cleanup := newGammaFixture(t, nil)
	defer cleanup()

	raw, err := callSkillFiles(us1WithSession(parent), parent, host, json.RawMessage(`{"name":"gamma"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got skillFilesResult
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

func TestSkillFiles_PathEscape(t *testing.T) {
	parent, host, _, cleanup := newGammaFixture(t, nil)
	defer cleanup()

	_, err := callSkillFiles(us1WithSession(parent), parent, host,
		json.RawMessage(`{"name":"gamma","subdir":"../etc"}`))
	if !errors.Is(err, tool.ErrPathEscape) {
		t.Fatalf("err = %v, want ErrPathEscape", err)
	}
}

func TestSkillFiles_PermissionDenied(t *testing.T) {
	denied := &fakeSkillsPerms{rules: map[string]perm.Permission{
		"hugen:command:skill_files:gamma": {Disabled: true, FromConfig: true},
	}}
	parent, host, _, cleanup := newGammaFixture(t, denied)
	defer cleanup()

	_, err := callSkillFiles(us1WithSession(parent), parent, host, json.RawMessage(`{"name":"gamma"}`))
	if !errors.Is(err, tool.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestSkillFiles_NotLoaded(t *testing.T) {
	store := skill.NewSkillStore(skill.Options{LocalRoot: t.TempDir()})
	skills := skill.NewSkillManager(store, nil)
	parent, host, cleanup := newTestParent(t, withTestSkills(skills))
	defer cleanup()

	// gamma is never loaded for this session.
	_, err := callSkillFiles(us1WithSession(parent), parent, host, json.RawMessage(`{"name":"gamma"}`))
	if !errors.Is(err, tool.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSkillFiles_BadGlob(t *testing.T) {
	parent, host, _, cleanup := newGammaFixture(t, nil)
	defer cleanup()

	_, err := callSkillFiles(us1WithSession(parent), parent, host,
		json.RawMessage(`{"name":"gamma","glob":"["}`))
	if !errors.Is(err, tool.ErrArgValidation) {
		t.Fatalf("err = %v, want ErrArgValidation", err)
	}
}

// ---------- registration ----------

func TestSkillTools_RegisteredOnSessionProvider(t *testing.T) {
	prov := NewSessionToolProvider(nil, SessionToolHost{})
	tools, err := prov.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := map[string]bool{
		"session:skill_load":    false,
		"session:skill_unload":  false,
		"session:skill_publish": false,
		"session:skill_files":   false,
		"session:skill_ref":     false,
	}
	for _, tt := range tools {
		if _, ok := want[tt.Name]; ok {
			want[tt.Name] = true
		}
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("%s not registered on SessionToolProvider", n)
		}
	}
}
