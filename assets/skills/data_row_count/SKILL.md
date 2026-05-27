---
name: data_row_count
description: >
  Count rows in a Hugr Data Mesh table via GraphQL `_rows_count`
  aggregation, with optional filter. One number out, suitable as
  a quick health metric or a scheduled diagnostic.
license: Apache-2.0
# Tools are inherited from the `hugr-data` closure
# (`requires_skills`) — discovery / schema / validate / inline
# GraphQL all come from there. Keeping this list empty avoids
# duplicating the grant list and stays in lockstep with hugr-data
# when the platform adds new discovery / data tools.
allowed-tools: []
metadata:
  hugen:
    requires_skills: [hugr-data]
    autoload: false
    tier_compatibility: [worker]

    task:
      eligible: true
      kind: worker
      goal_summary: >
        Count rows in a named Hugr data object via the `_rows_count`
        aggregation, with optional filter.
      inputs_schema:
        type: object
        required: [data_object]
        properties:
          module:
            type: string
            description: >
              Optional module path (e.g. `hub.db`). Omit to let the
              recipe call `discovery-search_module_data_objects` and
              pick the unique match for `data_object` across the
              deployment.
          data_object:
            type: string
            description: >
              The data object (table / view) to count. Use the
              backend's canonical name; the recipe resolves the
              callable GraphQL field via discovery.
          filter:
            type: object
            description: >
              Optional GraphQL filter object — passed verbatim to
              `_aggregation(filter: ...)`. Example:
              `{"status": {"eq": "active"}}`.

compatibility:
  model: any
  runtime: hugen-phase-6
---

# `data_row_count` recipe

Tiny task-eligible recipe: returns ONE row-count value for a named
Hugr data object. Demonstrates the Phase-6 recipe shape — one
worker, one GraphQL call, one structured handoff. Suitable for
scheduling ("every morning check the active-transactions count").

## Steps

Hard rule: **never** guess a GraphQL field name. Always discover
the data object FIRST, then read its callable query field shapes,
THEN build the aggregation query.

1. **Discover the data object.** Call
   `hugr-main:discovery-search_module_data_objects` with the
   `data_object` value. The response carries
   `items[].queries[].name` — those are the callable GraphQL field
   names per data object, per query flavour. Hugr's field naming
   (prefixed vs. unprefixed, dotted vs. underscored) is per-
   deployment; the discovery response is the source of truth, never
   the caller's guess.
   - If `module` was supplied, narrow to that module's row.
   - If `module` was empty and discovery returns exactly one match,
     use it.
   - Zero / many matches → `status=fail` handoff naming the
     candidates so the caller can disambiguate.
2. **Inspect the field schema.** From the discovery row, pick the
   aggregation field — typically the entry whose `query_type` is
   `aggregation` (or `bucket_aggregation` if the deployment only
   exposes the bucketed variant). If you need to know whether the
   field accepts a `filter` argument or what types it admits, call
   `hugr-main:schema-type_fields` on the parent type returned by
   discovery. Do NOT skip this step when `filter` is non-empty —
   filter object shape varies per field type, and the validator's
   error messages are terser than the schema lookup's.
3. **Build the aggregation query.** Use the discovered field name
   verbatim. Plain-aggregation shape:
   ```graphql
   query {
     <discovered_field>(filter: <filter>) {
       _rows_count
     }
   }
   ```
   If only `<discovered_field>_bucket_aggregation` exists, swap to:
   ```graphql
   query {
     <discovered_bucket_field>(filter: <filter>) {
       aggregations { _rows_count }
     }
   }
   ```
   When `filter` is absent, omit the `(filter: ...)` argument
   entirely.
4. **Validate.** Call `hugr-main:data-validate_graphql_query` with
   the assembled query. On validation error, treat it as a schema
   misread — re-fetch the field schema (step 2) rather than
   blind-retrying; emit `status=fail` if the validator still rejects
   after one corrected attempt.
5. **Execute.** Call `hugr-main:data-inline_graphql_result` with the
   validated query. For the plain aggregation use
   `jq_transform: ".data | first(.[]?) | ._rows_count"`. For the
   bucket variant adjust to
   `".data | first(.[]?) | .aggregations._rows_count"`.
6. **Emit handoff.** One fenced JSON block:
   ```handoff
   {
     "status": "ok",
     "body": { "count": <integer>, "field": "<discovered-field>" },
     "memory_summary": "Counted <discovered-field>: <integer> rows"
   }
   ```

## Failure modes

- **No such data object.** Validation surfaces "unknown field" —
  return `status=fail` with the discovery result so the user can
  retry with a correct name.
- **Field needs the unfiltered variant.** Some deployments expose
  only `_bucket_aggregation` (not plain `_aggregation`). If the
  validator complains about `_aggregation`, retry with
  `<module>_<data_object>_bucket_aggregation { aggregations { _rows_count } }`
  and adjust the jq path accordingly.
- **Filter rejected.** GraphQL filter shapes vary per data type
  (e.g. text uses `{eq, ilike}`, timestamps use `{gte, lte}`). Echo
  the validator error verbatim — the user can correct the filter.

## Not in scope

- **Multi-table joins** — load `hugr-data` and write a real query.
- **Bucket / grouped counts** — see `*_bucket_aggregation`; this
  recipe is single-number-only by design.
- **Trend / alerting logic** — the recipe returns a number; the
  scheduler / caller decides what to do with it.
