package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Per-session snapshot cache. Reads the unfiltered catalogue from
// ToolManager once per (toolGen, policyGen, extGen) triple and
// runs the registered ToolFilter chain over it. extGen is the sum
// of Generation(state) over every extension that implements
// [extension.GenerationProvider] — bumps invalidate the cache the
// same way a tool/policy gen bump does, without any cross-package
// plumbing.
//
// Phase 4.1b-pre stage 2 dropped the in-package skill filter and
// the [tool.Generations.Skill] field; the skill extension's
// FilterTools + Generation provide both via the generic capability
// chain.

// snapshotCache is the per-Session cached, filtered tool
// catalogue. Zero value is empty + invalid; first get rebuilds.
type snapshotCache struct {
	gens   tool.Generations
	extGen int64
	snap   tool.Snapshot
	valid  bool
}

// fetchSnapshot returns the filtered Snapshot for the session.
// Honours the (toolGen, policyGen, extGen) cache key — any
// generation move triggers a rebuild.
func (s *Session) fetchSnapshot(ctx context.Context) (tool.Snapshot, error) {
	if s.tools == nil {
		return tool.Snapshot{}, nil
	}
	gens := tool.Generations{
		Tool:   s.tools.ToolGen(),
		Policy: s.tools.PolicyGen(),
	}
	extGen := s.extensionGenerationSum(ctx)
	s.snapMu.Lock()
	if s.snapCache.valid && s.snapCache.gens == gens && s.snapCache.extGen == extGen {
		out := s.snapCache.snap
		s.snapMu.Unlock()
		return out, nil
	}
	s.snapMu.Unlock()

	raw, err := s.tools.Snapshot(ctx, s.id)
	if err != nil {
		return tool.Snapshot{}, err
	}
	filtered := raw.Tools
	// Extension ToolFilter chain composes by intersection — each
	// filter sees the prior result and may only narrow it further.
	// Order is registration order; an extension that returns the
	// same slice it was given is a no-op.
	if s.deps != nil {
		for _, ext := range s.deps.Extensions {
			tf, ok := ext.(extension.ToolFilter)
			if !ok {
				continue
			}
			filtered = tf.FilterTools(ctx, s, filtered)
		}
	}
	out := tool.Snapshot{Generations: gens, Tools: filtered}

	s.snapMu.Lock()
	s.snapCache = snapshotCache{gens: gens, extGen: extGen, snap: out, valid: true}
	s.snapMu.Unlock()

	return out, nil
}

// extensionGenerationSum returns the running sum of
// Generation(state) over every extension that implements
// [extension.GenerationProvider]. Cheap (one method call per
// implementing extension); folded into the snapshot cache key so a
// bump invalidates without any cross-package plumbing.
func (s *Session) extensionGenerationSum(_ context.Context) int64 {
	if s.deps == nil {
		return 0
	}
	var sum int64
	for _, ext := range s.deps.Extensions {
		if g, ok := ext.(extension.GenerationProvider); ok {
			sum += g.Generation(s)
		}
	}
	return sum
}
