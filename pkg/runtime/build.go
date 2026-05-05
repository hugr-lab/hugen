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
	_ = failed

	// Phase calls land in steps 11-15 (Stage B) and 16-26 (Stage
	// C-E). Each phase reads fields populated by prior phases.

	return core, nil
}
