package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/skill"

	"github.com/hugr-lab/query-engine/types"
)

// BuildSkillStack constructs the SkillStore + SkillManager from the
// installed bundled skills. system tier reads from
// ${stateDir}/skills/system/, local from ${stateDir}/skills/local/.
// CommunityRoot is left empty for now — operator-pinned community
// roots are a config-time extension that lands later.
func BuildSkillStack(stateDir string, log *slog.Logger) (*skill.SkillManager, skill.SkillStore, error) {
	if stateDir == "" {
		return nil, nil, fmt.Errorf("buildSkillStack: empty state dir")
	}
	store := skill.NewSkillStore(skill.Options{
		SystemRoot: filepath.Join(stateDir, "skills/system"),
		LocalRoot:  filepath.Join(stateDir, "skills/local"),
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
