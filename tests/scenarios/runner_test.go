//go:build duckdb_arrow && scenario

// Package scenarios is the observational scenario test entry point.
// TestScenarios walks runs.yaml × every scenario directory; runs
// that miss their `requires:` env vars t.Skip without crashing.
//
// Invocation:
//
//	make scenario                                # all runs
//	make scenario-run run=claude-sonnet-embedded # one run, every scenario
//	make scenario-one run=... name=...           # single scenario
//
// Or directly:
//
//	go test -tags=duckdb_arrow,scenario -count=1 -timeout=30m \
//	  -run TestScenarios -v ./tests/scenarios/...
//
// See design/001-agent-runtime/phase-4.1b-spec.md for the contract.
package scenarios

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/tests/scenarios/harness"
)

// TestScenarios is the single test entry. Two-level subtests:
//
//	TestScenarios/<run-name>/<scenario-name>
//
// so a developer can run a focused subset via standard
// `-run TestScenarios/...` selectors.
func TestScenarios(t *testing.T) {
	rootCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	runsPath := filepath.Join(rootCwd, "runs.yaml")
	envPath := filepath.Join(rootCwd, ".test.env")
	baseConfig := filepath.Join(rootCwd, "..", "..", "config.yaml")

	rf, err := harness.LoadRuns(runsPath)
	if err != nil {
		t.Fatalf("load runs.yaml: %v", err)
	}
	if len(rf.Runs) == 0 {
		t.Skip("runs.yaml has no entries")
	}

	timestamp := time.Now().UTC().Format("20060102-150405")
	dataRoot := filepath.Join(rootCwd, ".data", "run-"+timestamp)

	for _, run := range rf.Runs {
		run := run
		t.Run(run.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			t.Cleanup(cancel)

			rt := harness.Setup(ctx, t, harness.SetupOpts{
				Run:            &run,
				RunsPath:       runsPath,
				RunsRoot:       rootCwd,
				EnvFile:        envPath,
				BaseConfigPath: baseConfig,
				RunDir:         filepath.Join(dataRoot, run.Name),
			})

			for _, name := range run.Scenarios {
				name := name
				t.Run(name, func(t *testing.T) {
					scenarioPath := filepath.Join(rootCwd, "cases", name, "scenario.yaml")
					if _, err := os.Stat(scenarioPath); err != nil {
						t.Skipf("scenario file missing: %v", err)
					}
					sc, err := harness.LoadScenario(scenarioPath, name)
					if err != nil {
						t.Fatalf("load scenario %s: %v", name, err)
					}
					if ok, reason := harness.EvalRequires(rt.Env, sc.Requires); !ok {
						t.Skipf("scenario %s skipped: %s", name, reason)
					}

					// Phase 5.2 ι — install any test-only skill
					// fixtures the scenario depends on before
					// opening the session so the SkillStore picks
					// them up via the local backend's first scan.
					rt.InstallFixtures(t, sc.Fixtures)

					if len(sc.Roots) > 0 {
						rt.RunMultiRoot(ctx, t, sc)
						return
					}
					handle := rt.OpenSession(ctx, t)
					for i, step := range sc.Steps {
						handle.Step(ctx, step, i)
					}
				})
			}
		})
	}
}
