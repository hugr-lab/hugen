package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// allowAllPerms resolves every permission as allowed so the test
// ToolManager forwards hook dispatches to the fake provider.
type allowAllPerms struct{}

func (allowAllPerms) Resolve(context.Context, string, string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (allowAllPerms) Refresh(context.Context) error { return nil }
func (allowAllPerms) Subscribe(context.Context) (<-chan perm.RefreshEvent, error) {
	return nil, nil
}

// fakeHookProvider exposes a single tool and records the args every
// Call receives so the test can assert template rendering. The
// result envelope is configurable per call name.
type fakeHookProvider struct {
	toolName string
	// lastArgs captures the effective args of the most recent Call.
	lastArgs json.RawMessage
	// result is returned verbatim from Call; callErr (when set) is
	// returned instead to exercise the dispatch-error → failed-outcome
	// fold.
	result  json.RawMessage
	callErr error
}

func (p *fakeHookProvider) Name() string             { return providerPrefix(p.toolName) }
func (p *fakeHookProvider) Lifetime() tool.Lifetime  { return tool.LifetimePerSession }
func (p *fakeHookProvider) List(context.Context) ([]tool.Tool, error) {
	return []tool.Tool{{
		Name:             p.toolName,
		Provider:         providerPrefix(p.toolName),
		PermissionObject: "hugen:tool:" + providerPrefix(p.toolName),
	}}, nil
}
func (p *fakeHookProvider) Call(_ context.Context, _ string, args json.RawMessage) (json.RawMessage, error) {
	p.lastArgs = args
	if p.callErr != nil {
		return nil, p.callErr
	}
	return p.result, nil
}
func (p *fakeHookProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (p *fakeHookProvider) Close() error { return nil }

func providerPrefix(name string) string {
	if i := strings.IndexAny(name, ":."); i > 0 {
		return name[:i]
	}
	return name
}

// stateWithProvider builds a fakeState wired to a real ToolManager
// that hosts p, so runMissionHook can resolve + dispatch p's tool.
func stateWithProvider(t *testing.T, id string, p tool.ToolProvider) *fakeState {
	t.Helper()
	tm := tool.NewToolManager(allowAllPerms{}, nil, nil)
	if err := tm.AddProvider(p); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	return &fakeState{id: id, tools: tm}
}

func TestRunMissionHook_RendersArgsAndPasses(t *testing.T) {
	p := &fakeHookProvider{
		toolName: "bash:run",
		result:   json.RawMessage(`{"exit_code":0,"stdout":"ok","stderr":""}`),
	}
	state := stateWithProvider(t, "ses-hook-ok", p)

	hook := MissionHook{
		Tool: "bash:run",
		Args: map[string]any{
			"command": "cp -r {{.MissionSkill}}/templates/. {{.MissionDir}}/ && echo {{.Goal}}",
			"roles":   []any{"{{index .Roles 0}}", "{{index .Roles 1}}"},
		},
	}
	view := hookView{
		MissionDir:   "/ws/root/mis",
		MissionSkill: "/skills/hub/analyst",
		Goal:         "build-report",
		Roles:        []string{"schema-explorer", "query-builder"},
	}

	out, err := runMissionHook(context.Background(), state, hook, view)
	if err != nil {
		t.Fatalf("runMissionHook: %v", err)
	}
	if out.Failed {
		t.Errorf("outcome.Failed = true, want false (reason=%q)", out.Reason)
	}
	if out.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", out.ExitCode)
	}

	// Verify the args reached the provider fully rendered.
	var got struct {
		Command string   `json:"command"`
		Roles   []string `json:"roles"`
	}
	if err := json.Unmarshal(p.lastArgs, &got); err != nil {
		t.Fatalf("unmarshal lastArgs: %v (%s)", err, p.lastArgs)
	}
	wantCmd := "cp -r /skills/hub/analyst/templates/. /ws/root/mis/ && echo build-report"
	if got.Command != wantCmd {
		t.Errorf("command = %q, want %q", got.Command, wantCmd)
	}
	if len(got.Roles) != 2 || got.Roles[0] != "schema-explorer" || got.Roles[1] != "query-builder" {
		t.Errorf("roles = %v, want [schema-explorer query-builder]", got.Roles)
	}
}

func TestRunMissionHook_NonZeroExitFails(t *testing.T) {
	p := &fakeHookProvider{
		toolName: "python:run_script",
		result:   json.RawMessage(`{"exit_code":2,"stdout":"","stderr":"missing data-model.md"}`),
	}
	state := stateWithProvider(t, "ses-hook-fail", p)

	out, err := runMissionHook(context.Background(), state, MissionHook{Tool: "python:run_script"}, hookView{})
	if err != nil {
		t.Fatalf("runMissionHook: %v", err)
	}
	if !out.Failed {
		t.Fatalf("outcome.Failed = false, want true")
	}
	if out.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", out.ExitCode)
	}
	if !strings.Contains(out.Reason, "data-model.md") {
		t.Errorf("Reason = %q, want it to carry stderr", out.Reason)
	}
}

