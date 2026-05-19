package mission

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// MissionSkillExists implements [extension.MissionDispatcher].
// Returns (true, nil) when the named skill is registered in the
// mission ext's catalog AS A PDCA MISSION — i.e. the manifest
// carries a recognised `mission.plan.*` shape. The old
// `mission.enabled:true` flag is NOT consulted; missions in
// hugen are PDCA-shaped only.
//
// Either shape counts as "exists": ExperimentalInline waves (Phase
// A escape hatch) or a non-empty Plan.Role (Phase B planner-driven
// loop). A manifest declaring NEITHER section is in-progress and
// surfaces as not-found until completed, so spawn_mission can't
// kick off a mission with no executable plan.
func (e *Extension) MissionSkillExists(ctx context.Context, skill string) (bool, error) {
	if skill == "" || e.catalog == nil {
		return false, nil
	}
	m, err := e.catalog.LookupMission(ctx, skill)
	if err != nil {
		return false, err
	}
	if m == nil {
		return false, nil
	}
	hasInline := m.Plan.ExperimentalInline != nil && len(m.Plan.ExperimentalInline.Waves) > 0
	hasPlanner := m.Plan.Role != ""
	if !hasInline && !hasPlanner {
		return false, nil
	}
	return true, nil
}

// AdvertiseSystemPrompt implements [extension.Advertiser]. Renders
// the "## Available missions" block root reads to discover what
// missions it can spawn. Only fires on root sessions (depth 0);
// mission + worker tiers don't dispatch missions themselves, so
// the block is suppressed there.
//
// Empty when the catalog is empty or the calling session is not
// root.
func (e *Extension) AdvertiseSystemPrompt(ctx context.Context, state extension.SessionState) string {
	if state == nil || state.Depth() != 0 {
		return ""
	}
	if e.catalog == nil {
		return ""
	}
	entries, err := e.catalog.ListMissions(ctx)
	if err != nil || len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	var b strings.Builder
	b.WriteString("## Available missions\n\n")
	for _, m := range entries {
		summary := strings.TrimSpace(m.Summary)
		if summary == "" {
			fmt.Fprintf(&b, "- `%s`\n", m.Name)
			continue
		}
		fmt.Fprintf(&b, "- `%s` — %s\n", m.Name, summary)
	}
	return strings.TrimRight(b.String(), "\n")
}
