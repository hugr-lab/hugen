package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// staticIdentity is a tiny identity.Source for command tests.
type staticIdentity struct{ id string }

func (s staticIdentity) Agent(_ context.Context) (identity.Agent, error) {
	return identity.Agent{ID: s.id, Name: s.id}, nil
}
func (s staticIdentity) WhoAmI(_ context.Context) (identity.WhoAmI, error) {
	return identity.WhoAmI{UserID: s.id, UserName: s.id, Role: "test"}, nil
}
func (s staticIdentity) Permission(_ context.Context, _, _ string) (identity.Permission, error) {
	return identity.Permission{Enabled: true}, nil
}

// permsView is a minimal PermissionsView returning a fixed rule list.
type permsView struct{ rules []config.PermissionRule }

func (v *permsView) Rules() []config.PermissionRule { return v.rules }
func (v *permsView) RefreshInterval() time.Duration { return 0 }
func (v *permsView) RemoteEnabled() bool            { return false }
func (v *permsView) OnUpdate(func()) func()         { return func() {} }

// stubStore is a no-op RuntimeStore — the /skill handlers never
// write to the store, but Session construction needs one.
type stubStore struct{ mu sync.Mutex }

func (s *stubStore) OpenSession(_ context.Context, _ runtime.SessionRow) error { return nil }
func (s *stubStore) LoadSession(_ context.Context, id string) (runtime.SessionRow, error) {
	return runtime.SessionRow{ID: id, AgentID: "a1", Status: runtime.StatusActive}, nil
}
func (s *stubStore) UpdateSessionStatus(_ context.Context, _, _ string) error { return nil }
func (s *stubStore) AppendEvent(_ context.Context, _ runtime.EventRow, _ string) error {
	return nil
}
func (s *stubStore) ListEvents(_ context.Context, _ string, _ runtime.ListEventsOpts) ([]runtime.EventRow, error) {
	return nil, nil
}
func (s *stubStore) NextSeq(_ context.Context, _ string) (int, error) {
	return 1, nil
}
func (s *stubStore) AppendNote(_ context.Context, _ runtime.NoteRow) error { return nil }
func (s *stubStore) ListNotes(_ context.Context, _ string, _ int) ([]runtime.NoteRow, error) {
	return nil, nil
}
func (s *stubStore) ListSessions(_ context.Context, _, _ string) ([]runtime.SessionRow, error) {
	return nil, nil
}

// stubModel is the minimal Model required to satisfy the
// ModelRouter constructor. /skill handlers never call Generate.
type stubModel struct{}

func (stubModel) Spec() model.ModelSpec { return model.ModelSpec{Provider: "fake", Name: "f"} }
func (stubModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	return nil, nil
}

func newSkillCmdEnv(t *testing.T) runtime.CommandEnv {
	t.Helper()
	store := &stubStore{}
	spec := model.ModelSpec{Provider: "fake", Name: "f"}
	router, err := model.NewModelRouter(map[model.Intent]model.ModelSpec{
		model.IntentDefault: spec,
	}, map[model.ModelSpec]model.Model{spec: stubModel{}})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	agent, err := runtime.NewAgent("a1", "hugen", staticIdentity{id: "a1"})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	s := runtime.NewSession("s1", agent, store, router, runtime.NewCommandRegistry(), protocol.NewCodec(), nil)
	return runtime.CommandEnv{
		Session:     s,
		Author:      protocol.ParticipantInfo{ID: "u", Kind: protocol.ParticipantUser},
		AgentAuthor: agent.Participant(),
	}
}

func inlineSkillStack(t *testing.T, name string) (skill.SkillStore, *skill.SkillManager) {
	t.Helper()
	body := `---
name: ` + name + `
description: ` + name + ` skill.
license: MIT
allowed-tools:
  - provider: bash-mcp
    tools: [bash.read_file]
---
body`
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{name: []byte(body)}})
	mgr := skill.NewSkillManager(store, nil)
	return store, mgr
}

