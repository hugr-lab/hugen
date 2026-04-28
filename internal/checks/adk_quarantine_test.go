// Package checks holds enforcement tests that protect repository-wide
// invariants the constitution declares. These tests intentionally
// shell out to `go list` because that's the most reliable way to
// inspect transitive imports — anything else (parsing source files
// by hand, walking go/build) would miss build-tag-gated paths.
package checks

import (
	"os/exec"
	"strings"
	"testing"
)

// TestADKQuarantine enforces the constitutional rule:
//
//	"`google.golang.org/adk` and its transitive `genai` are
//	 quarantined to `pkg/models` only."
//
// The test fails if any package below the listed leaves transitively
// imports adk or genai. cmd/hugen and pkg/models are intentionally
// excluded — they are the legitimate consumers of pkg/models's
// ADK-bridging surface.
func TestADKQuarantine(t *testing.T) {
	leaves := []string{
		"github.com/hugr-lab/hugen/pkg/protocol/...",
		"github.com/hugr-lab/hugen/pkg/model/...",
		"github.com/hugr-lab/hugen/pkg/runtime/...",
		"github.com/hugr-lab/hugen/pkg/adapter/...",
	}
	args := append([]string{"list", "-tags=duckdb_arrow", "-deps"}, leaves...)
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	var leaks []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "google.golang.org/adk") ||
			strings.HasPrefix(line, "google.golang.org/genai") {
			leaks = append(leaks, line)
		}
	}
	if len(leaks) > 0 {
		t.Fatalf(
			"ADK / genai imported below pkg/models — constitution violation.\n"+
				"Leaks (sorted):\n  %s\n\n"+
				"Remediation: keep ADK + genai as private internals of pkg/models. "+
				"Runtime-side callers must consume pkg/model.Model and friends.",
			strings.Join(leaks, "\n  "),
		)
	}
}
