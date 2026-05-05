package main

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session"
)

// bootRuntime brings up the agent runtime for cmd/hugen.
//
// The function is a thin shim over pkg/runtime.Build:
//
//  1. loadBootstrapConfig reads .env / process env into a
//     BootstrapConfig.
//  2. projectRuntimeConfig turns it into the env-pure runtime.Config
//     Build consumes — Build never touches os.Environ.
//  3. runtime.Build runs the 9 named phases and returns *runtime.Core.
//  4. The /skill subcommand handler is registered on Core.Commands.
//     Skill loading needs Core.Skills + Core.SkillStore + Core.Permissions
//     (built by phase 7) and Core.Commands (built by phase 6) — so
//     registration must happen after Build returns. Builtin commands
//     (/help, /note, /cancel, /end, /model) are registered by phase 6
//     itself; /skill is the only command cmd/hugen owns.
//
// On any error the Core (if non-nil) is shut down before returning.
// On success the caller MUST defer core.Shutdown(ctx).
func bootRuntime(ctx context.Context) (*runtime.Core, *BootstrapConfig, error) {
	boot, err := loadBootstrapConfig(".env")
	if err != nil {
		return nil, nil, fmt.Errorf("bootstrap: %w", err)
	}
	logger := newLogger(boot.LogLevel)
	logger.Info("starting hugen", "info", boot.Info())

	cfg := projectRuntimeConfig(boot, logger)
	core, err := runtime.Build(ctx, cfg)
	if err != nil {
		return nil, boot, err
	}

	if err := core.Commands.Register("skill", session.CommandSpec{
		Handler:     skillCommandHandler(core.Skills, core.SkillStore, core.Permissions),
		Description: "list, load or unload skills: /skill list | /skill load <name> | /skill unload <name>",
	}); err != nil {
		core.Shutdown(ctx)
		return nil, boot, fmt.Errorf("register /skill: %w", err)
	}

	return core, boot, nil
}
