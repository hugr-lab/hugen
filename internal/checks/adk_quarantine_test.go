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
//	"No `google.golang.org/adk` and no `google.golang.org/genai`
//	 anywhere in the binary."
//
// Phase 1 quarantined ADK below pkg/models. Phase 2 (R-Plan-23 /
// R-Plan-24) finished the eviction: pkg/models, cmd/hugen, and the
// rest of the binary have zero adk/genai deps. The orphaned legacy
// pkg/a2a still has the deps; it is intentionally excluded — it is
// not in the binary, and phase 10 retires it. Adding pkg/a2a back
// to the binary requires fixing its deps first or this test fails.
func TestADKQuarantine(t *testing.T) {
	leaves := []string{
		"github.com/hugr-lab/hugen/cmd/hugen/...",
		"github.com/hugr-lab/hugen/cmd/hugen-skill-validate/...",
		"github.com/hugr-lab/hugen/pkg/protocol/...",
		"github.com/hugr-lab/hugen/pkg/model/...",
		"github.com/hugr-lab/hugen/pkg/models/...",
		"github.com/hugr-lab/hugen/pkg/runtime/...",
		"github.com/hugr-lab/hugen/pkg/adapter/...",
		"github.com/hugr-lab/hugen/pkg/config/...",
		"github.com/hugr-lab/hugen/pkg/skill/...",
		"github.com/hugr-lab/hugen/pkg/tool/...",
		"github.com/hugr-lab/hugen/pkg/auth/...",
		"github.com/hugr-lab/hugen/mcp/bash-mcp/...",
		"github.com/hugr-lab/hugen/mcp/hugr-query/...",
		"github.com/hugr-lab/hugen/mcp/python-mcp/...",
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
			"ADK / genai imported in the hugen binary — constitution violation.\n"+
				"Leaks (sorted):\n  %s\n\n"+
				"Remediation: ADK was fully evicted in phase 2. Runtime-side "+
				"callers must consume pkg/model.Model directly. The orphaned "+
				"pkg/a2a package keeps ADK deps until phase 10 retires it.",
			strings.Join(leaks, "\n  "),
		)
	}
}
