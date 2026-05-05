package runtime

import (
	"context"
	"fmt"
)

// Build assembles a Core from a fully resolved Config by running
// the 9 named phases in order. On any phase error it unwinds every
// resource acquired so far via Core.cleanupPartial and returns the
// error wrapped with the failing phase name. On success the caller
// MUST defer Core.Shutdown(ctx).
//
// Phase wiring lands incrementally during phase 4.1a Stage B-E. The
// skeleton commit (step 10) returns an empty Core after Validate;
// subsequent commits insert phaseBundledSkills, phaseHTTPAuth,
// phaseIdentity, phaseStorage, phaseModels, phaseAgent,
// phaseSkillsAndPerms, phaseTools, phaseSessionManager in that
// order. See design/001-agent-runtime/phase-4.1a-spec.md §5.
func Build(ctx context.Context, cfg Config) (*Core, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	core := &Core{Cfg: cfg, Logger: cfg.Logger}

	failed := func(step string, err error) error {
		core.cleanupPartial()
		return fmt.Errorf("runtime: %s: %w", step, err)
	}

	if err := phaseBundledSkills(core); err != nil {
		return nil, failed("bundled_skills", err)
	}
	if err := phaseHTTPAuth(ctx, core); err != nil {
		return nil, failed("http_auth", err)
	}
	if err := phaseIdentity(ctx, core); err != nil {
		return nil, failed("identity", err)
	}
	if err := phaseStorage(ctx, core); err != nil {
		return nil, failed("storage", err)
	}
	if err := phaseModels(ctx, core); err != nil {
		return nil, failed("models", err)
	}
	if err := phaseAgent(ctx, core); err != nil {
		return nil, failed("agent", err)
	}
	if err := phaseSkillsAndPerms(ctx, core); err != nil {
		return nil, failed("skills_perms", err)
	}
	if err := phaseTools(ctx, core); err != nil {
		return nil, failed("tools", err)
	}

	// Remaining phase (session_manager) lands in step 26.

	return core, nil
}
