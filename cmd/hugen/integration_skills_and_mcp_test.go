package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	mcpext "github.com/hugr-lab/hugen/pkg/extension/mcp"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// bashMCPBinary holds the path to a freshly-built bash-mcp binary
// shared across every subtest. TestMain populates it before any
// test runs.
var bashMCPBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hugen-it-bashmcp-*")
	if err != nil {
		slog.Default().Error("integration: mktemp", "err", err)
		os.Exit(2)
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "bash-mcp")
	cmd := exec.Command("go", "build", "-o", bin, "../../mcp/bash-mcp")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		slog.Default().Error("integration: build bash-mcp", "err", err)
		os.Exit(2)
	}
	bashMCPBinary = bin
	os.Exit(m.Run())
}

// integrationCore is the minimal runtime.Core-shaped harness the
// integration tests share. It owns the workspace dir, ToolManager,
// SkillManager, SessionManager wired with the bash-mcp lifecycle.
type integrationCore struct {
	workspaceDir string
	sharedDir    string
	stateDir     string
	tools        *tool.ToolManager
	skills       *skill.SkillManager
	skillStore   skill.SkillStore
	manager      *manager.Manager
}

func newIntegrationCore(t *testing.T, ruleSet []config.PermissionRule) *integrationCore {
	t.Helper()
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "workspace")
	sharedDir := filepath.Join(root, "shared")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{workspaceDir, sharedDir, stateDir, filepath.Join(stateDir, "skills/hub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfgSvc := config.NewStaticService(config.StaticInput{
		ToolProviders: []config.ToolProviderSpec{{
			Name:      "bash-mcp",
			Type:      "mcp",
			Transport: "stdio",
			Command:   bashMCPBinary,
			Lifetime:  "per_session",
			Env: map[string]string{
				"SHARED_DIR": sharedDir,
			},
		}},
	})

	skillStore := skill.NewSkillStore(skill.Options{
		SystemFS: runtime.SystemSkillsFS(),
		HubRoot:  filepath.Join(stateDir, "skills/hub"),
	})
	skills := skill.NewSkillManager(skillStore, nil)
	view := &permsView{rules: ruleSet}
	perms := perm.NewLocalPermissions(view, staticIdentity{id: "agent-it"})
	t.Cleanup(perms.Close)
	tools := tool.NewToolManager(perms, nil, nil)
	t.Cleanup(func() { _ = tools.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	router, agent := makeRouter(t)
	mgr := manager.NewManager(
		&stubStore{}, agent, router,
		session.NewCommandRegistry(), protocol.NewCodec(), tools, nil,
		manager.WithExtensions(
			wsext.NewExtension(workspaceDir, true),
			mcpext.NewExtension(cfgSvc.ToolProviders(), logger),
		),
	)

	return &integrationCore{
		workspaceDir: workspaceDir,
		sharedDir:    sharedDir,
		stateDir:     stateDir,
		tools:        tools,
		skills:       skills,
		skillStore:   skillStore,
		manager:      mgr,
	}
}

// makeRouter constructs a stub-backed ModelRouter + Agent for
// integration sessions. The /skill and bash-mcp dispatch paths
// don't ever call the model, so a stub is enough.
func makeRouter(t *testing.T) (*model.ModelRouter, *session.Agent) {
	t.Helper()
	spec := model.ModelSpec{Provider: "fake", Name: "f"}
	router, err := model.NewModelRouter(map[model.Intent]model.ModelSpec{
		model.IntentDefault: spec,
	}, map[model.ModelSpec]model.Model{spec: stubModel{}})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	agent, err := session.NewAgent("agent-it", "hugen", staticIdentity{id: "agent-it"}, "", nil)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	return router, agent
}

func TestSkill_DependencyCycle(t *testing.T) {
	a := []byte(`---
name: a
description: a.
license: MIT
allowed-tools: []
metadata:
  hugen:
    requires: [b]
---
body`)
	b := []byte(`---
name: b
description: b.
license: MIT
allowed-tools: []
metadata:
  hugen:
    requires: [a]
---
body`)
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{"a": a, "b": b}})
	mgr := skill.NewSkillManager(store, nil)
	_, err := mgr.ResolveClosure(context.Background(), "a")
	if !errors.Is(err, skill.ErrSkillCycle) {
		t.Errorf("err = %v, want ErrSkillCycle", err)
	}
}

