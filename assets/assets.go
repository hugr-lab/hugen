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

// ConstitutionFS holds the agent constitution markdown bundled
// with the binary. Top-level entries map one-to-one to session
// types — `agent.md` is the universal constitution rendered for
// every root session. Override via the operator's state directory:
// ${HUGEN_STATE}/constitution/<file> shadows the embedded copy.
//
//go:embed all:constitution
var ConstitutionFS embed.FS

// PromptsFS holds the model-visible prompt templates that ship
// with the binary. Top-level entries are categorised by surface
// (interrupts/, system/, skill/, notepad/, plan/, inquiry/);
// each leaf is a text/template file with `.tmpl` extension.
// Phase 5.1 §1.3.
//
// Consumed via pkg/prompts.NewRenderer, which scopes the FS to
// the `prompts/` sub-tree via fs.Sub. Operator override path
// (set in runtime config) shadows the embedded copy file by
// file at render time.
//
//go:embed all:prompts
var PromptsFS embed.FS
