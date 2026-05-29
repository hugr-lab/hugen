---
name: hugr-data
license: Apache-2.0
description: >
  Query the Hugr Data Mesh — a GraphQL-over-SQL engine that federates
  PostgreSQL, DuckDB, Parquet, Iceberg, and REST APIs under one schema.
  Use to discover modules / catalogs / data objects, build GraphQL
  queries, run aggregations and bucket aggregations, do spatial or
  dynamic joins, save large results to Parquet via hugr-query, and
  apply jq transforms. Read-only data fetch from the platform —
  for SQL on local files load `duckdb-data`; for charts / HTML / PDF
  reports load `python-runner`. FIRST consider recipes: for a simple
  single-object question (a count, a single value, a quick listing),
  if a `(recipe catalog)` in `## Available skills` covers it, prefer
  that recipe over hand-rolling the query here — load hugr-data for
  questions that need composition, joins, aggregation, or exploration.
allowed-tools:
  - provider: hugr-main
    tools:
      - discovery-*
      - schema-*
      - data-*
  - provider: hugr-query
    tools:
      - query
      - query_jq
metadata:
  hugen:
    requires: []
    autoload: false
    autoload_for: []
    # Root loads this skill when the user asks a data question that
    # is short enough to answer in chat (a count, a single value, a
    # quick listing). Mission loads it for read-only reference
    # grounding (skill:files / skill:ref) before fan-out; workers
    # load it to execute real queries inside a mission.
    tier_compatibility: [root, mission, worker]
    # In-turn corrective hints (ModelInTurnAdvisor / on_tool_error):
    # when a GraphQL call fails with one of these signatures, the
    # runtime folds the guidance INLINE into the failing tool result
    # the model reads next — pushing it back to discovery instead of
    # re-guessing. Matched against hugr-main:data-* + hugr-query:*
    # (where query errors surface). The body teaches the concepts;
    # these fire at the exact failure point.
    hints:
      - type: on_tool_error
        tools: ["hugr-main:data-*", "hugr-query:*"]
        match: 'Cannot query field "[^"]*_aggregation"'
        message: >
          There is no `<entity>_aggregation` root query field. Aggregations
          live INSIDE a module on a data object (`module { table_aggregation
          { ... } }`), never at the top level. Do NOT guess more
          `*_aggregation` names — run discovery-search_modules, then
          discovery-search_module_data_objects to get the real query field
          names, then build the aggregation on the object. See
          references/aggregations.md.
      - type: on_tool_error
        tools: ["hugr-main:data-*", "hugr-query:*"]
        match: 'Cannot query field "[^"]*" on type "Query"'
        message: >
          That field is not a top-level query. It is either a data object
          inside a module (dotted module names are NESTING: `osm.bw` →
          `osm { bw { ... } }`, never `osm_bw`) or it is restricted by your
          role. STOP guessing prefixed/underscored forms — run
          discovery-search_modules and read the exact module + field names
          back. See references/instructions.md.
      - type: on_tool_error
        tools: ["hugr-main:data-*", "hugr-query:*"]
        match: 'Cannot query field "[^"]*" on type "[a-z]'
        message: >
          That field does not exist on that type. Run
          schema-type_fields(type_name: "<Type>") and pick a real field —
          never guess. If you want a field by MEANING but cannot find the
          exact name, re-call with relevance_query: "<what you mean>" and
          include_description: true (wide tables return only the first 50
          fields alphabetically by default). See references/query.md.
      - type: on_tool_error
        tools: ["hugr-main:data-*", "hugr-query:*"]
        match: "(?i)(unknown|invalid).*(filter|operator|argument)"
        message: >
          Hugr's filter dialect is NOT standard GraphQL (there is no `neq`;
          negation is `_not: { field: { eq: ... } }`; one-to-many relations
          use any_of / all_of / none_of). Read references/filter-guide.md
          before retrying.
    sub_agents: []
    memory:
      categories:
        schema:
          description: "Table structures, field names and types"
          volatility: stable
          initial_score: 0.8
          tags_hint: "domain, entity"
        query_template:
          description: "Working GraphQL query patterns"
          volatility: stable
          initial_score: 0.7
          tags_hint: "domain, entity, operation"
        anti_pattern:
          description: "Errors and corrections with full context"
          volatility: stable
          initial_score: 0.9
          tags_hint: "domain, entity, error_type"
        field_values:
          description: "Distinct values and distributions"
          volatility: moderate
          initial_score: 0.6
          tags_hint: "domain, entity, field"
        data_insight:
          description: "Discovered patterns and relationships"
          volatility: fast
          initial_score: 0.5
          tags_hint: "domain, topic"
      review:
        enabled: true
        min_tool_calls: 3
        window_tokens: 4000
        overlap_tokens: 500
        floor_age: 1h
        exclude_event_types: [compaction_summary, reasoning, error]
        prompt: |
          Focus on Hugr Data Mesh artefacts worth remembering:
          schema structures, working query templates, anti-patterns
          (errors + corrections), notable field values, cross-table
          relationships. Skip greetings, retries, and raw tool output.
      compaction:
        preserve: [schema, query_patterns, error_messages, identifiers, numbers]
        discard: [greetings, repeated_tool_calls, verbose_raw_output, reasoning]
