// Package skill implements the skill subsystem: an
// agentskills.io-conformant manifest parser, a SkillStore over
// system/community/local/inline backends (hub:// is a phase-7
// stub), and a SkillManager that holds per-session bindings and
// streams change events.
//
// Hugen extensions live exclusively under metadata.hugen.* in the
// manifest; the parser keeps unknown top-level keys in
// Manifest.Metadata so skills authored against future spec
// revisions still parse.
//
// SkillManager is the per-runtime registry. Bindings(sessionID)
// returns a per-Turn snapshot keyed by a monotonic generation
// counter; ToolManager rebuilds its catalogue on mismatch. Loaded
// skills' metadata.hugen.requires closure is resolved
// transitively; cycles raise ErrSkillCycle.
package skill