func TestSkill_ThirdPartyDropIn(t *testing.T) {
	// Third-party (admin-delivered) skills land on the hub backend;
	// since the OriginCommunity tier was folded into OriginHub at
	// the system/hub split, this test exercises the hub backend's
	// "skill without metadata.hugen block" path.
	root := t.TempDir()
	hubRoot := filepath.Join(root, "hub")
	skillDir := filepath.Join(hubRoot, "weather")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `---
name: weather
description: a third-party skill that has no metadata.hugen block.
license: Apache-2.0
allowed-tools: []
---
# Weather skill body.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	store := skill.NewSkillStore(skill.Options{HubRoot: hubRoot})
	mgr := skill.NewSkillManager(store, nil)
	if _, err := mgr.ResolveClosure(context.Background(), "weather"); err != nil {
		t.Fatalf("ResolveClosure third-party skill: %v", err)
	}
	got, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, s := range got {
		if s.Manifest.Name == "weather" && s.Origin == skill.OriginHub {
			found = true
		}
	}
	if !found {
		t.Errorf("third-party skill not surfaced by store: %+v", got)
	}
}

func TestBashMCP_WriteRead(t *testing.T) {
	core := newIntegrationCore(t, nil)

	ctx := context.Background()
	sess, _, err := core.manager.Open(ctx, session.OpenRequest{OwnerID: "u"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer core.manager.Terminate(ctx, sess.ID(), "user:/end")

	snap, err := sess.Tools().Snapshot(ctx, sess.ID())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeTool, ok := findTool(snap.Tools, "bash-mcp:bash.write_file")
	if !ok {
		t.Fatalf("bash.write_file not in snapshot: %v", toolNames(snap.Tools))
	}
	readTool, _ := findTool(snap.Tools, "bash-mcp:bash.read_file")

	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})
	_, eff, err := sess.Tools().Resolve(dispatchCtx, writeTool, json.RawMessage(`{"path":"out.txt","content":"hello world"}`))
	if err != nil {
		t.Fatalf("Resolve write: %v", err)
	}
	if _, err := sess.Tools().Dispatch(dispatchCtx, writeTool, eff); err != nil {
		t.Fatalf("Dispatch write: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(core.workspaceDir, sess.ID(), "out.txt"))
	if err != nil {
		t.Fatalf("read host file: %v", err)
	}
	if string(content) != "hello world" {
		t.Errorf("file content = %q, want %q", content, "hello world")
	}

	_, eff2, err := sess.Tools().Resolve(dispatchCtx, readTool, json.RawMessage(`{"path":"out.txt"}`))
	if err != nil {
		t.Fatalf("Resolve read: %v", err)
	}
	res, err := sess.Tools().Dispatch(dispatchCtx, readTool, eff2)
	if err != nil {
		t.Fatalf("Dispatch read: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(res, &body); err != nil {
		t.Fatalf("decode read result: %v", err)
	}
	if body["content"] != "hello world" {
		t.Errorf("read content = %v, want hello world", body["content"])
	}
}

func TestBashMCP_PermissionDenied(t *testing.T) {
	rules := []config.PermissionRule{
		{Type: "hugen:tool:bash-mcp", Field: "bash.write_file", Disabled: true},
	}
	core := newIntegrationCore(t, rules)

	ctx := context.Background()
	sess, _, err := core.manager.Open(ctx, session.OpenRequest{OwnerID: "u"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer core.manager.Terminate(ctx, sess.ID(), "user:/end")

	snap, _ := sess.Tools().Snapshot(ctx, sess.ID())
	writeTool, ok := findTool(snap.Tools, "bash-mcp:bash.write_file")
	if !ok {
		t.Fatalf("bash.write_file missing")
	}
	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})
	_, _, err = sess.Tools().Resolve(dispatchCtx, writeTool, json.RawMessage(`{"path":"x","content":"y"}`))
	if !errors.Is(err, tool.ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
}

// TestWorkspace_OrphanSweep moved to pkg/extension/workspace alongside
// the (now-internal) tracker — same-package access lets the test
// drive sweepOrphans without re-exporting the API.

func TestWorkspace_SharedRoundTrip_AndCleanupOnClose(t *testing.T) {
	core := newIntegrationCore(t, nil)

	ctx := context.Background()
	sess, _, err := core.manager.Open(ctx, session.OpenRequest{OwnerID: "u"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	snap, _ := sess.Tools().Snapshot(ctx, sess.ID())
	writeTool, ok := findTool(snap.Tools, "bash-mcp:bash.write_file")
	if !ok {
		t.Fatalf("bash.write_file missing")
	}
	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})
	sharedFile := filepath.Join(core.sharedDir, "seed.csv")
	writeArgs, _ := json.Marshal(map[string]any{
		"path":    sharedFile,
		"content": "k,v\na,1\n",
	})
	_, eff, err := sess.Tools().Resolve(dispatchCtx, writeTool, writeArgs)
	if err != nil {
		t.Fatalf("Resolve write shared: %v", err)
	}
	if _, err := sess.Tools().Dispatch(dispatchCtx, writeTool, eff); err != nil {
		t.Fatalf("Dispatch write shared: %v", err)
	}

	body, err := os.ReadFile(sharedFile)
	if err != nil {
		t.Fatalf("read shared: %v", err)
	}
	if string(body) != "k,v\na,1\n" {
		t.Errorf("shared content = %q", body)
	}

	sessDir := filepath.Join(core.workspaceDir, sess.ID())
	if _, err := os.Stat(sessDir); err != nil {
		t.Fatalf("workspace dir missing before close: %v", err)
	}
	if err := core.manager.Terminate(ctx, sess.ID(), "user:/end"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Errorf("workspace dir not cleaned: %v", err)
	}
	if _, err := os.Stat(sharedFile); err != nil {
		t.Errorf("shared file removed: %v", err)
	}
}

func TestSkill_Validate_HappyPathAndInvalid(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "hugen-skill-validate")
	cmd := exec.Command("go", "build", "-o", bin, "../hugen-skill-validate")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build validate: %v\n%s", err, out)
	}

	valid := filepath.Join(dir, "good")
	_ = os.MkdirAll(valid, 0o755)
	if err := os.WriteFile(filepath.Join(valid, "SKILL.md"), []byte(`---
name: ok
description: ok skill.
license: MIT
allowed-tools: []
---
body
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, valid).CombinedOutput(); err != nil {
		t.Errorf("validate good failed: %v\n%s", err, out)
	}

	bad := filepath.Join(dir, "bad")
	_ = os.MkdirAll(bad, 0o755)
	if err := os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte(`---
name: BAD-NAME!
description: bad.
---
body`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, bad).CombinedOutput(); err == nil {
		t.Errorf("validate bad expected non-zero; got OK. output: %s", out)
	}
}

// --- helpers ---

func findTool(tools []tool.Tool, name string) (tool.Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return tool.Tool{}, false
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}
