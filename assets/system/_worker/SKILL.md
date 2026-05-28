---
name: _worker
description: Built-in skill granting any worker-tier session the minimal universal read surface — no spawn, no cross-worker channel, no PDCA assumptions. Mission-spawned workers layer `_mission_worker` on top via their role's `autoload_skills`; ad-hoc workers (e.g. recipe runners) get only what's here.
license: Apache-2.0
allowed-tools:
  - provider: session
    tools:
      # Phase 5.1 — workers may inquire when the AMBIGUITY IS IN
      # THE DATA (two equally-plausible source tables / columns).
      # Intent ambiguity belongs to the caller / planner.
      - inquire
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
    # mission tier; the per-role on_close on the dispatching
    # skill generally provides a more specific prompt. Skip the
    # close turn for idle workers that returned text without
    # tools.
    mission:
      on_close:
        notepad:
          skip_if_idle: true
compatibility:
  model: any
  runtime: hugen-phase-4
---

# `_worker` skill

Autoloaded into every worker-tier session. Documents the universal
worker tool surface — the things every leaf executor can rely on
regardless of who spawned it. The full operating rules — your
shape (leaf), what you must not do, the inquiry contract — live
in your tier manual (`tier-worker.md`).

Mission-spawned workers also receive `_mission_worker` (loaded by
the mission extension via the dispatching skill's
`sub_agents[].autoload_skills`). That layered skill documents the
upstream-state read surface (`mission:get_handoff`,
`mission:get_research`) and the fenced-handoff terminal contract.
Ad-hoc workers (a recipe runner under root, an external dispatch)
will not see those bits and shouldn't need them — their task is
its own contract.

## Tool surface (granted by this skill)

- `session:inquire` — narrow data-level ambiguity only. Intent
  ambiguity belongs to your caller; ad-hoc workers should report
  the ambiguity via their normal result channel, mission-spawned
  workers via a `status: "error"` handoff.
- `notepad:read` / `notepad:search` — cross-mission findings
  (`schema-finding` / `data-source` / `query-pattern` /
  `data-quality-issue`). Always check before re-discovering.

Granted by `_system` (always present): shell (`bash-mcp:*`),
skill catalogue (`skill:load` / `unload` / `ref` / `files`),
admin tools.

Lazy via `skill:load`: domain tools per the loaded skill (your
role may pre-declare loaded skills via `autoload_skills`).

## What this skill does NOT grant

- `session:spawn_*` — workers are leaves; there is no fan-out
  tool at this tier, mission-spawned or not.
- `session:parent_context` / `session:notify_subagent` /
  `session:wait_subagents` — removed under Phase H. Workers
  receive every relevant context fragment up front; the legacy
  pull-side APIs are gone.
- `plan:*` — workers do not own a shared plan; their caller
  owns wave shape (mission ext) or schedule shape (task ext).
  Workers may still use the per-session `plan:set` for their
  own scratch when the task spans many tool calls.
- `whiteboard:*` — leaf workers do not coordinate across siblings;
  cross-wave state flows through the mission's handoff store (when
  applicable) or the task's input/output channel.
