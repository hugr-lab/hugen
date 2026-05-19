// Package fixture carries the Phase A smoke-test mission skill —
// the manifest scenarios load as `_mission_v2` to exercise mission
// ext end-to-end without an LLM planner in the loop.
//
// The fixture is a Go constant rather than an asset under
// assets/system/ so it's deletable at Phase B end alongside the
// `plan.experimental_inline` escape hatch. Production paths never
// import this package; only unit tests under pkg/extension/mission
// and the scenario harness reach in.
//
// Deliberately NOT under `internal/` even though the spec describes
// it that way: the scenario harness lives outside `pkg/` and Go's
// internal rule would block the import. Treat the package as
// fixture-scope by convention — no production wiring should pull
// it in. Phase B deletes the package wholesale alongside the
// `plan.experimental_inline` escape hatch.
package fixture

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SkillName is the manifest's `name` field. Scenarios spawn this
// mission via session:spawn_mission(skill: _mission_v2, ...).
// The underscore prefix matches existing system-skill convention
// even though _mission_v2 is fixture-only — keeping the prefix
// stops it from being treated as a hub-installable extension on
// the model's Available missions block once the catalogue picks
// it up by name.
const SkillName = "_mission_v2"

// ManifestYAML is the SKILL.md body the fixture writes. Frontmatter
// + a minimal body so the SkillStore parser accepts it (Parse
// requires the frontmatter fence). The mission shape carries one
// wave with a single echo worker plus an optional synthesizer; the
// scripted scenario model emits the canonical handoff fence under
// both roles.
const ManifestYAML = `---
name: _mission_v2
description: Phase A fixture — single-wave PDCA mission with optional synthesizer. Deleted at Phase B end alongside plan.experimental_inline.
license: Apache-2.0
allowed-tools:
  - provider: mission
    tools:
      - get_handoff
      - finish
metadata:
  hugen:
    tier_compatibility: [mission]
    sub_agents:
      - name: echo
        description: Echo the inputs back as a handoff fence.
      - name: synthesizer
        description: Combine prior handoffs into the mission's final answer.
    mission:
      summary: "Phase A smoke-test mission — runs one echo wave then synthesises."
      plan:
        experimental_inline:
          waves:
            - label: wave-1
              subagents:
                - name: w1
                  role: echo
                  task: "Emit a handoff fence echoing the mission goal."
      synthesis:
        role: synthesizer
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _mission_v2 fixture skill

Phase A smoke-test mission. Drives one wave (a single ` + "`echo`" + ` worker) and
an optional synthesizer that consumes the prior wave's handoff and
emits the mission's final answer. The whole plan is hardcoded in
the manifest's ` + "`mission.plan.experimental_inline`" + ` — there is no
planner LLM. Phase B replaces this with a real planner role and the
fixture is deleted.
`

// WriteTo writes the fixture as a SKILL.md file under
// localSkillsRoot/<SkillName>/SKILL.md. The SkillStore's local-tier
// reader picks it up at boot. Returns an error if directory
// creation or file write fails; callers run it from test setup
// before runtime.Build.
//
// The directory layout mirrors what an operator-installed skill on
// the local tier looks like (state/skills/local/<name>/SKILL.md).
func WriteTo(localSkillsRoot string) error {
	dir := filepath.Join(localSkillsRoot, SkillName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mission fixture: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(ManifestYAML), 0o600); err != nil {
		return fmt.Errorf("mission fixture: write %s: %w", path, err)
	}
	return nil
}

// WriteToFS is the fs.FS-targeted variant for in-memory test stores
// that take a virtual filesystem. Today every caller uses the disk
// path above; the FS form is reserved for future tests that don't
// touch real disk.
//
// Kept as a stub returning a "not implemented" error so the dual-
// path API stays declared without misleading callers.
func WriteToFS(_ fs.FS) error {
	return fmt.Errorf("mission fixture: WriteToFS not implemented; use WriteTo for disk-backed tests")
}
