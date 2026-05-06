package session

import (
	"context"
	"strings"

	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Phase 4.1a stage A step 8 — per-session snapshot cache that
// owns the responsibilities the legacy pkg/tool.ToolManager.cache
// + .skills field used to handle:
//
//   1. Read the unfiltered union catalogue from
//      ToolManager.Snapshot (Manager no longer filters).
//   2. Apply the per-session skill-bindings allow-list (exact
//      names + `provider:prefix*` glob patterns).
//   3. Cache the filtered result keyed by the (toolGen, skillGen,
//      policyGen) triple so the next call within the same
//      generation triple returns the cached value.
//
// One cache instance per Session — Session holds it as a value
// field. The cache rebuilds whenever any generation moves;
// expected hit rate inside a turn is ~100% (no provider /
// skill / policy mutation between modelToolsForSession and
// dispatchToolCall).

// snapshotCache is the per-Session cached, filtered tool
// catalogue. Zero value is empty + invalid; first get rebuilds.
type snapshotCache struct {
	gens  tool.Generations
	snap  tool.Snapshot
	valid bool
}

// fetchSnapshot returns the filtered Snapshot for the session.
// Honours the (toolGen, skillGen, policyGen) cache key — any
// generation move triggers a rebuild. Returns the unfiltered
// catalogue when skills is nil (test setups; no-skill deployments
// surface every registered tool).
func (s *Session) fetchSnapshot(ctx context.Context) (tool.Snapshot, error) {
	if s.tools == nil {
		return tool.Snapshot{}, nil
	}
	gens, err := s.snapshotGenerations(ctx)
	if err != nil {
		return tool.Snapshot{}, err
	}
	s.snapMu.Lock()
	if s.snapCache.valid && s.snapCache.gens == gens {
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
	if s.skills != nil {
		filtered = applySkillFilter(ctx, s.skills, s.id, raw.Tools)
	}
	out := tool.Snapshot{Generations: gens, Tools: filtered}

	s.snapMu.Lock()
	s.snapCache = snapshotCache{gens: gens, snap: out, valid: true}
	s.snapMu.Unlock()

	return out, nil
}

// snapshotGenerations returns the (Tool, Skill, Policy) triple
// the cache key uses. Skill comes from the SkillManager bindings;
// Tool / Policy come from ToolManager. nil skills → Skill=0.
func (s *Session) snapshotGenerations(ctx context.Context) (tool.Generations, error) {
	gens := tool.Generations{
		Tool:   s.tools.ToolGen(),
		Policy: s.tools.PolicyGen(),
	}
	if s.skills != nil {
		b, err := s.skills.Bindings(ctx, s.id)
		if err != nil {
			return gens, err
		}
		gens.Skill = b.Generation
	}
	return gens, nil
}

// allowedSet is the per-session compiled allow-list. Holds both
// exact tool names and `provider:prefix*` glob patterns so a skill
// granting `discovery-*` against the `hugr-main` provider matches
// every `hugr-main:discovery-<anything>` tool.
//
// nil ⇒ no filter (used when skills is nil — tests / deployments
// without skill management). Empty (non-nil) ⇒ no skills loaded
// → empty catalogue.
type allowedSet struct {
	exact    map[string]bool
	patterns []string // each is "provider:prefix" with the trailing * stripped
}

// match reports whether the fully-qualified tool name (e.g.
// "hugr-main:discovery-search_data_sources") is allowed by any
// rule in the set.
func (a *allowedSet) match(name string) bool {
	if a == nil {
		return true
	}
	if a.exact[name] {
		return true
	}
	for _, p := range a.patterns {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// allowedFromBindings compiles the loaded-skill bindings into an
// allowedSet. Returns:
//   - nil when skills is nil (no filter wired).
//   - empty (non-nil) when skills is wired but no skill is loaded
//     for the session.
//   - populated when allowed-tools entries exist.
func allowedFromBindings(ctx context.Context, skills *skill.SkillManager, sessionID string) *allowedSet {
	if skills == nil {
		return nil
	}
	b, err := skills.Bindings(ctx, sessionID)
	if err != nil || len(b.AllowedTools) == 0 {
		return &allowedSet{exact: map[string]bool{}}
	}
	out := &allowedSet{exact: map[string]bool{}}
	for _, g := range b.AllowedTools {
		for _, t := range g.Tools {
			full := g.Provider + ":" + t
			if strings.HasSuffix(t, "*") {
				out.patterns = append(out.patterns, strings.TrimSuffix(full, "*"))
				continue
			}
			out.exact[full] = true
		}
	}
	return out
}

// applySkillFilter returns the subset of in whose names match the
// loaded-skill allowed-tools bindings for sessionID. Pure helper
// — no caching, no manager state.
func applySkillFilter(ctx context.Context, skills *skill.SkillManager, sessionID string, in []tool.Tool) []tool.Tool {
	allowed := allowedFromBindings(ctx, skills, sessionID)
	if allowed == nil {
		return in
	}
	out := make([]tool.Tool, 0, len(in))
	for _, t := range in {
		if allowed.match(t.Name) {
			out = append(out, t)
		}
	}
	return out
}
