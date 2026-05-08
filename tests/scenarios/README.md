# Phase 4.1b — observational scenario harness

This directory holds the **observational** scenario test suite —
not a regression gate. The runner boots a real `*runtime.Core`
against a live LLM (and optionally a real Hugr endpoint), drives
the agent through a YAML-described scenario, and dumps the
resulting `session_events` transcripts via GraphQL queries to
`t.Log`. Pass/fail is "did the runner crash"; behaviour judgment
is by-eye.

The pain point this harness exists to surface: **the model is
not reliably delegating long-running work to sub-agents**. Manual
smoke tests show the root agent doing everything inline. Run
`delegation_required` across multiple LLMs and read
`tool_calls_root` side-by-side to see which model classes need
prompt iteration.

## Quickstart

```bash
# 1. Copy the env template, fill in keys.
cp tests/scenarios/.test.env.example tests/scenarios/.test.env
$EDITOR tests/scenarios/.test.env

# 2. Capture a fresh Hugr OIDC token once (browser flow runs once).
make hugr-token

# 3. Run a single LLM × topology cell.
make scenario-run run=claude-sonnet-embedded

# 4. Run a single scenario inside that cell.
make scenario-one run=claude-sonnet-embedded name=delegation_required

# 5. Run everything (whatever your env credentials allow).
make scenario
```

Per-run artefacts land in
`tests/scenarios/.data/run-<timestamp>/<run-name>/`:

- `agent-config.yaml` — exact merged config the run booted with
- `state/memory.db`, `state/engine.db` — DuckDB files preserved
  for follow-up inspection (`duckdb state/memory.db`)
- `runtime.log` — slog text output
- `workspaces/<session-id>/` — per-session bash workspace

## Layout

```text
tests/scenarios/
├── README.md                # this file
├── runner_test.go           # TestScenarios entry
├── runs.yaml                # (LLM × topology × scenarios) tuples
├── .test.env.example        # template for credentials + URLs
├── configs/
│   ├── llm-claude-sonnet.yaml
│   ├── llm-gemini-pro.yaml
│   ├── llm-gpt-5-4.yaml
│   ├── llm-gemma4-26b.yaml
│   ├── topology-embedded.yaml
│   └── topology-external-hugr.yaml
├── harness/                 # test-only Go package (build-tag scenario)
│   ├── env.go runs.go runtime.go runner.go inspect.go types.go
│   └── *_test.go            # harness self-tests
├── single_explorer/scenario.yaml
├── delegation_required/scenario.yaml
└── ...                      # one directory per scenario
```

## Adding a scenario

1. Create `<name>/scenario.yaml`. Keep it under 100 lines.
2. Reference the live session id with the literal `$sid` string in
   query `vars:`; the harness substitutes the runtime-allocated id.
3. Use `jq()` in GraphQL when filtering on the `metadata` JSON
   column (extension_frame ops). Set `path: extensions.jq.jq` on
   the query so the harness extracts the jq result.
4. Add the scenario name to one or more `runs:` entries in
   `runs.yaml`. List its `requires:` if it needs Hugr or a
   specific LLM.

See `design/001-agent-runtime/phase-4.1b-spec.md §4 / §9` for the
full step-primitive + query reference.

## Conventions

- **No asserts on LLM output.** Drift is captured in `memory.db`
  for human review.
- **No mock providers.** All MCP subprocesses are real; a test
  that needs a stub belongs in `pkg/<name>/_test.go`, not here.
- **`requires:`-gated.** Empty `.test.env` is a valid state —
  every run skips with a clear message; the harness must compile
  and discovery walks must succeed.
- **Build-tag pair** `duckdb_arrow,scenario` keeps every file in
  this tree out of `go test ./...`. Default test paths never see
  these files.
