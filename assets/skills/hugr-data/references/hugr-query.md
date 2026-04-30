# hugr-query — file-output Hugr GraphQL provider

`hugr-query` is the in-tree, local MCP server bundled with the agent
(spawned per-agent over stdio). It complements the remote `hugr-main`
MCP: where `hugr-main` returns results inline, `hugr-query` writes
them to disk and returns a path plus a small preview.

Use `hugr-query` when:

- The expected row count exceeds ~50.
- You want to read the result back later via `bash.read_file`,
  pass it to another tool, or hand it to the user as an artefact.
- You need a JQ post-processing step that produces one JSON
  value (single document, not multiple files).

Use `hugr-main`'s `data-inline_graphql_result` when:

- Result is small (≤ 50 rows or one small object).
- You need the model to reason directly over the rows.
- A preview alone is not enough.

## Tool: `hugr-query:query`

Run a GraphQL query and persist tabular/object results to files.

**Args**:
- `graphql` (string, required) — query text.
- `variables` (object, optional) — GraphQL variables.
- `path` (string, optional) — output path under
  `/workspace/<session_id>/data/` or `/shared/<agent_id>/`.
  Defaults to a generated path under
  `/workspace/<session_id>/data/<short_id>.parquet`.
- `format` (`parquet` (default) | `json`) — applies to tabular
  shapes; non-tabular always serialises to JSON regardless.
- `timeout_ms` (int, optional) — per-call deadline. Silently
  clamped to the operator's ceiling (`HUGR_QUERY_MAX_TIMEOUT_MS`,
  default 24 h). Default per-call budget is 1 h.

**Returns**: `QueryResult` —
- `query_id` — short id of the call.
- `path` — written file when single output.
- `paths` — array of paths when GraphQL produced multiple
  top-level siblings.
- `format` — `parquet` or `json`.
- `row_count` — for tabular outputs.
- `preview` — ≤ 50 rows for tabular shapes; the value itself
  (capped) for objects/scalars.
- `elapsed_ms` — actual time the call took.

## Tool: `hugr-query:queryJQ`

Run a GraphQL query and post-process via JQ before persisting a
single JSON value.

**Args**:
- `graphql` (string, required).
- `variables` (object, optional).
- `jq` (string, required) — JQ expression applied server-side.
- `path` (string, optional) — defaults to
  `/workspace/<session_id>/data/<short_id>.json`.
- `timeout_ms` (int, optional) — same semantics as `hugr-query:query`.

**Returns**: `QueryResult` with `format=json`.

## Errors

The provider reports structured `tool_error` codes:

| code | meaning |
|------|---------|
| `timeout` | the call exceeded its effective deadline (LLM clamped or operator ceiling) |
| `path_escape` | requested `path` resolves outside the session workspace and the shared dir |
| `hugr_error` | Hugr returned a GraphQL-level error (see `graphql_errors`) |
| `auth` | token bootstrap or refresh failed |
| `jq_error` | `hugr-query:queryJQ` JQ expression invalid |
| `arg_validation` | LLM-supplied args did not match the schema |
| `io` | disk write failure (full disk, permission, etc.) |

## Path rules

- Bare or relative `path` → anchored under `/workspace/<sid>/data/`.
- Absolute `path` → must canonicalise (post-symlink-resolution)
  under either the session workspace or the shared dir.
  Anything else is rejected with `path_escape`.
- Multi-output GraphQL (more than one top-level field) writes
  one file per field; `paths` holds the array, `path` is empty.
  Field names are sanitised in filenames (slashes / spaces /
  dots become `_`).

## Reading the result back

After a successful `hugr-query:query`, the persisted file is reachable
from any subsequent `bash.read_file` / `bash.list_dir` call in
the same session. Example:

```text
1. hugr-query:query graphql=…
   → { path: "/workspace/<sid>/data/abc12345.parquet",
       row_count: 12345, preview: [...] }
2. bash.list_dir path="data"
3. bash.read_file path="data/abc12345.parquet" start=0 length=1024
   → first KB of the parquet header (binary)
```

For chunked inspection of a large Parquet, use `start` / `length`
arguments on `bash.read_file`.

## Choosing a budget

The default `timeout_ms` (1 h) handles most analytical queries.
Drop it (e.g. `timeout_ms=30000`) for interactive flows where
the user is waiting at the prompt; raise it (up to the operator
ceiling) only for the rare long-running aggregations. Setting
`timeout_ms` above the ceiling is silently clamped — the
returned `elapsed_ms` shows what actually ran.
