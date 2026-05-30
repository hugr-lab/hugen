---
name: _mission_worker
description: Mission-PDCA worker companion — adds the upstream-state read surface (mission:get_handoff, mission:get_research) plus the fenced-handoff terminal contract. Not autoloaded; mission ext attaches it explicitly via sub_agents[].autoload_skills so ad-hoc (non-mission) workers stay free of PDCA-specific prose.
license: Apache-2.0
allowed-tools:
  - provider: mission
    tools:
      # Mission-PDCA — workers fetch prior-wave handoffs by ref via
      # mission:get_handoff when their depends_on / catalog points
      # them at one. Read-only; storage is managed by the executor.
      - get_handoff
metadata:
  hugen:
    requires_skills: []
    autoload: false
    tier_compatibility: [worker]
compatibility:
  model: any
  runtime: hugen
---

# `_mission_worker` skill

Loaded by the mission extension into every worker it spawns under
its PDCA loop (declared in the dispatching skill's
`sub_agents[].autoload_skills`). Carries the upstream-state read
surface and the fenced-handoff terminal contract — the rules
specific to working inside a mission's wave loop, kept out of the
universal `_worker` skill so an ad-hoc worker (one spawned outside
a mission, e.g. by a recipe runner) doesn't inherit assumptions
that don't apply to it.

## Reading mission state — read BEFORE you discover

The mission runtime injects upstream context into your first
user message and exposes a small set of read-only tools that
surface other waves' output. Re-discovering what's already there
is the most common worker failure mode — and it inflates context
fast. Read first; discover second.

- **`[Resolved depends_on]`** — when your task brief declares
  `depends_on`, the runtime inlines the upstream-wave handoff
  bodies right into your first message under this header. Lift
  names, paths, fields, and numbers VERBATIM; never re-discover
  what's already inlined.
- **`mission:get_handoff(ref)`** — fetch a stored handoff by ref
  for upstream output the runtime did NOT inline (large bodies,
  optional, or surfaced indirectly via the `[Available handoffs]`
  catalog). Cheaper than re-running the producing wave.
- **`mission:get_research`** — when your task brief signals that
  a research stage ran (the brief mentions "the researcher
  resolved …", "scope set by research", a `[Research findings]`
  block is visible, OR a resolved input like `file_path` only the
  user could have provided), call this BEFORE doing your own
  discovery. Returns `{ available, findings, resolved_user_inputs,
  ac_proposals }`. The research stage already paid the discovery
  cost — you reuse it. Tool granted only when your role's manifest
  opts in; if you don't see it in your snapshot, the planner has
  already lifted what you need into the task brief.

The order matters: `depends_on` first (it's already in your
prompt — no tool call), then `get_research` (single cheap tool
call), then `notepad:search` (granted by `_worker`, cheap and
cross-mission), and only THEN spend tool calls on fresh discovery
against the underlying data sources.

## Returning to your mission

When you finish:

1. **Emit your final assistant message as the fenced `handoff`
   block** the `[Handoff contract]` block in your first message
   showed you. One fence, parseable JSON inside, no narration
   before or after. The mission reads only that fenced block;
   anything else in your turn is ignored. The runtime parses it
   and stores the body under `<name>@<wave>`; the mission's
   checker, planner, and synthesizer roles all read from that
   store.
2. The `memory_summary` field on your handoff is auto-extracted
   into the mission's `plan_context` journal — keep it ONE line,
   describing what this turn LEARNED (not what it produced).
3. **Quote actual numbers from tool responses verbatim** in your
   handoff body. Never paraphrase, never round, never invent.

If you can't complete the task, emit the error shape from the
contract:

```handoff
{"status":"error","reason":"<one-sentence reason>","memory_summary":"<one line>"}
```

The mission's checker will read your `reason` and route the
planner to amend the next wave.

## What this skill does NOT grant

- `mission:get_research` is exposed by the mission extension only
  when the role's manifest opts in — this skill documents it but
  does not blanket-grant it.
- The handoff store write is owned by the executor, not the
  worker — there is no `mission:put_handoff`. You emit the fenced
  block; the runtime persists it.