compatibility:
  model: any
  runtime: hugen-phase-3
---

# Hugr Data Mesh

Hugr is an open-source Data Mesh platform: a high-performance GraphQL
backend that uses DuckDB to federate PostgreSQL, DuckDB, Parquet,
Iceberg, Delta Lake, and REST APIs into one read-only GraphQL schema.

## The data model (the mental model that matters)

- **Modules** are hierarchical namespaces. **Dotted module names are
  nesting, not identifiers**: a module `osm.bw` is queried as
  `osm { bw { ... } }` — never `osm_bw`, `osm.bw`, or any flat /
  prefixed form. The dot count equals the nesting depth before the
  data-object field. This is the single most common mistake.
- **Data objects** (tables / views) live inside modules. Each exposes
  generated query fields: `table` (select), `table_by_pk`,
  `table_aggregation` (single-row), `table_bucket_aggregation`
  (GROUP BY). **Aggregations are fields on a data object inside its
  module — there is no `<entity>_aggregation` root query.**
- **Functions** live on a separate path:
  `function { module { my_func(arg: "x") { ... } } }`. Aggregations
  are NOT functions — don't look for them with
  `discovery-search_module_functions`.
- **Relations** let one query traverse to related objects (nested
  sub-selection) and aggregate / filter over them — prefer this to
  issuing several flat queries and joining client-side.
- The schema is **role-filtered** (RBAC): what discovery and schema
  tools return is exactly what your role may see. "Not found" can mean
  "restricted", not "nonexistent" — never assume access, rely on what
  the tools actually return.

## Tools at a glance

Names are fully-qualified `<provider>:<tool>`; the live tool list
shows `:` and `.` as `_` (e.g. `hugr-main_discovery-search_modules`),
but use the canonical form below when reading docs / refs.

- `hugr-main:discovery-search_modules` — find modules by NL query.
  **Start here for any data question.**
- `hugr-main:discovery-search_module_data_objects` — find tables/views
  in a module; returns both the type name (for `schema-*`) and the
  callable query field names. **Copy `queries[].name` verbatim.**
- `hugr-main:discovery-search_module_functions` — find custom functions
  (NOT aggregations).
- `hugr-main:discovery-field_values` — distinct values + stats for a
  field.
- `hugr-main:schema-type_fields` — fields of a type. Default `limit: 50`
  (wide tables have more — paginate via `offset` or rank with
  `relevance_query`); `include_arguments: true` for a field's
  filter/order_by/args; `include_description: true` when names are
  auto-generated.
- `hugr-main:schema-type_info`, `hugr-main:schema-enum_values` —
  type metadata / enum values.
- `hugr-main:data-validate_graphql_query` — validate before executing.
- `hugr-main:data-inline_graphql_result` — execute, inline reply;
  optional `jq_transform`. **JQ runs on the full `{data, errors,
  extensions}` envelope — every path starts with `.data`.**
