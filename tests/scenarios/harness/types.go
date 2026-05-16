//go:build duckdb_arrow && scenario

package harness

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration wraps time.Duration with JSON / YAML unmarshalling that
// accepts strings ("60s", "5m"), bare-number seconds (60), and
// nested numeric scalars. The default time.Duration UnmarshalJSON
// only accepts integer nanoseconds, which makes scenario.yaml
// unwriteable in human-friendly units. The oasdiff/yaml package
// goes through JSON, so a custom UnmarshalJSON covers both paths.
type Duration time.Duration

// String renders via the underlying time.Duration formatter
// ("1m30s") so log output matches the YAML the operator wrote.
func (d Duration) String() string { return time.Duration(d).String() }

// Std exposes the wrapped time.Duration for sites that take the
// stdlib type (context deadlines, timer arithmetic).
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalJSON accepts:
//   - quoted duration strings: "90s", "5m", "1h"
//   - bare numbers: 90 (interpreted as seconds)
//   - null: zero duration
func (d *Duration) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*d = 0
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		dur, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", s, err)
		}
		*d = Duration(dur)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(data, &secs); err != nil {
		return fmt.Errorf("parse duration scalar: %w", err)
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// RunsFile is the top-level shape of tests/scenarios/runs.yaml.
type RunsFile struct {
	Runs []Run `yaml:"runs"`
}

// Run is one (LLM × topology × scenarios) tuple. The harness boots
// one *runtime.Core per Run and replays every listed scenario
// sequentially against it. Each scenario gets its own root session
// inside the Core; the per-run state directory survives on disk.
type Run struct {
	Name string `yaml:"name"`
	// LLM is the path (relative to runs.yaml) to a config that
	// merges into the agent-config and selects models.model.
	LLM string `yaml:"llm"`
	// Topology is the path to a config that selects tool_providers
	// (embedded vs external-hugr).
	Topology string `yaml:"topology"`
	// Requires gates the whole run on environment availability.
	// Known keys: "hugr" (HUGR_URL + HUGR_ACCESS_TOKEN), "anthropic"
	// (ANTHROPIC_API_KEY), "gemini" (GEMINI_API_KEY), "openai"
	// (OPENAI_API_KEY), "local" (LLM_LOCAL_URL).
	Requires  []string `yaml:"requires"`
	Scenarios []string `yaml:"scenarios"`
}

// Scenario is the shape of <name>/scenario.yaml. The Name field is
// optional; the harness defaults it to the directory basename when
// empty.
//
// Single-root scenarios (`Steps` populated, `Roots` nil) follow
// the original phase-4.1b flow: one OpenSession, sequential
// steps, queries reference `$sid`.
//
// Multi-root scenarios (phase 5.1b δ — `Roots` populated, `Steps`
// nil) open one root per `Roots` entry, run each entry's `Steps`
// concurrently in a goroutine, then run `Assertions` queries
// (single-shot, after all roots' work finishes). Assertion
// queries can reference per-root ids via `$sid_<name>` template
// variables (e.g. `$sid_alice`, `$sid_bob`).
type Scenario struct {
	Name      string   `yaml:"name,omitempty"`
	Requires  []string `yaml:"requires,omitempty"`
	SessionID string   `yaml:"session_id,omitempty"` // informational only — runtime allocates real id
	Steps     []Step   `yaml:"steps,omitempty"`

	// Fixtures names test-only skill bundles the harness installs
	// into the run's local skill backend before Core boot. Each
	// entry resolves to `tests/scenarios/fixtures/<name>/` and is
	// copied to `${stateDir}/skills/local/<name>/`. Phase 5.2 ι.
	Fixtures []string `yaml:"fixtures,omitempty"`

	// Overrides is a free-form config patch deep-merged on top of
	// the run's agent-config.yaml before Core boot. Used by ε
	// scenarios that need short timeouts / smaller caps than the
	// production defaults. Phase 5.2 ι.
	Overrides map[string]any `yaml:"overrides,omitempty"`

	// Roots names a set of independent root sessions opened in
	// parallel under a shared Manager. Phase 5.1b δ. Mutually
	// exclusive with Steps. Map key becomes the `$sid_<key>`
	// template variable in Assertions.
	Roots map[string]RootSpec `yaml:"roots,omitempty"`

	// Assertions are GraphQL inspection clauses run after every
	// root's Steps finished. Used for cross-root invariants
	// (e.g. "alice has a persisted inquiry_request that bob's
	// session_events tail does NOT contain"). Each query may
	// reference `$sid_<root-name>` in its Vars block — the
	// harness resolves to that root's session id. Single-shot;
	// no Step-level Budget / WaitFor* applies.
	Assertions []Query `yaml:"assertions,omitempty"`
}

// RootSpec describes one root session of a multi-root scenario.
// Owner is propagated to OpenRequest.OwnerID — useful for
// per-root permission isolation tests; empty defaults to
// "harness-<root-name>".
type RootSpec struct {
	Owner string `yaml:"owner,omitempty"`
	Steps []Step `yaml:"steps"`
}

// Step is one user-driven beat. Either Say or Tick must be present
// (Say sends a UserMessage; Tick is a no-op tick that lets the
// inbox drain). WaitForSubagents and WaitForCondition are optional
// settling sentinels run before Queries.
type Step struct {
	Say              string    `yaml:"say,omitempty"`
	Tick             bool      `yaml:"tick,omitempty"`
	Budget           Duration  `yaml:"budget,omitempty"` // overrides default 60s
	WaitForSubagents Duration  `yaml:"wait_for_subagents,omitempty"`
	WaitForCondition *WaitCond `yaml:"wait_for_condition,omitempty"`
	// InquiryResponses scripts answers to session:inquire calls that
	// bubble up while this step is active. The harness pump matches
	// each *protocol.InquiryRequest against the rules in order; the
	// first un-consumed match is consumed and answered via the
	// Manager.Deliver path. Unmatched requests fall through to the
	// runtime inquire-tool timeout. Phase 5.1 § κ.
	InquiryResponses []InquiryRule `json:"inquiry_responses,omitempty" yaml:"inquiry_responses,omitempty"` //nolint:tagliatelle
	Queries          []Query       `yaml:"queries,omitempty"`

	// PostSettle is an extra sleep BEFORE running Queries — gives
	// runtime-driven asynchronous events (idle-timer fires, ceiling
	// drops, parking idle expirations) time to land in the event
	// log. Use sparingly; prefer WaitForCondition for assertions
	// that can be expressed as a SQL/GraphQL predicate. Phase 5.2 ι.
	PostSettle Duration `yaml:"post_settle,omitempty"`

	// RestartRuntime, when true, signals the runner to gracefully
	// stop the current Core after this step's queries land and
	// build a fresh Core against the same StateDir before the next
	// step runs. The fresh Core's Manager.RestoreActive reattaches
	// the scenario's root via the persisted session_events;
	// scenario step indexing continues against the restored root
	// id. Either Say + RestartRuntime can coexist (run the step,
	// then bounce). Phase 5.2 ι (restore_parked_replay).
	RestartRuntime bool `yaml:"restart_runtime,omitempty"`
}

// WaitCond is a generic "poll until persisted state matches" gate.
// Sparingly used — only for scenarios that need to observe a
// specific side-effect before logging queries (e.g. notepad rows
// for a sub-agent that's still running).
type WaitCond struct {
	GraphQL  string         `yaml:"graphql"`
	Vars     map[string]any `yaml:"vars,omitempty"`
	Path     string         `yaml:"path,omitempty"`
	Expected int            `yaml:"expected_rows"`
	Budget   Duration       `yaml:"budget,omitempty"`
}

// Query is one inspection clause logged after the step's primary
// action settles. Result of Query is rendered into t.Log; failures
// (GraphQL errors) are logged and the runner keeps going — never
// t.Fatal on a query.
type Query struct {
	Name    string         `yaml:"name"`
	GraphQL string         `yaml:"graphql"`
	Vars    map[string]any `yaml:"vars,omitempty"`
	// Path is a dotted selector into the GraphQL response. Empty =
	// dump the whole response. For jq()-wrapped queries the value
	// is "extensions.jq.jq" because the built-in jq() field returns
	// its result there.
	Path string `yaml:"path,omitempty"`
}
