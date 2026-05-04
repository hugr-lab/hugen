---
name: _root
description: Built-in skill granting a root session the orchestration tools it needs to plan work, spawn sub-agents, and follow their results.
license: Apache-2.0
allowed-tools:
  - provider: session
    tools:
      - spawn_subagent
      - wait_subagents
      - subagent_runs
      - subagent_cancel
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [root]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _root skill

The _root skill is autoloaded into every root session — the
conversation a user opens with the agent. It exposes the orchestration
surface a coordinator needs:

- `session:spawn_subagent` — fan out one or more sub-agent sessions
  in a single batched call. Each entry needs a `task`; `skill` and
  `role` are optional but recommended so the child boots with the
  right toolset.
- `session:wait_subagents` — block until the listed sub-agents
  produce a terminal result. Returns one row per id with `status`
  (`completed` / `hard_ceiling` / `subagent_cancel` / `cancel_cascade` /
  `restart_died` / `panic`), `result` (final assistant text), and
  `reason` (free-form mirror of the child's `session_terminated`).
- `session:subagent_runs` — paginated transcript pull-through. Use
  this when you need to see a sub-agent's intermediate work before
  it finishes — long-running runs, mid-flight diagnostics.
- `session:subagent_cancel` — terminate one of your sub-agents with
  a reason. Cancellation cascades to its descendants automatically.

## When to spawn vs. answer directly

Spawn a sub-agent when:

- The work needs an isolated scratch directory or a different toolset
  (e.g. heavy DuckDB exploration, long-running scripts).
- You want to run multiple investigations in parallel and merge the
  results.
- A specific role is the right contract for the work (e.g. a
  research-and-summarise pattern).

Answer directly when the request can be handled by your own loaded
skills + tools without isolation. Spawning a sub-agent is not free —
each one takes a process slot and adds latency.

## What this skill does NOT grant

- `session:parent_context` — root has no parent. Calling it always
  surfaces `tool_error{code:"no_parent"}`.
- `session:whiteboard_write` — the host of a whiteboard usually
  orchestrates rather than writing into its own broadcast channel.
  Override per skill if your deployment really needs this.

Higher-tier skills (analyst, planner, whiteboard-host) layer on top
of this surface. Check the skill catalogue in your system prompt
before reaching for `skill_load`.