func TestSkillCommand_Usage(t *testing.T) {
	env := newSkillCmdEnv(t)
	store, mgr := inlineSkillStack(t, "alpha")
	perms := perm.NewLocalPermissions(&permsView{}, staticIdentity{id: "a1"})
	defer perms.Close()
	frames, err := skillCommandHandler(mgr, store, perms)(context.Background(), env, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind() != protocol.KindError {
		t.Errorf("frames = %+v", frames)
	}
}

func TestSkillCommand_List(t *testing.T) {
	env := newSkillCmdEnv(t)
	store, mgr := inlineSkillStack(t, "alpha")
	perms := perm.NewLocalPermissions(&permsView{}, staticIdentity{id: "a1"})
	defer perms.Close()
	frames, err := skillCommandHandler(mgr, store, perms)(context.Background(), env, []string{"list"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind() != protocol.KindAgentMessage {
		t.Fatalf("frames = %+v", frames)
	}
	body := frames[0].(*protocol.AgentMessage).Payload.Text
	if !strings.Contains(body, "alpha") {
		t.Errorf("list body missing skill: %s", body)
	}
}

func TestSkillCommand_Load_Success(t *testing.T) {
	env := newSkillCmdEnv(t)
	store, mgr := inlineSkillStack(t, "alpha")
	perms := perm.NewLocalPermissions(&permsView{}, staticIdentity{id: "a1"})
	defer perms.Close()
	frames, err := skillCommandHandler(mgr, store, perms)(context.Background(), env, []string{"load", "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d", len(frames))
	}
	mk, ok := frames[0].(*protocol.SystemMarker)
	if !ok || mk.Payload.Subject != protocol.SubjectSkillLoaded {
		t.Errorf("frames[0] = %+v", frames[0])
	}
	if _, err := mgr.LoadedSkill(context.Background(), env.Session.ID(), "alpha"); err != nil {
		t.Errorf("LoadedSkill: %v", err)
	}
}

func TestSkillCommand_Load_PermissionDenied(t *testing.T) {
	env := newSkillCmdEnv(t)
	store, mgr := inlineSkillStack(t, "alpha")
	view := &permsView{rules: []config.PermissionRule{
		{Type: "hugen:skill", Field: "load:alpha", Disabled: true},
	}}
	perms := perm.NewLocalPermissions(view, staticIdentity{id: "a1"})
	defer perms.Close()
	frames, err := skillCommandHandler(mgr, store, perms)(context.Background(), env, []string{"load", "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (tool_result + tool_denied marker)", len(frames))
	}
	tr, ok := frames[0].(*protocol.ToolResult)
	if !ok {
		t.Fatalf("frame[0] = %s, want tool_result", frames[0].Kind())
	}
	if !tr.Payload.IsError {
		t.Errorf("tool_result.IsError = false, want true")
	}
	mk, ok := frames[1].(*protocol.SystemMarker)
	if !ok || mk.Payload.Subject != protocol.SubjectToolDenied {
		t.Errorf("frame[1] = %+v", frames[1])
	}
	if _, err := mgr.LoadedSkill(context.Background(), env.Session.ID(), "alpha"); err == nil {
		t.Errorf("skill loaded despite deny")
	}
}

func TestSkillCommand_Unload(t *testing.T) {
	env := newSkillCmdEnv(t)
	store, mgr := inlineSkillStack(t, "alpha")
	perms := perm.NewLocalPermissions(&permsView{}, staticIdentity{id: "a1"})
	defer perms.Close()
	h := skillCommandHandler(mgr, store, perms)
	if _, err := h(context.Background(), env, []string{"load", "alpha"}); err != nil {
		t.Fatalf("load: %v", err)
	}
	frames, err := h(context.Background(), env, []string{"unload", "alpha"})
	if err != nil {
		t.Fatalf("unload: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d", len(frames))
	}
	mk, ok := frames[0].(*protocol.SystemMarker)
	if !ok || mk.Payload.Subject != protocol.SubjectSkillUnloaded {
		t.Errorf("frames = %+v", frames)
	}
}
