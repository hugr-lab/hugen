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
	if len(s.Steps) == 0 {
		return nil, fmt.Errorf("scenario %q: steps is empty", s.Name)
	}
	for i, st := range s.Steps {
		if st.Say == "" && !st.Tick {
			return nil, fmt.Errorf("scenario %q step %d: must have either say: or tick: true",
				s.Name, i)
		}
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
