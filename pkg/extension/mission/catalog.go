package mission

import "context"

// Catalog is the narrow interface mission ext uses to inspect the
// skill catalogue without importing pkg/skill directly. Production
// wiring (pkg/runtime) supplies an adapter wrapping the real
// SkillManager; tests pass an in-memory stub.
//
// The mission-PDCA shape is recognised by the presence of
// `metadata.hugen.mission.plan.inline.waves` (deterministic
// pipeline) or `plan.role` (LLM-driven planner) in the skill's
// manifest. Skills whose manifests carry neither aren't mission-
// eligible — MissionSkillExists returns false and spawn_mission
// rejects them.
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

	// Research declares the optional pre-planner research stage.
	// Phase 5.x — B15. Nil means "no research stage". When set,
	// the runtime spawns Role before the planner loop, lets it
	// gather user clarifications + scope findings, and surfaces
	// those into the planner's plan_context.
	Research *ResearchManifest

	// Plan declares how the mission's wave sequence is sourced —
	// either Inline waves (deterministic pipeline) or a planner
	// Role (LLM-driven).
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

	// Control names the verdict-emitting role spawned after each
	// non-planner wave. Empty when the manifest declares no
	// control — the planner loop falls back to the implicit
	// `continue` routing. Phase C.
	Control ControlManifest

	// Capabilities declares the mission-tier capability toggles.
	// Phase F (design 003) — declarative schema only; current
	// runtime keeps notepad / whiteboard / plan_context always
	// available on mission sessions.
	Capabilities MissionCapabilities

	// Roles maps role-name → typed capabilities the mission ext
	// honours at worker-spawn time. Populated by the runtime
	// catalog adapter from the skill's `sub_agents` block. Empty
	// for roles that declare no Capabilities; resolution falls
	// through to the role-class default.
	Roles map[string]RoleCapabilities

	// AcceptanceCriteria is the typed projection of the manifest's
	// iter-0 AC seed. Each entry is the raw template string from the
	// skill manifest; the runtime renders it with `.Inputs` at
	// mission spawn time and calls SeedAC(..., OriginManifest).
	//
	// Empty / nil → no manifest seed. Phase 5.x — B11 §3.2.2.
	AcceptanceCriteria []string
}

// MissionCapabilities is the mission-tier capability projection.
// Each pointer-bool field preserves "unset → defer to runtime
// default"; an explicit `false` is a deliberate opt-out. Phase F.
type MissionCapabilities struct {
	Notepad     *bool
	Whiteboard  *bool
	PlanContext *bool
}

// RoleCapabilities is the per-role capability projection consumed
// by the executor's worker-spawn path. v1 surface is narrow —
// PlanContextAccess gates the [Plan context] section injection.
// Future fields (notepad read/write, refs read/write) land here.
// Phase F.
type RoleCapabilities struct {
	// PlanContextAccess: "off" | "read". Empty inherits the
	// role-class default at resolution time.
	PlanContextAccess string
}

// PlanContext access modes.
const (
	PlanContextOff  = "off"
	PlanContextRead = "read"
)

// ControlManifest names the checker role for the verdict phase.
// Empty Role means "no checker spawned"; the loop auto-continues.
type ControlManifest struct {
	Role string
}

// ResearchManifest is the typed projection of the skill manifest's
// `metadata.hugen.mission.research` block. Phase 5.x — B15.
//
// The runtime auto-runner spawns Role before the planner loop on
// missions where the trigger (When + optional Predicate) fires.
// Output is parsed via DecodeResearchOutput; on `done: true` the
// runtime stamps ResearchFindings + ResolvedUserInputs +
// ACProposals on MissionState and moves to the planner spawn.
type ResearchManifest struct {
	// Role names the research sub-agent role declared in the
	// skill's `sub_agents` block. Required when the block is
	// present.
	Role string

	// When selects the trigger predicate. Canonical values:
	// `always`, `auto`, `if_goal_matches`. Empty defaults to
	// `auto` at projection time.
	When string

	// Predicate is the goal-string regex evaluated when
	// When=`if_goal_matches`. Empty for other trigger modes.
	Predicate string

	// MaxIterations caps research re-fire cycles when the role
	// emits `done: false`. Defaults to ResearchDefaultMaxIterations
	// at projection time.
	MaxIterations int
}