func TestRunMissionHook_IsErrorEnvelopeFails(t *testing.T) {
	p := &fakeHookProvider{
		toolName: "bash:run",
		result:   json.RawMessage(`{"is_error":true,"text":"command not found: foo"}`),
	}
	state := stateWithProvider(t, "ses-hook-mcperr", p)

	out, err := runMissionHook(context.Background(), state, MissionHook{Tool: "bash:run"}, hookView{})
	if err != nil {
		t.Fatalf("runMissionHook: %v", err)
	}
	if !out.Failed {
		t.Fatalf("outcome.Failed = false, want true for is_error envelope")
	}
	if !strings.Contains(out.Reason, "command not found") {
		t.Errorf("Reason = %q, want the error text", out.Reason)
	}
}

func TestRunMissionHook_PlainResultPasses(t *testing.T) {
	p := &fakeHookProvider{
		toolName: "hugr:query",
		result:   json.RawMessage(`{"text":"[{\"n\":1}]"}`),
	}
	state := stateWithProvider(t, "ses-hook-plain", p)

	out, err := runMissionHook(context.Background(), state, MissionHook{Tool: "hugr:query"}, hookView{})
	if err != nil {
		t.Fatalf("runMissionHook: %v", err)
	}
	if out.Failed {
		t.Errorf("outcome.Failed = true, want false for a plain result")
	}
}

func TestRunMissionHook_MissingToolErrors(t *testing.T) {
	p := &fakeHookProvider{toolName: "bash:run", result: json.RawMessage(`{}`)}
	state := stateWithProvider(t, "ses-hook-missing", p)

	_, err := runMissionHook(context.Background(), state, MissionHook{Tool: "python:run_script"}, hookView{})
	if err == nil {
		t.Fatalf("expected error for a tool absent from the snapshot")
	}
	if !strings.Contains(err.Error(), "not in session snapshot") {
		t.Errorf("err = %v, want a snapshot-miss error", err)
	}
}

func TestRunMissionHook_DispatchErrorFoldsToFailed(t *testing.T) {
	p := &fakeHookProvider{toolName: "bash:run", callErr: context.DeadlineExceeded}
	state := stateWithProvider(t, "ses-hook-disp", p)

	out, err := runMissionHook(context.Background(), state, MissionHook{Tool: "bash:run"}, hookView{})
	if err != nil {
		t.Fatalf("runMissionHook returned runner error, want folded outcome: %v", err)
	}
	if !out.Failed {
		t.Fatalf("outcome.Failed = false, want true on dispatch error")
	}
}

func TestRenderTemplateString_MissingFieldErrors(t *testing.T) {
	_, err := renderTemplateString("{{.Nope}}", hookView{})
	if err == nil {
		t.Fatalf("expected missingkey=error to fail on an unknown field")
	}
}
