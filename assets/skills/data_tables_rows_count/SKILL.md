---
name: data_tables_rows_count
description: >
  Count rows in one Hugr data object. Input: exact `name` or
  natural-language `query` (+ optional `module`). Output: a single
  integer count.
license: Apache-2.0
allowed-tools:
  - provider: hugr-main
    tools:
      - discovery-search_modules
      - discovery-search_module_data_objects
      - discovery-describe_data_objects
      - data-inline_graphql_result
  - provider: session
    tools:
      - inquire
metadata:
  hugen:
    requires_skills: []
    allowed_skills: []
    autoload: false
    tier_compatibility: [worker]
    mission:
      on_close:
        notepad:
          skip: true
    task:
      eligible: true
      kind: worker
      # intent: reasoning
      goal_summary: >
        Count rows in one Hugr data object via the aggregation
        GraphQL field. Resolves the target by exact name (fast path)
        or natural-language query (search + disambiguation).
      inputs_schema:
        type: object
        oneOf:
          - required: [name]
          - required: [query]
        properties:
          name:
            type: string
            description: "Exact type name. Fast path: describe directly."
          query:
            type: string
            description: "Natural-language description. Used when name is absent."
          module:
            type: string
            description: "Optional module to narrow the query search."
compatibility:
  model: any
  runtime: hugen
---

# data_tables_rows_count

Count rows in one Hugr data object. The tools you need are in your
catalogue — don't load anything else.

**Output discipline — read this first.** Your ONLY deliverable is the
terminal `handoff` block (step 4): a single integer in `body.count`.
Do NOT write files, do NOT run shell, do NOT call `bash` / `python` /
`hugr-query` — the answer is one number and it lives in the handoff,
nowhere else. Even if those tools appear in your surface, this task
never needs them: use ONLY the pipeline's
search / describe / `data-inline_graphql_result` / `inquire` tools.
Counting one table is a 3–4 tool job, not a data-analysis exercise.

## Pipeline

Inputs: `{name?, query?, module?}`. Schema guarantees `name` OR `query`.

### 1. Resolve the table name

Three routes — pick the first that matches:

- **`name` set** → use it. Go to step 2.
- **`query` set, `module` set** → `search_module_data_objects(module, query)`. Pick a table.
- **`query` set, `module` absent** → `search_modules(query)`. Pick a module. Then `search_module_data_objects(module, query)`. Pick a table.

**`search_modules`** — module descriptions are intentionally broad
(modules cover many tables / topics). Rules:

- 0 items → handoff `no_module`. Stop.
- 1 item → use it.
- ≥2 items → **always inquire**. Do not pick by score or by "this
  one looks more relevant". Module choice decides what tables
  you'll even see at the next step; ask the user.

**`search_module_data_objects`** — table descriptions tend to be
specific. Rules:

- 0 items → handoff `no_match`. Stop.
- 1 item → use it.
- ≥2 items, exactly one has a non-empty description that literally
  names the target (other descriptions empty or about a different
  concept) → use that one. Score alone is not a tiebreaker; read
  the descriptions.
- ≥2 items, none is an obvious Clear match (multiple plausible,
  multiple empty, or top scorer has empty description while a
  lower scorer literally names the target) → **inquire**.

Take `items[i].name` verbatim when picking — that's the catalog
type name describe needs.

### 2. Describe the table

`describe_data_objects(names: [name])` — pass the single type name as a
one-element array; read the one record back as `items[0]` (call it `O`).

- error / `not_found` AND you got here from the input `name` (first
  time) → set `query := name`, drop `name`, restart step 1. One
  fallback only.
- error / `not_found` after a search round → handoff `not_found`. Stop.
- From `O.queries[]`, pick the entry with `query_type == "aggregate"`
  (NOT `bucket_agg`). No such entry → handoff `no_aggregation`. Stop.

Bind two variables for step 3:

- `M` = `O.module` (may be empty, may contain dots).
- `Q` = the aggregate query name from `O.queries[]` (copy verbatim).

### 3. Build the GraphQL query

**Dots in `M` are GraphQL nesting, never identifiers.** Translate each
dot into a `{ ... }` wrapper. Compare:

```text
M = ""          query { Q { _rows_count } }
M = "foo"       query { foo { Q { _rows_count } } }
M = "foo.bar"   query { foo { bar { Q { _rows_count } } } }
M = "a.b.c"     query { a { b { c { Q { _rows_count } } } } }
```

So `foo.bar` becomes `foo { bar { … } }`, NEVER `foo_bar { … }` or
`"foo.bar" { … }`. Underscores inside a single segment (e.g. `Q`
itself, or a one-word module like `osm_amenities`) stay as written —
they're part of the name. Only the dot is a separator.

Call `data-inline_graphql_result(query: "<your query>")`. No
`jq_transform`. The count lives at the same path inside `data`:

```text
M = "foo.bar"  →  response.data.foo.bar.Q._rows_count
M = ""         →  response.data.Q._rows_count
```

GraphQL error in response → handoff `query_failed` with the error
text. Stop.

### 4. Hand off

```handoff
{
  "status": "ok",
  "body": { "count": <integer>, "module": "<M>", "field": "<Q>" },
  "memory_summary": "Counted <M>.<Q>: <integer> rows"
}
```

`count` is verbatim — no rounding, no thousand separator, no quoting.

## Inquire

When step 1 needs disambiguation, call `session:inquire`:

```json
{
  "type": "clarification",
  "question": "Which one?",
  "clarifications": [{
    "id": "module_choice",
    "question": "Which one?",
    "kind": "required",
    "allow_comment": true,
    "options": [
      "<items[0].name> — <items[0].description>",
      "<items[1].name> — <items[1].description>",
      "Other — none of these match; describe in the comment"
    ]
  }]
}
```

For a **table** clarification: use `id: "table_choice"` and append
` (module: <items[].module>)` to each result option line.

Truncate descriptions to ~120 chars. The trailing `Other` option is
mandatory.

**Response handling** — read `answers.<id>.{value, comment}`:

- `value` matches a real option → take the prefix before ` — ` as the
  name (module or table) and continue.
- `value` starts with `Other —` AND `comment` is non-empty → use
  `comment` as the new `query`, re-run the SAME search. Max 2 retries
  per step (3 searches total). After that → handoff `user_no_match`.
- `value` starts with `Other —` AND `comment` is empty → handoff
  `user_choice_invalid`. Stop.
- `value` empty / no ` — ` separator → handoff `user_choice_invalid`. Stop.

## Handoff reasons

All `status: "fail"` (except `ok`). Always include a short
`memory_summary`; specific fields per reason listed below.

| reason | when | extra fields |
|---|---|---|
| `no_module` | `search_modules` returned 0 items | — |
| `no_match` | `search_module_data_objects` returned 0 items | — |
| `not_found` | describe failed (after the optional name→search fallback) | — |
| `no_aggregation` | describe ok but no `aggregate` entry in `queries[]` | — |
| `query_failed` | aggregation call returned a GraphQL error | `error` (truncated message) |
| `user_choice_invalid` | inquire response did not match any option, or `Other` without comment | `got` (verbatim reply) |
| `user_no_match` | user exhausted the refinement budget | `last_comment` |
