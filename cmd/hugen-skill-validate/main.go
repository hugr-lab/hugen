// Command hugen-skill-validate parses one or more skill
// directories or SKILL.md paths and reports validity. Mirrors the
// agentskills.io skills-ref validate exit codes (0 ok, 1 invalid,
// 2 io error) and the same diagnostic format, so a hugen-installed
// CI can drop in where skills-ref was used.
package main

import (
	"fmt"
	"os"
)

// Stubbed in T001. Real implementation arrives in T021.
func main() {
	fmt.Fprintln(os.Stderr, "hugen-skill-validate: not yet implemented (phase-3 task T021)")
	os.Exit(2)
}
