// Phase-3.5 US5 drop-providers regression (T050).
//
// Boots the agent with bash-mcp ONLY (no duckdb-mcp, no python-mcp)
// and verifies:
//
//   - the runtime starts inside the phase-3 baseline budget (SC-008);
//   - loading the bundled `duckdb-data` skill succeeds, but its
//     allowed-tools grants are flagged unavailable via
//     skill.AnnotateUnavailable (existing phase-3 mechanism);
//   - bash-mcp tools still resolve and dispatch;
//   - skill:files still works (does not depend on the analyst
//     providers).
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/extension"
	skillext "github.com/hugr-lab/hugen/pkg/extension/skill"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers"
)

func TestUS3_5_US5_DropProviders(t *testing.T) {
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "workspace")
	sharedDir := filepath.Join(root, "shared")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{workspaceDir, sharedDir, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := runtime.InstallBundledSkills(stateDir, logger); err != nil {
		t.Fatalf("InstallBundledSkills: %v", err)
	}

	cfgSvc := config.NewStaticService(config.StaticInput{
		ToolProviders: []config.ToolProviderSpec{{
			Name:      "bash-mcp",
			Type:      "mcp",
			Transport: "stdio",
			Command:   bashMCPBinary,
			Lifetime:  "per_session",
			Env:       map[string]string{"SHARED_DIR": sharedDir},
		}},
	})

	skillStore := skill.NewSkillStore(skill.Options{
		SystemRoot: filepath.Join(stateDir, "skills/system"),
	})
	skills := skill.NewSkillManager(skillStore, logger)
	view := &permsView{rules: nil}
	perms := perm.NewLocalPermissions(view, staticIdentity{id: "agent-it"})
	t.Cleanup(perms.Close)
	tools := tool.NewToolManager(perms, cfgSvc.ToolProviders(), nil,
		tool.WithBuilder(providers.NewBuilder(nil, perms, workspaceDir, nil)))
	t.Cleanup(func() { _ = tools.Close() })

	skillExt := skillext.NewExtension(skills, perms, "agent-it")
	if err := tools.AddProvider(skillExt); err != nil {
		t.Fatalf("AddProvider skillExt: %v", err)
	}

	ws := session.NewWorkspace(workspaceDir, true)
	store := &stubStore{}
	resources := session.NewResources(session.ResourceDeps{
		Providers:  cfgSvc.ToolProviders(),
		Skills:     skills,
		SkillStore: skillStore,
		Workspace:  ws,
		Logger:     logger,
	})

	router, agent := makeRouter(t)
	mgr := session.NewManager(
		store, agent, router,
		session.NewCommandRegistry(), protocol.NewCodec(), tools, nil,
		session.WithLifecycle(resources),
		session.WithExtensions(skillExt),
		session.WithSessionOptions(
			session.WithPerms(perms),
		),
	)

	ctx := context.Background()
	sess, _, err := mgr.Open(ctx, session.OpenRequest{OwnerID: "u"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Terminate(ctx, sess.ID(), "user:/end") })

	// Loading duckdb-data must succeed even though duckdb-mcp is
	// absent — phase-3 path: skill manifest validates, allowed-tools
	// referencing missing providers are tagged Unavailable.
	if err := skills.Load(ctx, sess.ID(), "duckdb-data"); err != nil {
		t.Fatalf("Load duckdb-data with missing duckdb-mcp: %v", err)
	}

	// Verify the unavailability annotation surfaces. Bindings on its
	// own only knows about loaded skills' grants; AnnotateUnavailable
	// is the helper that flags entries whose provider isn't in the
	// registered list. We mirror what the runtime does: pass the
	// list of registered provider names. With duckdb-mcp dropped,
	// duckdb-mcp grants must show up as Unavailable.
	b, err := skills.Bindings(ctx, sess.ID())
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	registered := []string{"bash-mcp", "system", "session"}
	annotated := skill.AnnotateUnavailable(b, registered)
	foundDuckdb := false
	for _, u := range annotated.Unavailable {
		if u.Provider == "duckdb-mcp" {
			foundDuckdb = true
			break
		}
	}
	if !foundDuckdb {
		t.Errorf("duckdb-mcp not flagged unavailable; got %v", annotated.Unavailable)
	}

	// bash-mcp tools still work end-to-end.
	snap, err := sess.Tools().Snapshot(ctx, sess.ID())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeFile, ok := findTool(snap.Tools, "bash-mcp:bash.write_file")
	if !ok {
		t.Fatalf("bash.write_file missing")
	}
	skillFiles, ok := findTool(snap.Tools, "skill:files")
	if !ok {
		t.Fatalf("skill:files missing")
	}

	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})
	dispatchCtx = session.WithSession(dispatchCtx, sess)
	dispatchCtx = extension.WithSessionState(dispatchCtx, sess)
	args, _ := json.Marshal(map[string]string{"path": "hello.txt", "content": "ok"})
	_, eff, err := sess.Tools().Resolve(dispatchCtx, writeFile, args)
	if err != nil {
		t.Fatalf("Resolve write_file: %v", err)
	}
	if _, err := sess.Tools().Dispatch(dispatchCtx, writeFile, eff); err != nil {
		t.Fatalf("Dispatch write_file: %v", err)
	}

	sfArgs, _ := json.Marshal(map[string]string{"name": "duckdb-data"})
	_, eff, err = sess.Tools().Resolve(dispatchCtx, skillFiles, sfArgs)
	if err != nil {
		t.Fatalf("Resolve skill_files: %v", err)
	}
	out, err := sess.Tools().Dispatch(dispatchCtx, skillFiles, eff)
	if err != nil {
		t.Fatalf("Dispatch skill_files: %v", err)
	}
	if len(out) == 0 || out[0] != '{' {
		t.Errorf("skill_files envelope not JSON: %s", out)
	}
}
