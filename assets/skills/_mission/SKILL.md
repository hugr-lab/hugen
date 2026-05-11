---
name: _mission
description: Built-in skill granting a mission-tier session its coordination + orchestration surface — plan, whiteboard, spawn, parent context.
license: Apache-2.0
allowed-tools:
  - provider: session
    tools:
      - spawn_subagent
      - wait_subagents
      - subagent_runs
      - subagent_cancel
      - parent_context
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [mission]
    tier_compatibility: [mission]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _mission skill

The _mission skill is autoloaded into every mission-tier session —
the depth-1 session a root spawns to coordinate a piece of work.
It exposes the baseline tools a coordinator needs:

- `session:spawn_subagent` — fan out one or more worker sessions in
  a single batched call. Each entry needs a `task`; `skill` and
  `role` are optional but recommended so the child boots with the
  right toolset.
- `session:wait_subagents` — block until the listed workers produce
  a terminal result. Returns one row per id with `status`
  (`completed` / `hard_ceiling` / `subagent_cancel` /
  `cancel_cascade` / `restart_died` / `panic`), `result` (final
  assistant text), and `reason` (free-form mirror of the child's
  `session_terminated`).
- `session:subagent_runs` — paginated transcript pull-through. Use
  this when you need to see a worker's intermediate work before it
  finishes — long-running runs, mid-flight diagnostics.
- `session:subagent_cancel` — terminate one of your workers with a
  reason. Cancellation cascades to its descendants automatically.
- `session:parent_context` — read your direct parent's (root's)
  user-facing conversation: the messages root exchanged with the
  user. Use this when you need anchor context root didn't bake into
  your spawn `inputs`. Filtered to user and assistant messages only
  — tool calls, reasoning, and internal events are excluded by
  design.

## Working with your parent

Root passed you a `goal` and (optionally) an `inputs` blob when it
spawned you. Treat both as authoritative — root will not revise the
goal mid-run unless it cancels and re-spawns. When you finish, the
final assistant message you produce becomes the `result` field root
sees. Keep it tight and structured — root will route it directly
into its own reasoning, not display it to the end user verbatim.

## Decomposing into waves

Your job is to break the goal into focused worker tasks and fan
them out. Workers are leaf executors — they do data work, you do
coordination. Iterate: spawn a wave, wait, read the whiteboard,
synthesise, decide the next wave (if any). End when you have
enough to answer.

## What this skill does NOT grant

- Domain data tools (hugr-*, python-*, duckdb-*, bash-*). Mission
  is coordination; workers do data work. Spawn a worker with the
  right skill instead of loading data tools yourself.
- `whiteboard:init` / `whiteboard:stop` — these come from the
  `_whiteboard` skill when it autoloads at your tier. The
  whiteboard surface is shared across the tree, not owned by
  `_mission`.
- `plan:*` — the planner surface comes from `_planner` autoload.
  Loading `_planner` is conditional on your tier; check
  `skill:tools_catalog` if you need a plan and don't see the tools.
