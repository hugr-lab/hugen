---
name: data_utils
description: >
  Quick Hugr Data Mesh utilities — diagnostic recipes you can run
  on a table without writing a full GraphQL query yourself. Load
  this skill to get a small toolkit of one-shot recipes in the tool
  catalog.
license: Apache-2.0
allowed-tools:
  - provider: task
    tools:
      - data_tables_rows_count
metadata:
  hugen:
    requires_skills: []
    autoload: false
    tier_compatibility: [root, mission, worker]
compatibility:
  model: any
  runtime: hugen-phase-6
---

# `data_utils` category

Loadable skill that admits Hugr Data Mesh utility recipes into the
session's tool catalog. Recipes themselves live as separate
task-eligible skills; this manifest is the discovery + permission
group that brings them into root.

Currently bundled:

- **`task:data_tables_rows_count(data_object)`** — counts rows in a
  named data object via the aggregation GraphQL field. Returns a
  single integer. Useful for daily diagnostics ("how many
  transactions yesterday?") or quick sanity checks before a deeper
  analysis.

## When to load

The user named a table and wants a count, schema-shape check, or
similarly tiny one-question answer about a single data object —
load this category, call the appropriate `task:*` tool, surface the
result. Bigger questions (joins, group-by, dashboards) still belong
in the analyst mission.
