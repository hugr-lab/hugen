package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	artifactext "github.com/hugr-lab/hugen/pkg/extension/artifact"
	compactorext "github.com/hugr-lab/hugen/pkg/extension/compactor"
	liveviewext "github.com/hugr-lab/hugen/pkg/extension/liveview"
	mcpext "github.com/hugr-lab/hugen/pkg/extension/mcp"
	missionext "github.com/hugr-lab/hugen/pkg/extension/mission"
	notepadext "github.com/hugr-lab/hugen/pkg/extension/notepad"
	planext "github.com/hugr-lab/hugen/pkg/extension/plan"
	recapext "github.com/hugr-lab/hugen/pkg/extension/recap"
	schedext "github.com/hugr-lab/hugen/pkg/extension/scheduler"
	skillext "github.com/hugr-lab/hugen/pkg/extension/skill"
	stuckdetectorext "github.com/hugr-lab/hugen/pkg/extension/stuckdetector"
	taskext "github.com/hugr-lab/hugen/pkg/extension/task"
	wbext "github.com/hugr-lab/hugen/pkg/extension/whiteboard"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// phaseExtensions runs phase 8.5: builds the runtime's session
// extensions (notepad, plan, whiteboard, skill — added per
// migration), registers each ToolProvider-implementing extension on
// Core.Tools so their catalogue surfaces to every session, and
// merges any Commander-contributed slash commands onto Core.Commands.
//
// Capability hooks beyond ToolProvider / Commander (StateInitializer,
// Recovery, Closer, Advertiser, ToolFilter, FrameRouter) are
// dispatched at runtime by Session.NewSession and friends —
// phaseExtensions only owns construction + agent-level registrations.
//
// Today only the notepad extension migrated to this model; the rest
// still live as session: tools registered directly on Session.
// Adding plan/whiteboard/skill follows the same shape: build
// instance with deps + append to Core.Extensions + (if ToolProvider)
// AddProvider on Core.Tools + (if Commander) Register on
// Core.Commands.
func phaseExtensions(_ context.Context, core *Core) error {
	// Order matters once we have read-from-state dependencies:
	// the workspace extension must run InitState before mcpext
	// because mcpext reads workspace.FromState(state) for
	// SESSION_DIR / WORKSPACES_ROOT. Skill / notepad have no
	// inter-extension state reads at InitState; their order is
	// purely aesthetic.
	// Agent-level prompt renderer — injected into the compactor's
	// constructor (it's a system constant that depends only on the
	// embedded templates + logger, NOT on session state). Built lazily;
	// phaseSessionManager reuses the same cached instance.
	renderer, err := core.PromptRenderer()
	if err != nil {
		return fmt.Errorf("build prompt renderer: %w", err)
	}

	// Compactor lands AFTER notepad so its Advertiser Block C composes
	// after notepad's Block B. γ wires the operator config as a
	// CompactorView — the resolver re-reads each fire so a future
	// hot-reload propagates without re-creating the extension.
	// SkillCatalog gives the per-tier resolver mission + per-role
	// manifest overrides. Held as a var so the context:* provider below
	// can use it as the hide-time segment summariser. Phase 5.2 / Stage 2.
	compactorExt := compactorext.NewExtension(
		core.Logger,
		core.Config.Compactor(),
		compactorext.Deps{
			Router:       core.Models,
			Store:        core.Store,
			AgentID:      core.Agent.ID(),
			SkillCatalog: compactorext.NewSkillManagerCatalog(core.Skills),
			Prompts:      renderer,
		},
	)

	// Artifact store (Phase 8): durable, user-facing files under
	// <artifacts.dir>/<agent>/<root_id>/ — the folder IS the registry
	// (no DB). Empty dir falls back to <state>/artifacts. The extension
	// grants list/copy/publish/delete and reaps a root's artifacts on
	// root-session close.
	artifactsDir := core.Cfg.Artifacts.Dir
	if artifactsDir == "" {
		artifactsDir = filepath.Join(core.Cfg.StateDir, "artifacts")
	}
	artifactStore := artifactext.NewStore(artifactsDir, core.Agent.ID(),
		core.Cfg.Artifacts.MaxTotalSize, core.Cfg.Artifacts.MaxSessionSize, core.Logger)
	artifactExt := artifactext.NewExtension(artifactStore, core.Agent.ID(), core.Logger)
	// Stashed on Core so phaseRunner can register the idle reaper over
	// the same store and adapters can reach Ingest / Path (download).
	core.Artifacts = artifactExt

	// Marketplace publisher for skill:publish (SK3): the agent JWT / user token
	// via the "hugr" token store + HUGEN_HUB_URL. Nil when no marketplace is
	// configured — skill:publish then answers a clear "not configured" error.
	var skillPublisher *skillext.Publisher
	if core.Auth != nil {
		if ts, ok := core.Auth.TokenStore("hugr"); ok {
			skillPublisher = skillext.NewPublisher(core.Cfg.Hugr.HubURL, ts)
		}
	}

	exts := []extension.Extension{
		wsext.NewExtension(core.Cfg.Workspace.Dir, core.Logger),
		// Artifact ext registered right after workspace: its InitState
		// reads workspace.FromState for the copy/publish dir, so the
		// workspace handle must exist first. Phase 8.
		artifactExt,
		compactorExt,
		planext.NewExtension(core.Agent.ID()),
		wbext.NewExtension(core.Agent.ID()),
		skillext.NewExtension(core.Skills, core.Permissions, core.Agent.ID()).WithPublisher(skillPublisher),
		// Notepad registered AFTER skillext (B31): both contribute a
		// ModelInTurnAdvisor.TurnPreamble joined in deps.Extensions
		// order, and the deliberate reading order is skill catalogue +
		// recommended-tags → notepad snapshot → user ask (findings get
		// maximal recency, sitting closest to the ask). notepad no
		// longer advertises into the system prefix, and nothing reads
		// its state during InitState, so the later slot is free.
		notepadext.NewExtension(core.Store, core.Agent.ID(), notepadext.Config{}),
		// Recap (db-2 prerequisite): a ROOT-only extension that distils
		// the recent user↔assistant dialogue into a short {topic, text,
		// categories} marker for db-2's dynamic-skill recall (and Phase 7's
		// memory index), and replays its last marker on restart (CategoryOp
		// frame). A synchronous FrameObserver keeps a small ring of recent
		// messages; a TurnBoundaryHook (re)forms the marker via the cheap
		// summariser model SYNCHRONOUSLY before the turn renders, so the
		// topic is current before the skill advertise reads it — at the cost
		// of a small per-turn model call (bounded by BuildTimeout). Config
		// knobs take their defaults; tune via config later if needed.
		recapext.NewExtension(recapext.Deps{
			Router:  core.Models,
			AgentID: core.Agent.ID(),
			Logger:  core.Logger,
		}, recapext.Config{
			// Operator-tunable per-turn fold cap (config `recap.fold_timeout`);
			// 0 → the extension default. Bounds the synchronous turn-start
			// block, so raise it when testing against a slow local model.
			BuildTimeout: core.Config.Recap().FoldTimeout(),
			// Per-message cap (config `recap.max_message_tokens`); 0 → the
			// extension default. Generous so a full delegated task distils
			// into the marker intact.
			MaxMessageTokens: core.Config.Recap().MaxMessageTokens(),
		}),
		// Mission ext owns the entire mission-PDCA dispatch
		// surface — MissionDispatcher (validates spawn_mission's
		// `skill` arg), MissionAutoRunner (drives the executor
		// goroutine), and the "Available missions" prompt block.
		// Catalog is backed by the shared SkillManager so any
		// installed skill declaring metadata.hugen.mission.plan
		// auto-appears as a dispatch target. Mission-PDCA
		// (design 003).
		missionext.NewExtension(missionext.Config{
			AgentID: core.Agent.ID(),
			Logger:  core.Logger,
			Catalog: newSkillManagerMissionCatalog(core.Skills),
		}),
		// stuckdetector observes tool_call / tool_result frames and
		// emits the four §8.3 rising-edge nudges. Phase 5.2.η.4
		// moved it out of `pkg/session/turn_stuck.go` so the session
		// goroutine no longer carries detector state. Registered
		// AFTER skillext so the latter's ToolPolicyAdvisor (the
		// only DisableStuckNudges source today) is visible to the
		// detector's per-evaluation policy gather. Placement before
		// mission ext keeps it adjacent to the other observability
		// extensions; ordering is otherwise free.
		stuckdetectorext.NewExtension(core.Agent.ID(), core.Logger),
		mcpext.NewExtension(core.Config.ToolProviders(), core.Logger),
		// scheduler extension (Phase 6.1b): exposes the schedule:*
		// management surface (create / list / pause / resume / cancel).
		// Full fire dispatch / Runner registration lands in 6.1c.
		// Constructed even when TaskStore is nil so the tool surface
		// stays advertised (Call paths return not_yet_implemented or
		// store_error in that case).
		schedext.NewExtension(core.TaskStore, core.Skills, core.Agent.ID(), core.Logger),
		// task extension (Phase 6.1d): exposes one synthetic
		// `task:<recipe>` tool per task-eligible skill in the
		// manager. Dispatch spawns a leaf subagent under the
		// caller's root for kind=worker recipes; kind=mission is
		// stub-rejected until a future phase wires the mission
		// shape. Bound to the session manager via phaseRunner so
		// the dispatch path can resolve the owner session.
		taskext.NewExtension(core.Skills, core.Agent.ID(), core.Logger),
		// liveview lands last so its FrameObserver / ChildFrameObserver
		// see frames AFTER siblings have processed them via their own
		// Recovery / state mutations. It contributes no tool surface;
		// its ReportStatus iterates the slice above when assembling
		// its emit payload. Phase 5.1b §"Wire-up".
		liveviewext.New(core.Logger),
	}

	// Stage 2 (L3) — the context:* in-turn checkpoint tools live on a
	// standalone provider named "context" (the compactor extension's
	// provider name is "compactor", which would reject context-prefixed
	// tool names). Stateless: every Call recovers the calling session's
	// CompactorState from the dispatch ctx. The compactor extension is
	// passed as the hide-time segment summariser (it owns the model
	// router) so a hide auto-distils the segment into the placeholder
	// brief. Registered alongside the compactor so the checkpoint state
	// owner + its tool surface ship together.
	if err := core.Tools.AddProvider(compactorext.NewContextProvider(compactorExt)); err != nil {
		return fmt.Errorf("register context checkpoint provider: %w", err)
	}

	for _, ext := range exts {
		if p, ok := ext.(tool.ToolProvider); ok {
			if err := core.Tools.AddProvider(p); err != nil {
				return fmt.Errorf("register extension %q as tool provider: %w", ext.Name(), err)
			}
		}
		if cmder, ok := ext.(extension.Commander); ok {
			for _, cmd := range cmder.Commands() {
				if err := core.Commands.Register(cmd.Name, session.CommandSpec{
					Description: cmd.Description,
					Handler:     adaptExtensionCommand(cmd.Handler),
				}); err != nil {
					return fmt.Errorf("register extension %q command %q: %w",
						ext.Name(), cmd.Name, err)
				}
			}
		}
	}

	// Phase 5.2.η — exactly one HistoryOwner per agent. Compactor
	// is the canonical owner; missing or duplicated registration is
	// a configuration error (Session.buildMessages reads through
	// the singular owner each turn). η.3 tightens η.1's "at most
	// one" to "exactly one" now that the legacy s.history slice is
	// gone — there is no fallback.
	var owners []string
	for _, ext := range exts {
		if _, ok := ext.(extension.HistoryOwner); ok {
			owners = append(owners, ext.Name())
		}
	}
	switch len(owners) {
	case 0:
		return fmt.Errorf("phase 5.2.η: no HistoryOwner extension registered; runtime cannot build model prompts")
	case 1:
		// expected
	default:
		return fmt.Errorf("phase 5.2.η: multiple HistoryOwner extensions registered: %v", owners)
	}

	core.Extensions = exts

	// Register a cleanup that walks every Shutdowner-implementing
	// extension at process shutdown (after Manager.Stop has
	// drained sessions, before pkg/runtime closes the local
	// store). Reverse registration order so dependencies that
	// registered earlier survive longer.
	core.addCleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for i := len(exts) - 1; i >= 0; i-- {
			s, ok := exts[i].(extension.Shutdowner)
			if !ok {
				continue
			}
			if err := s.Shutdown(ctx); err != nil {
				core.Logger.Warn("extension shutdown failed",
					"extension", exts[i].Name(), "err", err)
			}
		}
	})
	return nil
}

// adaptExtensionCommand bridges an [extension.CommandFn] (which sees
// only [extension.SessionState] + [extension.CommandContext]) to the
// session-level [session.CommandHandler] signature the registry
// expects. The wrapping is a single closure per command — no
// per-call allocation beyond the CommandContext literal.
func adaptExtensionCommand(fn extension.CommandFn) session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		return fn(ctx, env.Session, extension.CommandContext{
			Author:      env.Author,
			AgentAuthor: env.AgentAuthor,
		}, args)
	}
}
