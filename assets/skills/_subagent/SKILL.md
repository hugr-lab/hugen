---
name: _subagent
description: Built-in skill granting a sub-agent session its baseline orchestration + parent-context surface.
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
    autoload_for: [subagent]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _subagent skill

The _subagent skill is autoloaded into every spawned sub-agent
session. It mirrors the _root surface plus one extra tool that only
makes sense for non-root sessions:

- `session:parent_context` — read your direct parent's user-facing
  conversation: the messages your spawning session exchanged with
  its own user / parent. Use this when you need anchor context the
  parent didn't bake into your spawn `inputs`. Filtered to user and
  assistant messages only — tool calls, reasoning, and internal
  events are excluded by design.

The four `session:subagent_*` tools are also available so a sub-agent
can fan out further sub-agents of its own. Whether the actual spawn
is permitted is decided at runtime:

- The runtime caps the tree depth (default 5). Exceeding it surfaces
  `tool_error{code:"depth_exceeded"}`.
- Each role declares `can_spawn` (default true). A role with
  `can_spawn: false` cannot call `spawn_subagent` at all.

## Working with your parent

The parent passed you a `task` when it spawned you (your initial
user message) and optionally a JSON `inputs` blob (available to your
host runtime). Treat both as authoritative: the parent will not
revise the task mid-run unless it cancels and re-spawns.

When you finish, the final assistant message you produce becomes the
`result` field your parent sees in `wait_subagents`. Keep it tight
and structured — the parent will route it directly into its own
reasoning, not display it to the end user verbatim.

## What this skill does NOT grant

- `session:whiteboard_init` / `session:whiteboard_stop` — the
  whiteboard host role is reserved for sub-agents that themselves
  spawn deeper children. The `_whiteboard` skill (when it loads)
  upgrades you to the host surface.
- `plan:*` — the planner surface lands as a separate skill
  (`_planner`) you require explicitly when planning is part of your
  workflow.
