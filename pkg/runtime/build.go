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

	_ = ctx

	if err := phaseBundledSkills(core); err != nil {
		return nil, failed("bundled_skills", err)
	}

	// Remaining phases (http_auth → identity → storage → models →
	// agent → skills_perms → tools → session_manager) land in
	// steps 12-26.

	return core, nil
}
