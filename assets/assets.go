// Package assets embeds the bundled skill manifests that ship with
// the hugen binary. Consumers (cmd/hugen, tests) read from
// SkillsFS via fs.FS conventions; the on-disk install lives at
// ${HUGEN_STATE}/skills/system/ and is refreshed at boot via
// cmd/hugen.installBundledSkills.
package assets

import "embed"

// SkillsFS holds every directory under assets/skills/. The
// top-level entries map one-to-one to a system-tier skill name.
//
//go:embed all:skills
var SkillsFS embed.FS
