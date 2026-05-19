package runtime

import (
	"context"

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
// section recognised by mission ext's Phase-A shape. Phase B
// extends this with `plan.role: planner` (LLM-driven planning).
func isPdcaMission(m skillpkg.Manifest) bool {
	inline := m.Hugen.Mission.Plan.ExperimentalInline
	return inline != nil && len(inline.Waves) > 0
}

// projectMissionManifest converts a skill.Manifest's mission
// section into mission ext's typed shape. Returns nil when the
// skill is not a PDCA mission so the caller short-circuits
// without filling a partial struct.
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
	if mb.Synthesis.Role != "" {
		out.Synthesis = missionext.SynthesisManifest{Role: mb.Synthesis.Role}
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
