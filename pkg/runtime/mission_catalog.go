package runtime

import (
	"context"
	"strings"

	missionext "github.com/hugr-lab/hugen/pkg/extension/mission"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// skillManagerMissionCatalog adapts a *skill.SkillManager to the
// missionext.Catalog interface. mission ext consumes a narrow
// "give me the typed PDCA shape for a skill" surface so it doesn't
// import pkg/skill itself; this adapter is the production bridge
// owned by pkg/runtime where both packages already converge.
//
// Mission-PDCA (design 003) recognises any installed skill whose
// manifest carries `metadata.hugen.mission.plan.*`. Today only the
// experimental_inline shape is exercised (Phase A); the adapter
// projects the typed pkg/skill.MissionPlanInline value verbatim
// into the in-mission shape mission ext consumes.
type skillManagerMissionCatalog struct {
	manager *skillpkg.SkillManager
}

func newSkillManagerMissionCatalog(m *skillpkg.SkillManager) missionext.Catalog {
	return &skillManagerMissionCatalog{manager: m}
}

func (c *skillManagerMissionCatalog) LookupMission(ctx context.Context, name string) (*missionext.MissionManifest, error) {
	if c == nil || c.manager == nil || name == "" {
		return nil, nil
	}
	all, err := c.manager.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, sk := range all {
		if sk.Manifest.Name != name {
			continue
		}
		return projectMissionManifest(sk.Manifest), nil
	}
	return nil, nil
}

func (c *skillManagerMissionCatalog) ListMissions(ctx context.Context) ([]missionext.MissionCatalogEntry, error) {
	if c == nil || c.manager == nil {
		return nil, nil
	}
	all, err := c.manager.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]missionext.MissionCatalogEntry, 0)
	for _, sk := range all {
		if !isPdcaMission(sk.Manifest) {
			continue
		}
		summary := sk.Manifest.Hugen.Mission.Summary
		if summary == "" {
			summary = sk.Manifest.Description
		}
		out = append(out, missionext.MissionCatalogEntry{
			Name:    sk.Manifest.Name,
			Summary: summary,
		})
	}
	return out, nil
}

// isPdcaMission returns true when the skill carries a mission.plan
// section recognised by mission ext. Phase A — inline waves; Phase
// B — `plan.role` (LLM planner). Either is sufficient; the runtime
// dispatch path picks the active mode at run time.
func isPdcaMission(m skillpkg.Manifest) bool {
	plan := m.Hugen.Mission.Plan
	if plan.ExperimentalInline != nil && len(plan.ExperimentalInline.Waves) > 0 {
		return true
	}
	if plan.Role != "" {
		return true
	}
	return false
}

// projectMissionManifest converts a skill.Manifest's mission
// section into mission ext's typed shape. Returns nil when the
// skill is not a PDCA mission so the caller short-circuits
// without filling a partial struct.
//
// Approval defaults follow spec § Phase B (Initial=required,
// Iteration=initial-only); MaxWaves defaults to
// missionext.DefaultMaxWaves when the manifest leaves it zero.
// Both inline AND role-driven manifests carry these defaults so a
// future v2 that lets inline plans request approval doesn't have
// to special-case projection.
func projectMissionManifest(m skillpkg.Manifest) *missionext.MissionManifest {
	if !isPdcaMission(m) {
		return nil
	}
	mb := m.Hugen.Mission
	out := &missionext.MissionManifest{
		Name:    m.Name,
		Summary: mb.Summary,
	}
	if mb.Summary == "" {
		out.Summary = m.Description
	}
	if mb.Plan.ExperimentalInline != nil {
		waves := make([]missionext.Wave, 0, len(mb.Plan.ExperimentalInline.Waves))
		for _, w := range mb.Plan.ExperimentalInline.Waves {
			waves = append(waves, missionext.Wave{
				Label:     w.Label,
				Subagents: projectSubagents(w.Subagents),
			})
		}
		out.Plan.ExperimentalInline = &missionext.InlinePlan{Waves: waves}
	}
	out.Plan.Role = mb.Plan.Role
	out.Plan.Approval = missionext.NormalizePlanApproval(missionext.PlanApproval{
		Initial:   mb.Plan.Approval.Initial,
		Iteration: mb.Plan.Approval.Iteration,
	})
	out.Plan.MaxWaves = mb.Plan.MaxWaves
	if out.Plan.MaxWaves <= 0 {
		out.Plan.MaxWaves = missionext.DefaultMaxWaves
	}
	if out.Plan.MaxWaves > missionext.MaxMaxWaves {
		out.Plan.MaxWaves = missionext.MaxMaxWaves
	}
	if mb.Synthesis.Role != "" {
		out.Synthesis = missionext.SynthesisManifest{Role: mb.Synthesis.Role}
	}
	if mb.Control.Role != "" {
		out.Control = missionext.ControlManifest{Role: mb.Control.Role}
	}
	if mb.Research != nil && mb.Research.Role != "" {
		when := mb.Research.When
		if when == "" {
			when = missionext.ResearchWhenAuto
		}
		maxIter := mb.Research.MaxIterations
		if maxIter <= 0 {
			maxIter = missionext.ResearchDefaultMaxIterations
		}
		if maxIter > missionext.ResearchMaxIterationsCap {
			maxIter = missionext.ResearchMaxIterationsCap
		}
		out.Research = &missionext.ResearchManifest{
			Role:          mb.Research.Role,
			When:          when,
			Predicate:     mb.Research.Predicate,
			MaxIterations: maxIter,
		}
	}
	out.Capabilities = projectMissionCapabilities(mb.Capabilities)
	out.Roles = projectRoleCapabilities(m.Hugen.SubAgents)
	out.Workers = projectDoWorkers(m.Hugen.SubAgents, mb.Plan.Role, mb.Control.Role, mb.Synthesis.Role)
	// Phase 5.x — B11 §3.2.2 — pass the manifest's iter-0 AC seed
	// through verbatim. Templates are rendered with .Inputs at
	// mission spawn (auto_runner.RunMission), not here, since the
	// catalog projection has no access to the spawn-time inputs.
	if len(mb.AcceptanceCriteria) > 0 {
		out.AcceptanceCriteria = append([]string(nil), mb.AcceptanceCriteria...)
	}
	return out
}

