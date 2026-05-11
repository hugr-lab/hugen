---
name: _worker
description: Built-in skill granting a worker-tier session minimal context-reading + whiteboard surface; no spawn by default.
license: Apache-2.0
allowed-tools:
  - provider: session
    tools:
      - parent_context
  - provider: whiteboard
    tools:
      - write
      - read
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [worker]
    tier_compatibility: [worker]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _worker skill

The _worker skill is autoloaded into every worker-tier session —
depth ≥ 2 leaves a mission spawned to do focused domain work.
Worker is a leaf executor: it does the actual data work, writes a
finding to the whiteboard, and returns a result to its mission.

The autoloaded surface is minimal:

- `session:parent_context` — read your mission's (parent's)
  user-facing conversation so you can resolve ambiguity in the
  task you were spawned with. Filtered to user and assistant
  messages only.
- `whiteboard:write` — broadcast one structured finding to your
  siblings and your mission. Caps: 4 KB per message, fan-out is
  global within the mission's whiteboard.
- `whiteboard:read` — read the current whiteboard projection.
  Useful when sibling findings inform what you should look at next.

## Doing your task

Your mission passed you a `task` (the user message you boot with)
and validated `inputs`. Do the work using the skill your mission
chose for your role (e.g. `hugr-data`, `python-runner`,
`duckdb-data`) — those skills autoload only at the worker tier and
provide the domain tools you need. If you don't see a tool you
expect, check `skill:tools_catalog` and load the skill explicitly.

When you finish:

1. Call `whiteboard:write` once with a tight finding your mission
   and siblings can consume. Do NOT spam — one significant message
   per worker is the cadence; siblings see every write.
2. Return your final result as a normal assistant message — the
   mission consumes it via `wait_subagents`.

## What this skill does NOT grant

- `session:spawn_subagent` and the rest of the spawn surface. By
  default workers do not fan out further; they are leaves. A role
  that explicitly declares `can_spawn: true` and grants
  `session:spawn_*` in its `tools:` block opts back in — that's
  the contract.
- `plan:*` — workers do not own the plan. Read it via parent
  context if you need to.
- `whiteboard:init` / `whiteboard:stop` — the host of the
  whiteboard is your mission; workers participate, they don't open
  the channel.