- `hugr-query:query` / `hugr-query:query_jq` — execute and persist
  Parquet/JSON to disk, return path + preview. Use for big result sets
  or file output. Same `.data` envelope rule for `query_jq`. See
  `references/hugr-query.md` before first use.

Rule of thumb: rows ≤ ~50 / small JSON → `hugr-main` inline; bigger
payloads or anything you'll read back via bash → `hugr-query`.

## Discover before you query

Module and field names cannot be guessed and the schema is
role-filtered, so **never write a GraphQL query before discovery**.
The reliable path: `discovery-search_modules` → note the exact module
names → `discovery-search_module_data_objects` → `schema-type_fields`
on the type you'll query → then compose. When a query errors, read the
error and pivot back to discovery rather than re-tweaking the same
shape (the runtime will steer you inline on common failures).

Good habits, with the ref that teaches each:
- Build **one** compound query with aliases instead of many small ones
  (`references/query.md`, `references/query-patterns.md`).
- Prefer server-side `_aggregation` / `_bucket_aggregation` over
  fetching rows and rolling up client-side (`references/aggregations.md`).
- Filter and aggregate **through relations** to cut data at the source
  (`references/query-patterns.md`, `references/filter-guide.md`).

## When a request is ambiguous

Hugr federates many sources, so similar/overlapping modules, tables,
and metrics are common (the same entity in a raw and a curated module;
"count of patients" = rows in a registry vs distinct subjects across
events). When you must produce a concrete answer and discovery returns
≥ 2 plausible candidates the context can't disambiguate, **stop before
the first `data-*` call and `session:inquire(type="clarification")`** —
list the candidates one per line as `name — short description` plus a
trailing `Other — describe what you mean`. Silently picking the first
match and returning its number is worse than asking. Skip the inquire
only when one candidate is the obvious dominant match (named by the
user/task, or alone in its module). **Exception — research /
exploration tasks**: the ambiguity IS the subject; record candidates
in your finding instead of inquiring.

## Reference catalogue

The skill ships a reference library under `references/`, delivered on
demand via `skill:ref(skill="hugr-data", ref="<name>")`. The body
above is deliberately minimal — fetch the right reference before
composing anything non-trivial; one `skill:ref` now beats fifteen
trial-and-error tool calls later.

| Reference | When to read |
|-----------|--------------|
| `instructions` | Master reference: schema model, dotted-module nesting, query-field naming, anti-patterns. First touch of a new schema. |
| `start`, `overview` | Lighter onboarding companions to `instructions`. |
| `query` | GraphQL select shape — fields, args, aliases. |
| `query-patterns` | Relations, nested sub-query args, `_join` cross-source, distinct_on, parameterised views. |
| `aggregations` | `_aggregation` / `_bucket_aggregation`, functions by field type, group-by mechanics. |
| `filter-guide` | Filter operators, relation filters (`any_of` / `all_of` / `none_of`), `_and` / `_or` / `_not` (Hugr's dialect is NOT standard GraphQL). |
| `spatial-queries` | `_spatial`: INTERSECTS / WITHIN / CONTAINS / DISJOINT / DWITHIN, geometry-to-geometry joins. |
| `h3-spatial` | `h3(resolution)` aggregation, density, geoembeddings, proportional redistribution. |
| `advanced-features` | Vector search, geometry transforms, JSON `struct:` extraction, cube tables, mutations, parameterised views. |
| `analyze` | Analytical workflows — distributions, anomalies, time series. |
| `dashboard` | Multi-panel KPI / chart query shapes. |
| `queries-deep-dive` | JQ functions, geometry / JSON filter operators, parameterised view internals. |
| `hugr-query` | Output-to-file mechanics: Parquet vs JSON per leaf, path layout, preview. |
| `tips` | Stuck-investigation: null/empty results, jq path slips, `is_truncated` escape hatch. |

Reading order on a new task: `instructions` (if unread this session) →
the task-specific reference above → the relevant reference again if the
first query errors.
