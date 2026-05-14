package main

import (
	"context"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/session"
)

// staticIdentity is a tiny identity.Source for integration tests.
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

// stubStore is a no-op RuntimeStore — integration tests never write
// to the store, but Session construction needs one.
type stubStore struct{}

func (s *stubStore) OpenSession(_ context.Context, _ session.SessionRow) error { return nil }
func (s *stubStore) LoadSession(_ context.Context, id string) (session.SessionRow, error) {
	return session.SessionRow{ID: id, AgentID: "a1", Status: session.StatusActive}, nil
}
func (s *stubStore) UpdateSessionStatus(_ context.Context, _, _ string) error { return nil }
func (s *stubStore) AppendEvent(_ context.Context, _ session.EventRow, _ string) error {
	return nil
}
func (s *stubStore) ListEvents(_ context.Context, _ string, _ session.ListEventsOpts) ([]session.EventRow, error) {
	return nil, nil
}
func (s *stubStore) LatestEventOfKinds(_ context.Context, _ string, _ []string) (session.EventRow, bool, error) {
	return session.EventRow{}, false, nil
}
func (s *stubStore) NextSeq(_ context.Context, _ string) (int, error) {
	return 1, nil
}
func (s *stubStore) AppendNote(_ context.Context, _ session.NoteRow) error { return nil }
func (s *stubStore) ListNotes(_ context.Context, _ string, _ session.ListNotesOpts) ([]session.NoteRow, error) {
	return nil, nil
}
func (s *stubStore) SearchNotes(_ context.Context, _, _ string, _ session.ListNotesOpts) ([]session.NoteRow, error) {
	return nil, nil
}
func (s *stubStore) CountNotesByCategory(_ context.Context, _ string, _ session.ListNotesOpts) (map[string]int, error) {
	return nil, nil
}
func (s *stubStore) ListSessions(_ context.Context, _, _ string) ([]session.SessionRow, error) {
	return nil, nil
}
func (s *stubStore) ListResumableRoots(_ context.Context, _ string) ([]session.ResumableRoot, error) {
	return nil, nil
}
func (s *stubStore) ListChildren(_ context.Context, _ string) ([]session.SessionRow, error) {
	return nil, nil
}

// stubModel is the minimal Model required to satisfy ModelRouter.
type stubModel struct{}

func (stubModel) Spec() model.ModelSpec { return model.ModelSpec{Provider: "fake", Name: "f"} }
func (stubModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	return nil, nil
}

