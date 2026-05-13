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
//     writable via skill:save.
func BuildSkillStack(stateDir string, log *slog.Logger) (*skill.SkillManager, skill.SkillStore, error) {
	if stateDir == "" {
		return nil, nil, fmt.Errorf("buildSkillStack: empty state dir")
	}
	store := skill.NewSkillStore(skill.Options{
		SystemFS:  SystemSkillsFS(),
		HubRoot:   filepath.Join(stateDir, "skills/hub"),
		LocalRoot: filepath.Join(stateDir, "skills/local"),
	})
	mgr := skill.NewSkillManager(store, log)
	return mgr, store, nil
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
func phaseSkillsAndPerms(_ context.Context, core *Core) error {
	skills, store, err := BuildSkillStack(core.Cfg.StateDir, core.Logger)
	if err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	core.Skills = skills
	core.SkillStore = store

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
