// Package runtime assembles the hugen agent runtime from a fully
// resolved Config. Build executes a fixed 9-phase sequence —
// bundled_skills → http_auth → identity → storage → models → agent
// → skills_perms → tools → session_manager — and returns a Core
// holding every dependency a subcommand handler needs.
//
// The package is the single boot-path entry for both cmd/hugen and
// the observational scenario harness (phase 4.1b). Config is
// env-pure: callers project from their own bootstrap layer (e.g.
// cmd/hugen.BootstrapConfig + .env) into runtime.Config; Build does
// not read os.Environ. See design/001-agent-runtime/phase-4.1a-spec.md
// §3 (boundary contract) and §5 (phases of Build) for the contract.
package runtime
