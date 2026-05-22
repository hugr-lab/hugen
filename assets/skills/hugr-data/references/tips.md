# Tips — what to do when a query goes sideways

This is the "I called a tool and the result confuses me" companion.
Two recurring failure modes get full diagnoses here; the SKILL.md
points at this reference instead of inlining the detail.

---

## 1. Empty / null result (`is_error: false` but no useful data)

Symptoms:

- `{"data": null}`
- `{"data": {"<field>": null}}` — top-level field came back null
- `{"data": {"foo": {"categories": [], "summary": null}}}` —
  empty arrays + null sums after jq reshaping
- `_rows_count: 0`
- aggregation `sum: null`, `avg: null`, etc.
- jq error `cannot iterate over: null` — same root cause: the
  query returned nothing where jq expected an array

**Empty is NOT a transient hiccup.** Re-running the same query —
or worse, re-running it with a different jq guard (`[]?` vs `[]`,
`select(... != null)` vs no filter) — will return the same
emptiness. Two identical empty replies in a row = stop and re-plan
the QUERY, not the post-processing.

### Diagnose by symptom

| Symptom | Likely cause | Fix |
|---|---|---|
| `bucket_aggregation` returns rows but every `key.<field>` is `null` | The column you chose for `key { <field> }` is sparsely populated / nullable / mostly empty | **Pick a different grouping field.** Call `discovery-field_values(type_name, field_name)` to see which columns have meaningful distributions BEFORE bucket-keying. |
| `bucket_aggregation` returns `[]` (empty array) | The bucket source itself filtered to zero, OR you wrapped it in `select(...)` that filters every entry | Re-run WITHOUT the jq `select` first to confirm the GraphQL returned ≥1 row. If yes, the jq filter is the culprit. If no, the GraphQL filter is. |
| `<aggregation>.sum: null` | The summed field is null for every row matching your filter, OR the field is non-numeric, OR your filter eliminated all rows | Inspect with `schema-type_fields` (is the field actually numeric?) + a small sample query (does it have values?). |
| `data: null` (top-level) | Wrong query field name OR RBAC stripped the result | Re-check with `schema-type_fields` for the parent type. Don't add `?` to jq — the path is wrong, not the data. |
| jq `cannot iterate over: null` | Same as the row above — the GraphQL response has `null` where jq expected an array | Fix the QUERY. `?` is a last-resort guard ONLY after confirming the shape via schema introspection. |
| `data: { "data": { ... } }` after jq returned nothing useful | The jq path is missing the top `data` wrapper — Hugr returns `{"data": {"<module>": {...}}}` and jq input is the full envelope | Prefix your jq path with `.data.<module>`, not `.<module>`. Common slip when copy-pasting bucket access. |

If after one diagnosis pass the query still returns empty / null —
**stop**. Switch to `schema-type_fields` + `discovery-field_values`
to confirm what's actually in the columns before building yet
another aggregation.

---

## 2. Inline result truncation (`is_truncated: true`)

`hugr-main:data-inline_graphql_result` enforces a default 2000-byte
cap on returned content. Hitting it sets `is_truncated: true` and
returns a `preview` with the first ~2KB. The full result lives on
the engine side but didn't travel back to you.

### Don't blindly re-bump `max_result_size`

Bumping the cap to 5000 helps when the FULL result is small
(under 5KB) and the default just clipped a bit. But:

- Full result above 5KB → 5000-cap still truncates.
- Re-calling identical queries with bigger cap wastes turns.
- Large bucket / top-N lists are routinely 10–100KB once
  expanded — they will NEVER fit in inline.

### File-output escape hatch

When the preview doesn't cover what you need (cut at item 12 of 50,
report needs the full series, downstream worker wants the data),
**switch tools:**

- `hugr-query:query` — Execute the same GraphQL, but Hugr writes
  the result to a Parquet (tabular leaves) or JSON (object leaves)
  file under the session workspace. Returns `path` + preview.
- `hugr-query:query_jq` — Same, but with a JQ transform applied
  server-side. Returns ONE JSON value at the given path.

The downstream consumer reads back via:

- `bash-mcp:bash.read_file(path)` — small files (< 100KB) loaded
  inline into the worker's reasoning.
- For larger artefacts: hand the `path` to the next worker via
  `inputs.<key>` (planner sets it on `next_wave.subagents[*].inputs`).
  The consumer reads / opens / parses the file directly.

### When to pick which tool

| Need | Tool |
|---|---|
| One-shot lookup; result clearly fits in 2-5KB; you'll act on it in this turn | `data-inline_graphql_result`, bump `max_result_size` to 5000 if needed |
| Building a report / chart / dashboard — need the full data, not a preview | `hugr-query:query` (Parquet/JSON to workspace, downstream worker reads back) |
| Want ONE structured JSON value (a single metric, a single nested object) and the GraphQL result is large | `hugr-query:query_jq` (server-side JQ, persisted) |
| Top-N over a wide table, complete bucket list, distribution by category for visualization | `hugr-query:query` (Parquet) |

For **report-building work** the default is `hugr-query:query` — the
report needs the full data, a 2KB preview is not enough. Reserve
inline for quick checks where the answer is one number.

---

## 3. Other gotchas (cheat-sheet)

- **`hugr-query` JQ input shape:** the JQ runs against the FULL
  `{data, errors}` envelope. Results live under `.data.<field>`.
  Common slip: jq path `.<field>` instead of `.data.<field>` —
  same fix as the empty-data row above.
- **`schema-type_fields` capped at 50 by default:** wide tables
  (100+ columns) need `limit: 200` + paginate, or
  `relevance_query: "<NL phrase>"` to semantic-rank. Don't conclude
  "field doesn't exist" before paginating + relevance-querying.
- **`discovery-field_values` is your friend** for `enum`-like
  columns — shows actual values + frequencies. Use it BEFORE
  choosing a bucket-aggregation key on a column you haven't seen.
- **Nested relation sub-queries** take `nested_order_by` /
  `nested_limit` / `nested_offset`. Plain `order_by` / `limit` on
  a nested relation apply pre-join globally and silently null-out
  per-parent rows. See `query-patterns` ref for the full rule.

---

Read order on a stuck investigation:

1. Look at the actual JSON shape returned (don't trust the
   model's gut feel about what's there).
2. Map the symptom to the table in §1 or §2.
3. If neither matches — re-check the parent type via
   `schema-type_fields` + sample row, then re-plan.
