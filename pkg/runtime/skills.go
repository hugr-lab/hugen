package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/skill"

	"github.com/hugr-lab/query-engine/types"
)

// SystemSkillsFS returns the embed-backed fs.FS scoped to the
// agent-core skills (`_root`, `_mission`, …). Callers wire it
// into skill.Options.SystemFS. Useful from tests / cmd binaries
// that build a SkillStore directly without going through
// [BuildSkillStack].
//
// Panics if the embed sub-extraction fails: the embed root is a
// compile-time constant, so a runtime failure means the binary
// was built without the `assets/system/` tree and booting it with
// a silent nil would leave the agent running with no `_root` /
// `_mission` / `_worker` autoload — catastrophic and silent.
// Fail loud at boot instead.
func SystemSkillsFS() fs.FS {
	sub, err := fs.Sub(assets.SystemSkillsFS, "system")
	if err != nil {
		panic(fmt.Sprintf("runtime: scope assets.SystemSkillsFS: %v", err))
	}
	return sub
}

// BuildSkillStack constructs the SkillStore + SkillManager over
// the three-tier production layout:
//
//   - **system** — embed-only from assets.SystemSkillsFS. Agent
//     core (`_root`, `_mission`, …); never on disk.
//   - **hub**    — `${stateDir}/skills/hub/`. Admin-delivered
//     extensions installed at boot from assets.SkillsFS (the
//     binary-bundled defaults). The on-disk path stays the
//     SkillStore's read window even after the future Hugr-hub
//     sync replaces the install step.
//   - **local**  — `${stateDir}/skills/local/`. Operator skills,
//     writable via skill:save. Phase 6.2.db: when a querier +
//     agentID are supplied this root becomes the DB-indexed
//     dynamic backend (dir + `skills` index); without them it stays
//     the plain writable dirBackend.
func BuildSkillStack(stateDir string, log *slog.Logger, opts DynamicSkillOpts) (*skill.SkillManager, skill.SkillStore, error) {
	if stateDir == "" {
		return nil, nil, fmt.Errorf("buildSkillStack: empty state dir")
	}
	store := skill.NewSkillStore(skill.Options{
		SystemFS:        SystemSkillsFS(),
		HubRoot:         filepath.Join(stateDir, "skills/hub"),
		LocalRoot:       filepath.Join(stateDir, "skills/local"),
		DynamicQuerier:  opts.Querier,
		AgentID:         opts.AgentID,
		EmbedderEnabled: opts.EmbedderEnabled,
	})
	mgr := skill.NewSkillManager(store, log)
	return mgr, store, nil
}

// DynamicSkillOpts carries the Phase-6.2.db wiring for the dynamic
// (DB-indexed) skill backend. Zero value (nil Querier) keeps the
// plain local dirBackend — used by tests / no-engine boots.
type DynamicSkillOpts struct {
	Querier         types.Querier
	AgentID         string
	EmbedderEnabled bool
}

// pickQuerier returns the live querier the skill index should talk to:
// the embedded local engine when present, else the remote hub. Mirrors
// ChooseStore / the TaskStore wiring so all three stores stay coherent.
func pickQuerier(core *Core) types.Querier {
	if core.LocalQuerier != nil {
		return core.LocalQuerier
	}
	if core.RemoteQuerier != nil {
		return core.RemoteQuerier
	}
	return nil
}

// BuildPermissionService constructs the perm.Service used by the
// ToolManager and consulted by every tool dispatch.
//
// The selector picks Tier-2-aware RemotePermissions when:
//
//   - the deployment opts in via view.RemoteEnabled(); and
//   - a hugr token store is registered (authHasHugr); and
//   - some types.Querier is available to run
//     function.core.auth.my_permissions against. The remote querier
//     is preferred (the Hugr hub is the source of truth for role
//     rules); the local engine falls back when the deployment
//     bundles its own engine.
//
// Otherwise LocalPermissions stays as the Tier-1-only floor.
func BuildPermissionService(
	view perm.PermissionsView,
	idSrc identity.Source,
	authHasHugr bool,
	remoteQ, localQ types.Querier,
	log *slog.Logger,
) perm.Service {
	local := perm.NewLocalPermissions(view, idSrc)
	if !view.RemoteEnabled() {
		return local
	}
	if !authHasHugr {
		return local
	}
	var q types.Querier
	if remoteQ != nil {
		q = remoteQ
	} else if localQ != nil {
		q = localQ
	}
	if q == nil {
		return local
	}
	log.Info("permissions: remote tier enabled (function.core.auth.my_permissions)")
	return perm.NewRemotePermissions(view, idSrc, perm.NewRemoteQuerier(q))
}

// phaseSkillsAndPerms runs phase 7: builds the SkillManager +
// SkillStore from the installed-skills tree and assembles the
// permission service. Populates Core.Skills, Core.SkillStore,
// Core.Permissions.
//
// The /skill slash command (list / load / unload) lives on the
// skill extension's [extension.Commander] surface; phaseExtensions
// registers it onto Core.Commands later in the boot sequence.
func phaseSkillsAndPerms(ctx context.Context, core *Core) error {
	// Phase 6.2.db: thread the live querier + agent scope into the
	// dynamic backend so skill:save lands a DB index row alongside
	// the bundle. Local engine preferred over remote (mirrors
	// ChooseStore). Without a querier the stack falls back to the
	// plain local dirBackend.
	var dynOpts DynamicSkillOpts
	if q := pickQuerier(core); q != nil && core.Agent != nil {
		embed := core.Config.Embedding().EmbeddingConfig()
		dynOpts = DynamicSkillOpts{
			Querier:         q,
			AgentID:         core.Agent.ID(),
			EmbedderEnabled: embed.Mode != "" && embed.Model != "",
		}
	}
	skills, store, err := BuildSkillStack(core.Cfg.StateDir, core.Logger, dynOpts)
	if err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	core.Skills = skills
	core.SkillStore = store

	// Phase 6.2.db: sync the dynamic index at boot — install the hub
	// bundles per the config install-set, then reconcile the writable
	// (authored) dir + relink catalogs. Per-source dirs (hub / local)
	// stay on disk; only the `skills` index is unified. Re-run on a
	// config change via the SkillsView OnUpdate subscription (today a
	// no-op on the static service; wired for the future dynamic config
	// service). No-op without a dynamic backend.
	if ds, ok := store.(*skill.Store); ok && ds.HasDynamic() {
		hubDir := filepath.Join(core.Cfg.StateDir, "skills/hub")
		sync := func() {
			view := core.Config.Skills()
			n, rerr := ds.SyncDynamic(ctx, hubDir, view.InstallSet(), view.InstallSetDeclared())
			if rerr != nil {
				core.Logger.Warn("skills: dynamic sync had errors", "indexed", n, "err", rerr)
			} else {
				core.Logger.Info("skills: dynamic index synced", "bundles", n)
			}
		}
		sync()
		cancel := core.Config.Skills().OnUpdate(sync)
		core.addCleanup(func() { cancel() })
	}

	authHasHugr := false
	if core.Auth != nil {
		_, authHasHugr = core.Auth.TokenStore("hugr")
	}
	core.Permissions = BuildPermissionService(
		core.Config.Permissions(),
		core.Identity,
		authHasHugr,
		core.RemoteQuerier,
		core.LocalQuerier,
		core.Logger,
	)
	return nil
}
