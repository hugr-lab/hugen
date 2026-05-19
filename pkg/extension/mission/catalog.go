package mission

import "context"

// Catalog is the narrow interface mission ext uses to inspect the
// skill catalogue without importing pkg/skill directly. Production
// wiring (pkg/runtime) supplies an adapter wrapping the real
// SkillManager; tests pass an in-memory stub.
//
// The mission-PDCA Phase-A shape is recognised by the presence of
// `metadata.hugen.mission.plan.experimental_inline.waves` in the
// skill's manifest. Phase B adds `plan.role` for LLM planners.
// Skills whose manifests carry neither aren't mission-eligible —
// MissionSkillExists returns false and spawn_mission rejects them.
type Catalog interface {
	// LookupMission returns the typed mission manifest projection
	// for the named skill, or (nil, nil) when the skill exists but
	// is not a PDCA mission, or (nil, error) on lookup failure.
	LookupMission(ctx context.Context, name string) (*MissionManifest, error)

	// ListMissions returns every mission-eligible skill the
	// catalogue knows about. Used to render the "Available
	// missions" prompt section the root chat reads.
	ListMissions(ctx context.Context) ([]MissionCatalogEntry, error)
}

// MissionManifest is mission ext's typed projection of the
// PDCA-relevant fields from a skill's `metadata.hugen.mission`
// subtree. Decoupled from pkg/skill's MissionBlock so the runtime
// wiring can map either typed (Phase A — read from
// skill.MissionBlock) or freeform (future) sources into this
// canonical shape.
type MissionManifest struct {
	// Name mirrors the skill name. Required.
	Name string

	// Summary is the one-line description shown to root in the
	// Available missions prompt. Empty falls back to the skill's
	// top-level description in adapters.
	Summary string

	// Plan declares how the mission's wave sequence is sourced.
	// Phase A — only ExperimentalInline is recognised.
	Plan MissionPlanManifest

	// Synthesis names the role that produces the mission's final
	// answer. Empty means "no synthesis step" — the mission
	// terminates with the last wave's primary handoff as result.
	Synthesis SynthesisManifest

	// Workers declares the role catalogue available to the
	// executor. Phase A — minimal shape (role name only). Each
	// worker may declare its own output_contract / capabilities;
	// later phases extend.
	Workers []WorkerManifest
}

// MissionPlanManifest is the typed plan section of a PDCA mission.
// In Phase A only the inline shape is supported.
type MissionPlanManifest struct {
	// ExperimentalInline is the Phase-A escape hatch: the skill
	// author lists waves directly. Nil when the manifest declares
	// a planner role instead (Phase B).
	ExperimentalInline *InlinePlan
}

// InlinePlan carries the fixed-wave sequence for a Phase-A
// mission. Mirrors the in-flight Wave AST consumed by the
// executor.
type InlinePlan struct {
	Waves []Wave
}

// SynthesisManifest names the role for the final synthesis step.
type SynthesisManifest struct {
	Role string
}

// WorkerManifest is the per-role catalogue entry. Phase A — name
// only; Phase B adds OutputContract for kind validation.
type WorkerManifest struct {
	Role string
}

// MissionCatalogEntry is the row a [Catalog.ListMissions] caller
// reads. Carries enough to render the Available missions prompt
// section without re-fetching every full manifest.
type MissionCatalogEntry struct {
	Name    string
	Summary string
}

// staticCatalog is an in-memory Catalog implementation tests +
// fixtures use. Production wiring supplies its own (pkg/runtime
// adapter over the SkillManager); the staticCatalog stays for
// scenarios that pre-register their fixture skill before the
// mission ext is constructed.
type staticCatalog struct {
	missions map[string]*MissionManifest
}

// NewStaticCatalog returns a Catalog backed by an in-memory map.
// Mission ext's Phase-A fixture wiring uses this; production
// adapters in pkg/runtime supply their own.
func NewStaticCatalog(missions ...*MissionManifest) Catalog {
	c := &staticCatalog{missions: make(map[string]*MissionManifest, len(missions))}
	for _, m := range missions {
		if m == nil || m.Name == "" {
			continue
		}
		c.missions[m.Name] = m
	}
	return c
}

func (c *staticCatalog) LookupMission(_ context.Context, name string) (*MissionManifest, error) {
	if c == nil {
		return nil, nil
	}
	m, ok := c.missions[name]
	if !ok {
		return nil, nil
	}
	return m, nil
}

func (c *staticCatalog) ListMissions(_ context.Context) ([]MissionCatalogEntry, error) {
	if c == nil || len(c.missions) == 0 {
		return nil, nil
	}
	out := make([]MissionCatalogEntry, 0, len(c.missions))
	for _, m := range c.missions {
		out = append(out, MissionCatalogEntry{Name: m.Name, Summary: m.Summary})
	}
	return out, nil
}
