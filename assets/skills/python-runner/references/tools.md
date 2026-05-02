# Python-MCP tool envelope

The `python-mcp` provider exposes two tools — both spawn a fresh Python
process inside the session's relocatable venv. There is **no** persistent
interpreter, no kernel, no REPL state across calls.

## `python-mcp:run_code`

Inline snippet. The runtime writes `code` to a temp file inside
`${SESSION_DIR}` and execs the venv's Python on it.

```jsonc
{
  "code": "import pandas as pd; print(pd.__version__)",
  "timeout_ms": 30000   // optional; default 30 s, ceiling 10 min
}
```

When to use:
- One-liners and ad-hoc inspection (schema, head, describe).
- Debugging a script — try one statement, see what comes back.
- Anything <≈ 20 lines that you don't intend to re-run.

## `python-mcp:run_script`

Run a `.py` file already living under `${SESSION_DIR}`.

```jsonc
{
  "path": "scripts/transform.py",   // relative to ${SESSION_DIR}
  "timeout_ms": 600000              // optional; same ceiling as run_code
}
```

The runtime rejects:
- absolute paths (`/`, `~`, `${SESSION_DIR}/...`),
- paths containing `..`,
- non-`.py` files (script must end with `.py`).

When to use:
- Anything multi-line you'd want to re-run with tweaked params.
- Anything that imports a sibling module written via `bash.write_file`.
- Long-running ETL — pair with a generous `timeout_ms`.

Workflow:
1. `bash.write_file({path: "scripts/transform.py", content: "...\n"})`
2. `python-mcp:run_script({path: "scripts/transform.py"})`
3. On error: `bash.read_file` the script, fix, write back, re-run.

## Result envelope

Both tools return the same JSON shape:

```json
{
  "exit_code": 0,
  "stdout": "<captured stdout>",
  "stderr": "<captured stderr>",
  "duration_ms": 1234,
  "truncated": false
}
```

| Field | Meaning |
|-------|---------|
| `exit_code` | Process exit status. `0` = success. `!= 0` means the script raised. |
| `stdout` / `stderr` | Captured streams. Capped at 32 KiB per stream — see `truncated`. |
| `truncated` | `true` when either stream was capped. The tail is dropped, not the head. |
| `duration_ms` | Wall-clock time the child ran. |

`exit_code != 0` is a **script-level** failure — the user's code raised. Read
`stderr`, fix, retry. The MCP transport surfaces operator-level problems
(template missing, `uv` unavailable, timeout exceeded, bootstrap window
expired) as `tool_error{code:...}` envelopes, NOT as `exit_code != 0`.

## Environment contract

The runtime composes the child env from a clean baseline. Inherited keys
that begin with `HUGEN_` or `HUGR_ACCESS_TOKEN`/`HUGR_TOKEN_URL` are
**dropped** so children never see operator-only secrets. The Python process
gets:

| Var | Source | Notes |
|-----|--------|-------|
| `SESSION_DIR` | Runtime (absolute) | Workspace root for inputs/outputs. Stable for the session. |
| `HUGR_URL` | Operator YAML | Endpoint to dial (typically `…/ipc`). Empty under no-Hugr deployments. |
| `HUGR_TOKEN` | Runtime (rotated) | Fresh bearer per spawn. Empty under no-Hugr. |
| `PYTHONUNBUFFERED` | Runtime | `1` — stdout flushes per line; tail rendering doesn't lag. |
| `PYTHONDONTWRITEBYTECODE` | Runtime | `1` — no `__pycache__` clutter inside `${SESSION_DIR}`. |
| `MPLBACKEND` | Runtime | `Agg` — matplotlib runs headless, `plt.show()` is a no-op. |
| Any other | Operator-supplied via `tool_providers[].env` | Use this for things like `OPENAI_API_KEY` if the operator opts in. |

## Working with paths

`SESSION_DIR` is an absolute path **inside the child process**. The host's
view of the same directory may differ — when surfacing a path back to the
user, prefer the **relative-to-session** form:

```python
import os
out_abs = os.path.join(os.environ["SESSION_DIR"], "reports/sales.html")
out_rel = "reports/sales.html"  # ← what the user reads
```

Conventional subdirs:

```
${SESSION_DIR}/
├── data/        # parquet, csv — produced by hugr-query or python
├── scripts/     # .py modules written via bash.write_file
├── figures/     # static png/svg from matplotlib
└── reports/     # html, pdf — final user-facing artefacts
```

Create directories explicitly:

```python
os.makedirs(os.path.join(os.environ["SESSION_DIR"], "reports"), exist_ok=True)
```

## Common error patterns

- **`stderr` ends with "ModuleNotFoundError"** — the venv doesn't ship the
  package. The venv is sealed at template-build time; ask the operator to
  extend `assets/python/requirements.txt` and re-run
  `make python-mcp-template`. Don't `pip install` at runtime — there's
  nothing writable on the import path.
- **`stderr` says "permission denied" on a write** — you used an absolute
  path outside `${SESSION_DIR}`. Switch to a relative path joined with
  `os.environ["SESSION_DIR"]`.
- **`exit_code == 124` or `tool_error{code:timeout}`** — script ran past its
  deadline. Either chunk the work, raise `timeout_ms` (ceiling: 10 min), or
  switch to a `hugr-query` Parquet export + DuckDB aggregation if the bulk
  is upstream.
- **`tool_error{code:bootstrap_failed}`** — the per-session venv hasn't been
  provisioned yet and the lazy bootstrap failed. Operator-level: missing
  `uv`, missing template, no disk space. Surface the message verbatim and
  stop.
