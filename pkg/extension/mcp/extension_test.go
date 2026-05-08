package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// wireWorkspace runs the workspace extension's InitState against
// state so mcp ext tests get a real *SessionWorkspace handle in
// state. Avoids reaching for private SessionWorkspace fields.
func wireWorkspace(t *testing.T, state *fixture.TestSessionState, root string) {
	t.Helper()
	ext := wsext.NewExtension(root, false)
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("workspace InitState: %v", err)
	}
}

// allowAll resolves every permission as allowed; used so the
// session ToolManager doesn't reject providers in tests.
type allowAll struct{}

func (allowAll) Resolve(_ context.Context, _, _ string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (allowAll) Refresh(context.Context) error                                  { return nil }
func (allowAll) Subscribe(context.Context) (<-chan perm.RefreshEvent, error)    { return nil, nil }

// providersConfig is a tiny config.ToolProvidersView whose entries
// are returned verbatim. Saves us from importing config.NewStaticService
// + threading every other field.
type providersConfig struct{ specs []config.ToolProviderSpec }

func (p providersConfig) Providers() []config.ToolProviderSpec { return p.specs }
func (p providersConfig) OnUpdate(func()) func()              { return func() {} }

func TestInitState_NoProviders_NoOp(t *testing.T) {
	ext := NewExtension(providersConfig{}, nil)
	state := fixture.NewTestSessionState("ses-empty")
	state.SetTools(tool.NewToolManager(allowAll{}, nil, nil))
	wireWorkspace(t, state, t.TempDir())
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)
	if h == nil {
		t.Fatalf("handle missing after InitState")
	}
	if len(h.providers) != 0 {
		t.Errorf("providers = %d, want 0", len(h.providers))
	}
}

func TestInitState_SkipsPerAgentLifetime(t *testing.T) {
	ext := NewExtension(providersConfig{specs: []config.ToolProviderSpec{
		{Name: "agent-prov", Type: "mcp", Lifetime: "per_agent", Command: "/bin/true"},
	}}, nil)
	state := fixture.NewTestSessionState("ses-skip")
	state.SetTools(tool.NewToolManager(allowAll{}, nil, nil))
	wireWorkspace(t, state, t.TempDir())
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)
	if len(h.providers) != 0 {
		t.Errorf("per_agent provider was spawned: %d", len(h.providers))
	}
}

func TestInitState_RejectsEmptyCommand(t *testing.T) {
	ext := NewExtension(providersConfig{specs: []config.ToolProviderSpec{
		{Name: "missing-cmd", Type: "mcp", Lifetime: "per_session"},
	}}, nil)
	state := fixture.NewTestSessionState("ses-bad")
	state.SetTools(tool.NewToolManager(allowAll{}, nil, nil))
	wireWorkspace(t, state, t.TempDir())
	err := ext.InitState(context.Background(), state)
	if err == nil {
		t.Fatalf("expected error for empty command")
	}
}

// TestInitState_NoWorkspace_BailsCleanly ensures a session with no
// WorkspaceDir wired (test fixture path) just installs the empty
// handle rather than failing — the extension's contract is "if no
// workspace, no per_session MCPs", consistent with how lifecycle
// previously gated on r.deps.Workspace.
func TestInitState_NoWorkspace_BailsCleanly(t *testing.T) {
	ext := NewExtension(providersConfig{specs: []config.ToolProviderSpec{
		{Name: "x", Type: "mcp", Lifetime: "per_session", Command: "/bin/true"},
	}}, nil)
	state := fixture.NewTestSessionState("ses-noWS")
	state.SetTools(tool.NewToolManager(allowAll{}, nil, nil))
	// no SetWorkspace: WorkspaceDir() returns ("", false).
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Errorf("InitState should not error without workspace: %v", err)
	}
	h := FromState(state)
	if len(h.providers) != 0 {
		t.Errorf("expected no providers without workspace, got %d", len(h.providers))
	}
}

// TestInitState_MissingTools_Errors guards against a misconfigured
// session that has no per-session ToolManager; without one there's
// nowhere to register spawned providers.
func TestInitState_MissingTools_Errors(t *testing.T) {
	ext := NewExtension(providersConfig{specs: []config.ToolProviderSpec{
		{Name: "x", Type: "mcp", Lifetime: "per_session", Command: "/bin/true"},
	}}, nil)
	state := fixture.NewTestSessionState("ses-noTools")
	wireWorkspace(t, state, t.TempDir())
	err := ext.InitState(context.Background(), state)
	if err == nil {
		t.Fatalf("expected error when state.Tools() is nil")
	}
	if !errors.Is(err, errors.New("")) && err.Error() == "" {
		t.Errorf("expected non-empty error, got %v", err)
	}
}

// TestCloseSession_NoOp_WhenNoState exercises the "session never
// inited" path — extensions register CloseSession unconditionally,
// and the hook must be a clean no-op when InitState wasn't run.
func TestCloseSession_NoOp_WhenNoState(t *testing.T) {
	ext := NewExtension(providersConfig{}, nil)
	state := fixture.NewTestSessionState("ses-no-init")
	if err := ext.CloseSession(context.Background(), state); err != nil {
		t.Errorf("CloseSession on uninitialised state should be no-op: %v", err)
	}
}
