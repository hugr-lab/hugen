---
name: python-runner
license: Apache-2.0
description: >
  Run Python in a per-session venv (pandas, pyarrow, duckdb, plotly,
  matplotlib, great_tables, folium, weasyprint, hugr-client). The only
  path in this runtime for charts / plots / maps, HTML & PDF reports,
  and data transforms that don't fit a single SQL or GraphQL query.
  For pure SQL on workspace files prefer `duckdb-data`; for fetching
  from the Hugr platform prefer `hugr-data`.
allowed-tools:
  - provider: python-mcp
    tools:
      - run_code
      - run_script
  - provider: bash-mcp
    tools:
      - bash.write_file
      - bash.read_file
      - bash.list_dir
metadata:
  hugen:
    requires: []
    autoload: false
    autoload_for: []
    sub_agents: []
compatibility:
  model: any
  runtime: hugen-phase-3
---

# Python Runner

Per-session Python virtualenv with the data-stack pre-installed. The venv
materialises lazily from an operator-built template at first call — no
per-session network, no per-call install. Each session has its own copy and
its own working directory under `${SESSION_DIR}` that bash-mcp shares.

## Two tools

- **`python-mcp:run_code`** — execute a snippet. Args: `code` (string),
  `timeout_ms` (optional). Inline use, ad-hoc exploration.
- **`python-mcp:run_script`** — execute a `.py` file from the session
  workspace. Args: `path` (relative inside `${SESSION_DIR}`; absolute paths
  and `..` rejected), `timeout_ms` (optional). Multi-line work, anything you
  want to re-run.

Both return the same JSON envelope:

```json
{"exit_code":0,"stdout":"...","stderr":"...","duration_ms":1234,"truncated":false}
```

## Pre-installed packages

| Purpose | Package |
|---------|---------|
| Tabular | `pandas`, `pyarrow`, `duckdb` |
| Hugr API client | `hugr-client` |
| Static plots | `matplotlib` |
| Interactive / reports | `plotly`, `great_tables`, `folium` |
| HTML → PDF | `weasyprint` |

The venv is sealed at template-build time. Anything outside this list requires
the operator to extend `assets/python/requirements.txt` and re-run
`make python-mcp-template`.

## Standard workflow

NEVER run a non-trivial script before reading the relevant reference — the
patterns below cover the majority of tasks and prevent the typical
"works locally, breaks here" failures (cached `HUGR_TOKEN`, absolute paths,
`plt.show()` doing nothing).

0. **Read `tools` via `skill_ref`** the first time you spawn Python this
   session — it covers the tool envelope, timeouts, paths, env contract, and
   common error codes.
1. **Pick the right reference** below for the task and read it via
   `skill_ref` before composing code.
2. **Compose the code** — `run_code` for one-liners, `bash.write_file` +
   `run_script` for anything multi-line.
3. **Write outputs under `${SESSION_DIR}/`** — never absolute paths.
   Conventional subdirs: `data/`, `reports/`, `figures/`.
4. **Surface relative paths** to the user — they read files via the workspace
   root the host exposes.

## Task-Specific Guidance

| Task | Reference | When to read |
|------|-----------|--------------|
| **Tool envelope, env vars, paths** | `tools` | Always — first time this session |
| **Talk to Hugr from Python** | `hugr-client` | Any direct GraphQL call from Python |
| **DataFrames, joins, aggregates** | `data-science` | Read parquet/csv, transform, save back |
| **Charts** | `plotting` | matplotlib (static) or plotly (interactive) |
| **HTML / PDF reports, maps** | `visualization` | great_tables, folium, weasyprint |

If you skip the reference and the script fails on something covered there
(stale token, absolute path, headless backend, geometry decoding), the
correction belongs in the reference — read it before retrying.

## Critical rules (never forget)

- **Relative paths only.** Absolute paths and `..` escapes are rejected by
  `run_script`. Stay inside `${SESSION_DIR}`.
- **Re-read `HUGR_TOKEN` every call.** Don't stash it in a module-level
  constant — the runtime rotates it between calls. See `hugr-client` ref.
- **No interactive REPL, no shared state.** `run_code` and `run_script` are
  one-shot processes; nothing carries between calls. Persist via files.
- **`MPLBACKEND=Agg` is set** by the runtime — matplotlib runs headless,
  `plt.show()` is a no-op. Always `savefig`. See `plotting` ref.
- **Surface relative paths to the user**, not absolute — the host's view of
  the workspace differs from the path inside the script.

## Quick reference — env contract

| Env var | Source | Use |
|---------|--------|-----|
| `SESSION_DIR` | Runtime | Workspace root for inputs/outputs (absolute path inside the child) |
| `HUGR_URL` | Operator config | Hugr endpoint (typically `…/ipc`) |
| `HUGR_TOKEN` | Runtime (rotated) | Bearer for `hugr-client.HugrClient(token=...)` |
| `MPLBACKEND` | Runtime | Pinned to `Agg` |
| `PYTHONUNBUFFERED` | Runtime | `1` — stdout flushes per line |

`HUGR_ACCESS_TOKEN` and `HUGR_TOKEN_URL` (the loopback bootstrap pair) are
**dropped** before exec — Python should never see them. If your code reads
them, the runtime contract has been violated; fail loudly.
