package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// integration_us5_test.go covers exit criterion 4c from
// design/001-agent-runtime/phase-3-spec.md §11 — universal local
// agent (no Hugr):
//
//   - the agent boots with no `hugr-main`/`hugr-query` providers
//     and no `hugr` auth source;
//   - non-data tools (bash.write_file / bash.read_file) work
//     normally;
//   - a skill that grants Hugr tools loads without error and is
//     visible to /skill list, but the missing provider is tagged
//     in the bindings so callers see the gap clearly;
//   - the runtime never instantiates a Hugr token store and
//     therefore never serves /api/auth/agent-token.
//
// The test exercises the contracts hugen relies on rather than
// spawning the full cmd/hugen binary; cmd/hugen tests (config_test.go)
// already verify the hugr.URL == "" path skips BuildHugrSource.

// fsToolStub implements bash.write_file + bash.read_file as a
// fake provider so we can exercise the no-Hugr happy path without
// linking the bash-mcp subprocess into pkg/runtime tests.
type fsToolStub struct {
	files map[string]string
}

func (p *fsToolStub) Name() string                                                 { return "bash-mcp" }
func (p *fsToolStub) Lifetime() tool.Lifetime                                      { return tool.LifetimePerSession }
func (p *fsToolStub) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) { return nil, nil }
func (p *fsToolStub) Close() error                                                 { return nil }
func (p *fsToolStub) List(context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{Name: "bash-mcp:write_file", Provider: "bash-mcp", PermissionObject: "hugen:tool:bash-mcp"},
		{Name: "bash-mcp:read_file", Provider: "bash-mcp", PermissionObject: "hugen:tool:bash-mcp"},
	}, nil
}

func (p *fsToolStub) Call(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "write_file":
		var in struct{ Path, Content string }
		_ = json.Unmarshal(args, &in)
		p.files[in.Path] = in.Content
		return json.RawMessage(`{"written":true}`), nil
	case "read_file":
		var in struct{ Path string }
		_ = json.Unmarshal(args, &in)
		body := p.files[in.Path]
		return json.Marshal(map[string]string{"content": body})
	}
	return nil, nil
}

// TestUS5_NoHugr_BashFlowWorks — the agent boots without any
// Hugr providers and a bash-only round-trip succeeds.
func TestUS5_NoHugr_BashFlowWorks(t *testing.T) {
	files := &fsToolStub{files: map[string]string{}}

	mdl := &scriptedToolModel{turns: [][]model.Chunk{
		{{ToolCall: &model.ChunkToolCall{
			ID: "tc-write", Name: "bash-mcp:write_file",
			Args: map[string]any{"path": "out.txt", "content": "hello"},
		}}},
		{{ToolCall: &model.ChunkToolCall{
			ID: "tc-read", Name: "bash-mcp:read_file",
			Args: map[string]any{"path": "out.txt"},
		}}},
		{{Content: ptr("done"), Final: true}},
	}}

	store := newFakeStore()
	_ = store.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "ag01", Status: StatusActive})
	tm := tool.NewToolManager(permsAllow{}, nil, nil, nil)
	if err := tm.AddProvider(files); err != nil {
		t.Fatal(err)
	}

	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("ag01", "hugen", &fakeIdentity{id: "ag01"}, "")
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession("s1", agent, store, router, NewCommandRegistry(), protocol.NewCodec(), nil, WithTools(tm))
	sess.materialised.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sess.Run(ctx) }()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	if got := files.files["out.txt"]; got != "hello" {
		t.Errorf("written file = %q, want %q", got, "hello")
	}
}

// TestUS5_HugrSkillLoadsAndReportsUnavailable — a skill that
// grants Hugr-flavoured tools loads cleanly under a no-Hugr
// deployment; AnnotateUnavailable surfaces the gap so callers
// can show "tools unavailable" instead of crashing the load.
func TestUS5_HugrSkillLoadsAndReportsUnavailable(t *testing.T) {
	manifest := []byte(`---
name: hugr-data
description: Hugr data access skill.
license: MIT
allowed-tools:
  - provider: hugr-main
    tools: ["data-*", "discovery-*"]
  - provider: bash-mcp
    tools: [read_file]
---
Hugr data skill body.
`)
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{
		"hugr-data": manifest,
	}})
	mgr := skill.NewSkillManager(store, nil)

	if err := mgr.Load(context.Background(), "s1", "hugr-data"); err != nil {
		t.Fatalf("Load (Hugr absent): %v", err)
	}
	b, err := mgr.Bindings(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	annotated := skill.AnnotateUnavailable(b, []string{"bash-mcp", "system"})
	if len(annotated.Unavailable) != 1 {
		t.Fatalf("Unavailable = %+v, want 1 entry", annotated.Unavailable)
	}
	if annotated.Unavailable[0].Provider != "hugr-main" {
		t.Errorf("provider = %q, want hugr-main", annotated.Unavailable[0].Provider)
	}
	if !contains(annotated.Unavailable[0].Tools, "data-*") {
		t.Errorf("missing data-*: %+v", annotated.Unavailable[0].Tools)
	}
}

// TestUS5_HugrAbsentToolDispatchFails — when a skill grants
// hugr-main:* but no provider is registered, dispatching the
// tool surfaces ErrUnknownProvider rather than a panic.
func TestUS5_HugrAbsentToolDispatchFails(t *testing.T) {
	tm := tool.NewToolManager(permsAllow{}, nil, nil, nil)
	tl := tool.Tool{
		Name:             "hugr-main:data-execute_query",
		Provider:         "hugr-main",
		PermissionObject: "hugen:tool:hugr-main",
	}
	_, err := tm.Dispatch(context.Background(), tl, json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("expected error dispatching unknown provider")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("err = %v; want unknown provider", err)
	}
}

// TestUS5_PolicyServiceWithoutHugr — LocalPermissions remains
// the choice when the deployment skips Hugr; Resolve still works
// for the bash tool floor without contacting any remote.
func TestUS5_PolicyServiceWithoutHugr(t *testing.T) {
	v := &fakeUS5View{rules: []perm.Rule{
		{Type: "hugen:tool:bash-mcp", Field: "*"},
	}}
	p := perm.NewLocalPermissions(v, &fakeIdentity{id: "ag01"})
	got, err := p.Resolve(context.Background(), "hugen:tool:bash-mcp", "read_file")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Disabled || got.Hidden {
		t.Errorf("permission flagged blocked under empty floor: %+v", got)
	}
}

// fakeUS5View mirrors the perm.PermissionsView surface so
// LocalPermissions can spin up without the cmd/hugen wiring.
type fakeUS5View struct {
	rules []perm.Rule
}

func (v *fakeUS5View) Rules() []perm.Rule             { return v.rules }
func (v *fakeUS5View) RefreshInterval() time.Duration { return time.Hour }
func (v *fakeUS5View) RemoteEnabled() bool            { return false }
func (v *fakeUS5View) OnUpdate(_ func()) func()       { return func() {} }