// Recognised values for ResearchManifest.When. Mirror the skill
// manifest constants so the runtime side can keep a closed
// vocabulary.
const (
	ResearchWhenAlways        = "always"
	ResearchWhenAuto          = "auto"
	ResearchWhenIfGoalMatches = "if_goal_matches"

	// ResearchDefaultMaxIterations matches spec §2.1's default.
	ResearchDefaultMaxIterations = 3

	// ResearchMaxIterationsCap is the operator-visible hard ceiling
	// to keep a weak model from looping forever on a malformed
	// clarification batch.
	ResearchMaxIterationsCap = 6
)

// MissionPlanManifest is the typed plan section of a PDCA mission.
// Either Inline or Role is non-zero for a mission-eligible skill;
// the runtime picks the dispatch path off whichever field is
// populated. The two shapes never co-exist on a single manifest —
// the runtime catalog projection rejects ambiguous manifests at
// load.
type MissionPlanManifest struct {
	// Inline declares the wave sequence directly in the manifest
	// (deterministic pipeline — fixtures + task skills). Nil when
	// the manifest declares a planner role instead. Previously
	// named ExperimentalInline; renamed at Phase 6.0 when it
	// became load-bearing for task skills.
	Inline *InlinePlan

	// Role names the planner sub-agent role from the skill's
	// `sub_agents` block. Empty when the manifest uses the inline
	// path. Phase B.
	Role string

	// Approval is the typed approval policy for planner spawns.
	// Defaults applied at projection time so consumers can read
	// the policy without re-normalising.
	Approval PlanApproval

	// MaxWaves caps how many planner-driven iterations run before
	// the runtime forces synthesis. Zero leaves the consumer to
	// apply its own default; the runtime projection fills the
	// canonical default (10).
	MaxWaves int
}

// PlanApproval is the typed projection of MissionPlanApproval
// (skill manifest). Initial / Iteration are normalised against
// the v1 enums at projection time — consumers can rely on these
// strings being one of the canonical values.
type PlanApproval struct {
	// Initial — "required" | "skip". Default "required".
	Initial string
	// Iteration — "always" | "never" | "initial-only". Default
	// "initial-only".
	Iteration string
}

// Canonical approval values. Constants kept narrow — the v1 enum
// surface is tight per spec § 0.4 / Phase B; Phase I broadens it.
const (
	ApprovalInitialRequired   = "required"
	ApprovalInitialSkip       = "skip"
	ApprovalIterationAlways   = "always"
	ApprovalIterationNever    = "never"
	ApprovalIterationInitOnly = "initial-only"
	DefaultMaxWaves           = 10
	MaxMaxWaves               = 50
)

// NormalizePlanApproval fills empty fields with their spec
// defaults. Unknown values pass through unchanged so the executor
// can fail-loud at first use — the runtime projection rejects
// unknown values up front; this helper is a safety net for
// in-memory test fixtures that bypass projection.
func NormalizePlanApproval(p PlanApproval) PlanApproval {
	if p.Initial == "" {
		p.Initial = ApprovalInitialRequired
	}
	if p.Iteration == "" {
		p.Iteration = ApprovalIterationInitOnly
	}
	return p
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
// only; Phase B adds OutputContract for kind validation. Description
// carries the role's `sub_agents[].description` so the planner can
// render the catalogue of valid Do-roles into its first message and
// pick a real role instead of guessing a generic "worker" name.
type WorkerManifest struct {
	Role        string
	Description string
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
