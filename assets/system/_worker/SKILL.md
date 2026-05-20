---
name: _worker
description: Built-in skill granting a worker-tier session minimal context-reading surface; no spawn by default. Workers run under mission-PDCA — they emit a fenced handoff block as their terminal message.
license: Apache-2.0
allowed-tools:
  - provider: session
    tools:
      # Phase 5.1 — workers may inquire when the AMBIGUITY IS IN
      # THE DATA (two equally-plausible source tables / columns).
      # Intent ambiguity still belongs to the planner.
      - inquire
  # Phase 4.2.3 — workers read prior cross-mission findings but
  # do not write by default. Roles that need write capability can
  # extend allowed-tools at the role level (sub_agents block in
  # the dispatching skill).
  - provider: notepad
    tools:
      - read
      - search
  # Mission-PDCA Phase H — workers fetch prior-wave handoffs by ref
  # via mission:get_handoff when their depends_on / catalog points
  # them at one. Read-only; storage is managed by the executor.
  - provider: mission
    tools:
      - get_handoff
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

# _worker skill

Autoloaded into every worker-tier session. The full operating
rules — fenced-handoff contract, first-message section ordering,
boot sequence, what NOT to do — live in your tier manual
(`tier-worker.md`). This skill body documents the tool surface
and worker-specific knobs.

## Tool surface (granted by this skill)

- `session:inquire` — narrow data-level ambiguity only. Intent
  ambiguity belongs to the planner; return a `status: "error"`
  handoff instead.
- `notepad:read` / `notepad:search` — cross-mission findings
  (`schema-finding` / `data-source` / `query-pattern` /
  `data-quality-issue`). Always check before re-discovering.
- `mission:get_handoff(ref)` — fetch a prior-wave handoff body
  by ref. Refs come from `[Resolved depends_on]` (bytes auto-
  injected) and `[Available handoffs]` (catalog) in your first
  message; never invent ref names.

Granted by `_system` (always present): shell (`bash-mcp:*`),
skill catalogue (`skill:load` / `unload` / `ref` / `files`),
admin tools.

Lazy via `skill:load`: domain tools per the loaded skill (your
role may pre-declare loaded skills via `autoload_skills`).

## What this skill does NOT grant

- `session:spawn_*` — workers are leaves under mission-PDCA;
  there is no fan-out tool at this tier.
- `session:parent_context` / `session:notify_subagent` /
  `session:wait_subagents` — removed under Phase H. Workers
  receive every relevant context fragment up front (resolved
  depends_on, inputs, plan_context, catalog); the legacy
  pull-side APIs are gone.
- `plan:*` — workers do not own a plan; the dispatching mission's
  planner owns the wave shape.
- `whiteboard:*` — PDCA missions do not use the whiteboard;
  cross-wave state flows through the handoff store + plan_context
  journal.
