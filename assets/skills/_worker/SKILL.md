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
  # Phase 4.2.3 — workers read prior cross-mission findings but
  # do not write by default. Roles that need write capability can
  # extend allowed-tools at the role level (sub_agents block in
  # the dispatching skill).
  - provider: notepad
    tools:
      - read
      - search
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [worker]
    tier_compatibility: [worker]
    # Phase 4.2.3 ε — worker close turn. Same shape as the
    # mission tier (default prompt + [notepad:append] surface
    # + 2-iter cap), but the per-role on_close on the
    # dispatching skill (analyst's schema-explorer /
    # query-builder / data-analyst) generally provides a more
    # specific prompt. Skip the close turn for idle workers
    # (simple-answerer that returned text without tools).
    mission:
      on_close:
        notepad:
          skip_if_idle: true
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

## Doing your task — read the manual first

Your mission passed you a `task` (the user message you boot with)
and validated `inputs`. Domain skills (`hugr-data`,
`python-runner`, `duckdb-data`, `duckdb-docs`) are NOT autoloaded
into worker sessions — you load them on demand.

**Mandatory boot sequence for any task that needs a domain skill:**

0. **`notepad:search(query=<key concept from your task>)`** — if
   your task references a concept the conversation has been
   discussing (a table name, data source, user preference,
   recurring query pattern), check the session notepad first.
   Prior missions may have already surfaced what you need; reuse
   beats re-deriving. Skip when the task is genuinely fresh
   ground.

1. **`skill:load("<skill-name>")`** — pulls the skill's tool surface
   into your session. Once loaded, the tools appear in your next
   turn's snapshot.

2. **`skill:files(name="<skill-name>", subdir="references")`** —
   list the reference documents the skill ships. Each domain skill
   ships a small library of `references/*.md` (schema patterns,
   syntax cheatsheets, gotchas) curated by humans for the model.

3. **`skill:ref(skill="<skill-name>", ref="<base-name>")`** — read
   the reference relevant to your task BEFORE any data tool call.
   For example, for `hugr-data` work, the typical first reads are
   `start`, `overview`, and `query-patterns` (or
   `queries-deep-dive` for complex GraphQL). Do NOT compose
   queries from memory — the runtime's GraphQL flavour has
   skill-specific syntax the reference covers.

4. Now make your domain calls (`hugr-main:*`, `python-mcp:*`,
   `duckdb-mcp:*`). Use what the reference taught you.

Skipping the reference-read step is the single biggest cause of
malformed queries on weak models. Read the manual first, then act.

If you don't see a tool you expect AFTER skill:load, check
`skill:tools_catalog` to confirm the skill is loaded + what tools
it admits.

## Returning

When you finish:

1. Call `whiteboard:write` once with a tight finding your mission
   and siblings can consume — schema names, row counts,
   surprising patterns. Do NOT spam — one significant message
   per worker is the cadence; siblings see every write.
2. Return your final result as a normal assistant message — the
   mission consumes it via `wait_subagents`. Quote actual numbers
   from your tool responses; never paraphrase.

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
