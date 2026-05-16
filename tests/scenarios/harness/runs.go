//go:build duckdb_arrow && scenario

package harness

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/oasdiff/yaml"
)

// LoadRuns parses runs.yaml from disk. Paths inside the file
// (LLM, Topology) stay relative to the runs.yaml directory; the
// caller resolves them against runsPath when needed.
func LoadRuns(runsPath string) (*RunsFile, error) {
	data, err := os.ReadFile(runsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", runsPath, err)
	}
	var rf RunsFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", runsPath, err)
	}
	for i, r := range rf.Runs {
		if r.Name == "" {
			return nil, fmt.Errorf("runs[%d]: name is required", i)
		}
		if r.LLM == "" {
			return nil, fmt.Errorf("run %q: llm is required", r.Name)
		}
		if r.Topology == "" {
			return nil, fmt.Errorf("run %q: topology is required", r.Name)
		}
		if len(r.Scenarios) == 0 {
			return nil, fmt.Errorf("run %q: scenarios is empty", r.Name)
		}
	}
	return &rf, nil
}

// LoadScenario parses a scenario.yaml. The dirName argument is the
// scenario directory's basename, used as the default scenario Name
// when the YAML omits it.
func LoadScenario(scenarioPath, dirName string) (*Scenario, error) {
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", scenarioPath, err)
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", scenarioPath, err)
	}
	if s.Name == "" {
		s.Name = dirName
	}
	// Two modes:
	//   • single-root: Steps populated, Roots nil. Validate as
	//     before.
	//   • multi-root (phase 5.1b δ): Roots populated, Steps
	//     empty. Validate per-root Steps.
	switch {
	case len(s.Roots) > 0 && len(s.Steps) > 0:
		return nil, fmt.Errorf("scenario %q: roots: and steps: are mutually exclusive — use roots[<name>].steps[]",
			s.Name)
	case len(s.Roots) > 0:
		for rootName, rs := range s.Roots {
			if len(rs.Steps) == 0 {
				return nil, fmt.Errorf("scenario %q: root %q has empty steps",
					s.Name, rootName)
			}
			for i, st := range rs.Steps {
				if st.Say == "" && !st.Tick && !st.RestartRuntime {
					return nil, fmt.Errorf("scenario %q root %q step %d: must have say, tick: true, or restart_runtime: true",
						s.Name, rootName, i)
				}
			}
		}
	case len(s.Steps) > 0:
		for i, st := range s.Steps {
			if st.Say == "" && !st.Tick && !st.RestartRuntime {
				return nil, fmt.Errorf("scenario %q step %d: must have say, tick: true, or restart_runtime: true",
					s.Name, i)
			}
		}
	default:
		return nil, fmt.Errorf("scenario %q: either steps: or roots: must be non-empty",
			s.Name)
	}
	return &s, nil
}

// ResolveRunPath joins a RunsFile-relative path against the
// directory of runs.yaml. Absolute paths pass through.
func ResolveRunPath(runsPath, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(runsPath), p)
}
