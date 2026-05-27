---
name: data_row_count
description: >
  Count rows in a Hugr Data Mesh data object via the aggregation
  GraphQL field. One number out, suitable as a quick health metric
  or a scheduled diagnostic.
license: Apache-2.0
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
        Count rows in a named Hugr data object via the aggregation
        GraphQL field.
      inputs_schema:
        type: object
        required: [data_object]
        properties:
          data_object:
            type: string
            description: >
              The data object (table / view) to count. Either the
              backend's canonical name on its own, or `module.table`
              when the user named both — discovery accepts either.

compatibility:
  model: any
  runtime: hugen-phase-6
---

# `data_row_count` recipe

Tiny task-eligible recipe: returns ONE row-count value for a named
Hugr data object. Three tool calls, one structured handoff.

## Steps

1. **Discover the data object.**
   `hugr-main:discovery-search_module_data_objects(query:
   "<data_object>")`. The response lists matches with a `queries[]`
   array per item; find the entry whose `query_type` is the
   aggregation variant (`aggregation` or `bucket_aggregation` —
   the latter only when the plain `aggregation` is absent). Copy
   that `queries[].name` verbatim — it is the callable GraphQL
   field name. If discovery returns zero or multiple matches that
   look equally plausible, emit a `status=fail` handoff naming the
   candidates rather than guessing.

2. **Execute the aggregation.** Build the GraphQL query inline and
   run it through `hugr-main:data-inline_graphql_result`:

   - **Plain aggregation** (`query_type: aggregation`):
     ```graphql
     query { <discovered-name> { _rows_count } }
     ```
     `jq_transform`: `.data.<discovered-name>._rows_count`
   - **Bucket aggregation** (`query_type: bucket_aggregation`):
     ```graphql
     query { <discovered-name> { aggregations { _rows_count } } }
     ```
     `jq_transform`: `.data.<discovered-name>.aggregations._rows_count`

   JQ paths MUST start with `.data` — that's the response envelope
   shape (see `hugr-data` skill body for the rule).

3. **Emit handoff.** One fenced JSON block:
   ```handoff
   {
     "status": "ok",
     "body": { "count": <integer>, "field": "<discovered-name>" },
     "memory_summary": "Counted <discovered-name>: <integer> rows"
   }
   ```

## Not in scope

- **Filters / partial counts.** Recipe takes no filter. For
  filtered counts load `hugr-data` and write a real query.
- **Multi-table joins, group-by, distincts.** Same — this recipe
  is single-number-only by design.
- **Validation step.** The query shape is trivial; if execution
  fails, the inline-result error message is informative enough.
  Re-validate via `data-validate_graphql_query` only on a clear
  schema mismatch (e.g. discovery field rename mid-task).
