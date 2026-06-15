package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeToolProvider exposes a fixed tool set so the save-time tool-name
// validation has a real registry to check against.
type fakeToolProvider struct {
	name  string
	tools []tool.Tool
}

func (p *fakeToolProvider) Name() string            { return p.name }
func (p *fakeToolProvider) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }
func (p *fakeToolProvider) List(context.Context) ([]tool.Tool, error) {
	return p.tools, nil
}
func (p *fakeToolProvider) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (p *fakeToolProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (p *fakeToolProvider) Close() error { return nil }

func newToolManagerWithPython(t *testing.T) *tool.ToolManager {
	t.Helper()
	m := tool.NewToolManager(&fakePerms{}, nil, slog.New(slog.DiscardHandler))
	prov := &fakeToolProvider{name: "python-mcp", tools: []tool.Tool{
		{Name: "python-mcp:run_script", Provider: "python-mcp"},
		{Name: "python-mcp:install", Provider: "python-mcp"},
	}}
	if err := m.AddProvider(prov); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	return m
}

// newToolNameFixture wires a save-capable extension whose session has
// both a real ToolManager (python-mcp provider) AND a skill literally
// named "hugr-data" — so the unknown-tool hint can distinguish a
// skill-name-as-provider mistake.
func newToolNameFixture(t *testing.T) (*Extension, *fixture.TestSessionState, string) {
	t.Helper()
	localRoot := t.TempDir()
	store := skillpkg.NewSkillStore(skillpkg.Options{
		LocalRoot: localRoot,
		Inline: map[string][]byte{
			"alpha":     []byte(inlineAlphaManifest),
			"hugr-data": []byte("---\nname: hugr-data\ndescription: a skill, not a provider.\nlicense: MIT\n---\nbody\n"),
		},
	})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-toolname")
	state := fixture.NewTestSessionState("ses-tn").WithDepth(2)
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	wsExt := wsext.NewExtension(t.TempDir(), nil)
	if err := wsExt.InitState(context.Background(), state); err != nil {
		t.Fatalf("workspace InitState: %v", err)
	}
	state.SetTools(newToolManagerWithPython(t))
	return ext, state, wsext.FromState(state).Dir()
}

const taskManifestTmpl = `---
name: %s
description: road movement report task.
license: MIT
metadata:
  hugen:
    task:
      eligible: true
      kind: worker
      goal_summary: build the report
      allowed_tools_default:
%s---
body
`

func taskManifest(name string, tools []string) string {
	var b strings.Builder
	for _, tl := range tools {
		b.WriteString("        - ")
		b.WriteString(tl)
		b.WriteString("\n")
	}
	return fmt.Sprintf(taskManifestTmpl, name, b.String())
}

// TestCallSave_RejectsUnknownToolName proves the registry-backed half
// of D2: an allowed_tools_default entry whose provider does not exist
// is rejected with ErrUnknownToolName, and when the bad "provider" is
// actually a skill name the hint says so.
func TestCallSave_RejectsUnknownToolName(t *testing.T) {
	ext, state, wsDir := newToolNameFixture(t)
	md := taskManifest("roadmove", []string{"python-mcp:run_script", "hugr-data:execute"})
	dir := writeBundle(t, wsDir, "roadmove", md, nil)
	_, err := ext.Call(newCallCtx(state), "skill:save", saveArgs(dir, false, false))
	if !errors.Is(err, ErrUnknownToolName) {
		t.Fatalf("err = %v, want ErrUnknownToolName", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "hugr-data") {
		t.Errorf("error should name the bad entry: %q", msg)
	}
	if !strings.Contains(msg, "SKILL") {
		t.Errorf("error should hint that hugr-data is a SKILL not a provider: %q", msg)
	}
	// The real tool must NOT appear in the problem list.
	if strings.Contains(msg, "python-mcp:run_script") {
		t.Errorf("real tool wrongly flagged: %q", msg)
	}
}

// TestCallSave_AcceptsRealToolNames proves the validation passes when
// every allowed_tools_default entry resolves against the registry.
func TestCallSave_AcceptsRealToolNames(t *testing.T) {
	ext, state, wsDir := newToolNameFixture(t)
	md := taskManifest("roadok", []string{"python-mcp:run_script", "python-mcp:install"})
	dir := writeBundle(t, wsDir, "roadok", md, nil)
	out, err := ext.Call(newCallCtx(state), "skill:save", saveArgs(dir, false, true))
	if err != nil {
		t.Fatalf("validate_only with real tools: %v", err)
	}
	res := decodeSaveResult(t, out)
	if !res.Valid {
		t.Errorf("verdict not valid: %+v", res)
	}
}

// TestCallSave_AcceptsToolWildcard proves a provider:* wildcard passes
// when the provider has at least one matching tool.
func TestCallSave_AcceptsToolWildcard(t *testing.T) {
	ext, state, wsDir := newToolNameFixture(t)
	md := taskManifest("roadwild", []string{"python-mcp:*"})
	dir := writeBundle(t, wsDir, "roadwild", md, nil)
	if _, err := ext.Call(newCallCtx(state), "skill:save", saveArgs(dir, false, true)); err != nil {
		t.Fatalf("validate_only with wildcard: %v", err)
	}
}

// TestCallSave_RejectsMisplacedTaskBlock proves the pure half of D2:
// a top-level `task:` key (the dead-task run-2 failure) is caught even
// without a wired ToolManager.
func TestCallSave_RejectsMisplacedTaskBlock(t *testing.T) {
	ext, state, _, _, wsDir := newSaveFixture(t)
	md := "---\nname: misplaced\ndescription: x.\nlicense: MIT\ntask:\n  eligible: true\n  kind: worker\n---\nbody\n"
	dir := writeBundle(t, wsDir, "misplaced", md, nil)
	_, err := ext.Call(newCallCtx(state), "skill:save", saveArgs(dir, false, false))
	if !errors.Is(err, skillpkg.ErrTaskBlockMisplaced) {
		t.Errorf("err = %v, want ErrTaskBlockMisplaced", err)
	}
}

func TestToolEntryMatches(t *testing.T) {
	realTools := map[string]struct{}{
		"python-mcp:run_script": {},
		"bash-mcp:bash.shell":   {},
	}
	providers := map[string]struct{}{
		"python-mcp": {},
		"bash-mcp":   {},
	}
	cases := []struct {
		entry string
		want  bool
	}{
		{"python-mcp:run_script", true},  // exact
		{"python-mcp:*", true},           // wildcard, provider has tools
		{"python-mcp:run_*", true},       // prefix wildcard
		{"python-mcp", true},             // bare provider (lenient)
		{"python-mcp:nope", false},       // real provider, bad tool
		{"hugr-data:execute", false},     // unknown provider
		{"hugr-data:*", false},           // wildcard on unknown provider
		{"bash-mcp:bash.shell", true},    // exact dotted
		{"bash-mcp:bash.*", true},        // dotted prefix wildcard
	}
	for _, c := range cases {
		if got := toolEntryMatches(c.entry, realTools, providers); got != c.want {
			t.Errorf("toolEntryMatches(%q) = %v, want %v", c.entry, got, c.want)
		}
	}
}
