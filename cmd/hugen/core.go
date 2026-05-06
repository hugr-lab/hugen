package main

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/runtime"
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
//
// Builtin slash commands (/help, /note, /cancel, /end, /model) are
// registered by phase 6, /skill by phase 7 — cmd/hugen owns no
// command registrations of its own.
//
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
	return core, boot, nil
}