// projectDoWorkers filters the skill's sub_agents down to the
// "Do" roles — everything that isn't the planner, the checker, or
// the synthesizer — and carries the role name + description into
// mission ext's WorkerManifest catalogue. The planner's first
// message renders this catalogue so the model picks a real role
// (`schema-explorer`, `query-builder`, …) instead of guessing
// `worker` and falling through to the generic _worker autoload.
func projectDoWorkers(roles []skillpkg.SubAgentRole, plannerRole, controlRole, synthesisRole string) []missionext.WorkerManifest {
	if len(roles) == 0 {
		return nil
	}
	skip := map[string]struct{}{}
	for _, r := range []string{plannerRole, controlRole, synthesisRole} {
		if r != "" {
			skip[r] = struct{}{}
		}
	}
	out := make([]missionext.WorkerManifest, 0, len(roles))
	for _, r := range roles {
		if r.Name == "" {
			continue
		}
		if _, drop := skip[r.Name]; drop {
			continue
		}
		out = append(out, missionext.WorkerManifest{
			Role:        r.Name,
			Description: strings.TrimSpace(r.Description),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// projectMissionCapabilities mirrors skill.MissionCapabilities onto
// mission ext's typed shape. Pointer-bool fields pass through
// verbatim — unset stays nil.
func projectMissionCapabilities(in skillpkg.MissionCapabilities) missionext.MissionCapabilities {
	return missionext.MissionCapabilities{
		Notepad:     in.Notepad,
		Whiteboard:  in.Whiteboard,
		PlanContext: in.PlanContext,
	}
}

// projectRoleCapabilities walks the skill's sub_agents list and
// returns the per-role capabilities map mission ext consumes at
// worker-spawn time. Roles with no declared capabilities are
// omitted; mission ext falls through to role-class defaults for
// those names. Phase F.
func projectRoleCapabilities(roles []skillpkg.SubAgentRole) map[string]missionext.RoleCapabilities {
	if len(roles) == 0 {
		return nil
	}
	out := make(map[string]missionext.RoleCapabilities, len(roles))
	for _, r := range roles {
		if r.Name == "" {
			continue
		}
		if r.Capabilities.PlanContext == "" {
			continue
		}
		out[r.Name] = missionext.RoleCapabilities{
			PlanContextAccess: r.Capabilities.PlanContext,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func projectSubagents(in []skillpkg.MissionPlanSubagent) []missionext.SubagentSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]missionext.SubagentSpec, 0, len(in))
	for _, s := range in {
		out = append(out, missionext.SubagentSpec{
			Name:      s.Name,
			Skill:     s.Skill,
			Role:      s.Role,
			Task:      s.Task,
			Inputs:    s.Inputs,
			DependsOn: append([]string(nil), s.DependsOn...),
		})
	}
	return out
}
