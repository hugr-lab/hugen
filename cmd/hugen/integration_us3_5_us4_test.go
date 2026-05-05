// Phase-3.5 US4 integration test: skill_files round-trip (T048).
//
// Exit criterion 10 from design/001-agent-runtime/phase-3.5-spec.md §10:
//
//   - install bundled `duckdb-data` skill on disk;
//   - load it into a session;
//   - call session:skill_files("duckdb-data") through the full
//     ToolManager + SystemProvider pipeline;
//   - read one absolute path from the envelope via bash.read_file
//     and confirm bytes match the bundled file (SC-010 cross-check).
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers"
)

func TestUS3_5_US4_SkillFilesRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping skill_files integration test")
	}

	root := t.TempDir()
	workspaceDir := filepath.Join(root, "workspace")
	sharedDir := filepath.Join(root, "shared")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{workspaceDir, sharedDir, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Install every bundled skill onto disk; this is the same code
	// path cmd/hugen runs at boot. Among the installed entries are
	// `_system` (autoload requirement of analyst skills) and
	// `duckdb-data`.
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

	// SystemProvider is empty post-step-25; left out of the wiring.

	ws := session.NewWorkspace(workspaceDir, true)
	resources := session.NewResources(session.ResourceDeps{
		Providers:  cfgSvc.ToolProviders(),
		Tools:      tools,
		Skills:     skills,
		SkillStore: skillStore,
		Workspace:  ws,
		Logger:     logger,
	})

	router, agent := makeRouter(t)
	mgr := session.NewManager(
		&stubStore{}, agent, router,
		session.NewCommandRegistry(), protocol.NewCodec(), nil,
		session.WithLifecycle(resources),
		session.WithPerms(perms),
		session.WithSessionOptions(
			session.WithTools(tools),
			session.WithSkills(skills),
		),
	)
	// Register Manager as the session ToolProvider so `session:skill_files`
	// is callable through the ToolManager pipeline.
	if err := tools.AddProvider(mgr); err != nil {
		t.Fatalf("AddProvider session: %v", err)
	}

	ctx := context.Background()
	sess, _, err := mgr.Open(ctx, session.OpenRequest{OwnerID: "u"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Terminate(ctx, sess.ID(), "user:/end") })

	// Load duckdb-data into the session.
	if err := skills.Load(ctx, sess.ID(), "duckdb-data"); err != nil {
		t.Fatalf("Load duckdb-data: %v", err)
	}

	snap, err := sess.Tools().Snapshot(ctx, sess.ID())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	skillFiles, ok := findTool(snap.Tools, "session:skill_files")
	if !ok {
		t.Fatalf("session:skill_files missing: %v", toolNames(snap.Tools))
	}
	readFile, ok := findTool(snap.Tools, "bash-mcp:bash.read_file")
	if !ok {
		t.Fatalf("bash-mcp:bash.read_file missing")
	}

	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})
	// session-scoped tools (session:skill_files) recover their *Session
	// via session.WithSession; the live dispatcher does this implicitly
	// in session.Run, the integration test bypasses Run so we wire it
	// here.
	dispatchCtx = session.WithSession(dispatchCtx, sess)

	args, _ := json.Marshal(map[string]string{"name": "duckdb-data"})
	_, eff, err := sess.Tools().Resolve(dispatchCtx, skillFiles, args)
	if err != nil {
		t.Fatalf("Resolve skill_files: %v", err)
	}
	out, err := sess.Tools().Dispatch(dispatchCtx, skillFiles, eff)
	if err != nil {
		t.Fatalf("Dispatch skill_files: %v", err)
	}

	var envelope struct {
		Skill string `json:"skill"`
		Root  string `json:"root"`
		Files []struct {
			Rel  string `json:"rel"`
			Abs  string `json:"abs"`
			Size int64  `json:"size"`
			Mode string `json:"mode"`
		} `json:"files"`
		Truncated bool `json:"truncated,omitempty"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("decode skill_files envelope: %v\n%s", err, out)
	}
	if envelope.Skill != "duckdb-data" {
		t.Errorf("envelope.skill = %q, want duckdb-data", envelope.Skill)
	}
	if !strings.HasSuffix(envelope.Root, filepath.Join("skills/system", "duckdb-data")) {
		t.Errorf("envelope.root = %q, suspicious", envelope.Root)
	}
	if len(envelope.Files) < 5 {
		t.Errorf("expected ≥5 files (SKILL.md + ≥6 references); got %d", len(envelope.Files))
	}
	// Find references/workspace.md so we can read it back.
	var workspaceFile string
	var workspaceSize int64
	for _, f := range envelope.Files {
		if f.Rel == filepath.Join("references", "workspace.md") {
			workspaceFile = f.Abs
			workspaceSize = f.Size
			break
		}
	}
	if workspaceFile == "" {
		t.Fatalf("references/workspace.md missing from skill_files envelope")
	}

	// SC-010 cross-check: read the absolute path via bash.read_file
	// and confirm bytes match the bundled file on disk.
	readArgs, _ := json.Marshal(map[string]string{"path": workspaceFile})
	_, eff2, err := sess.Tools().Resolve(dispatchCtx, readFile, readArgs)
	if err != nil {
		t.Fatalf("Resolve read_file: %v", err)
	}
	bashOut, err := sess.Tools().Dispatch(dispatchCtx, readFile, eff2)
	if err != nil {
		t.Fatalf("Dispatch read_file: %v", err)
	}
	var readEnvelope struct {
		Content string `json:"content"`
		Size    int64  `json:"size"`
	}
	if err := json.Unmarshal(bashOut, &readEnvelope); err != nil {
		t.Fatalf("decode read_file: %v\n%s", err, bashOut)
	}
	if int64(len(readEnvelope.Content)) != workspaceSize {
		t.Errorf("read_file size %d != skill_files size %d",
			len(readEnvelope.Content), workspaceSize)
	}
	if !strings.Contains(readEnvelope.Content, "Workspace conventions") {
		t.Errorf("workspace.md content missing expected heading: %.100s", readEnvelope.Content)
	}
}
