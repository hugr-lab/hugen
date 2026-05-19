---
name: _worker
description: Built-in skill granting a worker-tier session minimal context-reading surface; no spawn by default. Workers run under mission-PDCA — they emit a fenced handoff block as their terminal message.
license: Apache-2.0
allowed-tools:
  - provider: session
    tools:
      # Phase 5.1 follow-up — workers may inquire when the
      # AMBIGUITY IS IN THE DATA (e.g. two equally-plausible
      # source tables / columns for the user's request). The
      # runtime approval gate already routes worker tool-call
      # approvals through the same cascade; granting clarification
      # symmetrically closes that gap. Mission still owns
      # intent-ambiguity ("which goal did you mean") — see
      # tier-worker.md for the rule.
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
    # mission tier (default prompt + [notepad:append] surface
    # + 2-iter cap), but the per-role on_close on the
    # dispatching skill generally provides a more specific
    # prompt. Skip the close turn for idle workers that
    # returned text without tools.
    mission:
      on_close:
        notepad:
          skip_if_idle: true
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _worker skill

The _worker skill is autoloaded into every worker-tier session
(depth ≥ 2 leaves a mission's planner included in a wave). Worker
is a leaf executor: it does the actual data work, emits a single
fenced `handoff` block as its terminal message, and the mission
ext stores it under `<name>@<wave>` for the next wave / checker /
synthesizer to consume.

The autoloaded surface is minimal:

- `session:inquire` — ambiguity in the data only; intent
  ambiguity belongs to the planner (return a `status: "error"`
  handoff instead).
- `notepad:read` / `notepad:search` — cross-mission findings
  (schema-finding / data-source / query-pattern / data-quality-
  issue). Always check before re-discovering.
- `mission:get_handoff(ref)` — fetch a prior-wave handoff body
  by ref. Refs are discoverable through `[Resolved depends_on]`
  (bytes auto-injected) and `[Available handoffs]` (catalog) in
  your first message; never invent ref names.

## The handoff contract — runtime-injected

The mission ext appends a `[Handoff contract]` block to your
first message. It tells you the EXACT fenced-block shape the
runtime expects as your terminal output:

```handoff
{"status":"ok","body":"<short result text>","memory_summary":"<one line>"}
```

The triple-backticks + the `handoff` word are mandatory; the JSON
inside must parse. ANY narration / reasoning / tool-call recap
before or after the fenced block is discarded by the runtime —
the fence is your ONLY return channel. The mission's checker
reads your `body`; the synthesizer reads accumulated bodies + the
plan_context journal; future planner iterations see the
`memory_summary` in `[Plan context]`.

If the task can't be completed, emit the error shape from the
contract:

```handoff
{"status":"error","reason":"<one-sentence reason>","memory_summary":"<one line>"}
```

The checker reads `reason` and routes the planner to amend the
next wave.

## Reading your first message

The mission's planner composes your task. The runtime then layers
the standard sections on top:

```
[Resolved depends_on]      ← bytes auto-injected for refs in your depends_on
  <ref1 (role: ..., status: ok)>
    <handoff body verbatim>
  <ref2 ...>

[Plan context]             ← present iff your role opts in via
  ...                          capabilities.plan_context: read

[Inputs from parent]
{ "<key>": "<resolved value>", ... }

[Task]
<task prose>

[Available handoffs]       ← every ref in the mission's store, names only
  - schema-orders@schema-discovery   (schema-explorer, ok)
  - ...

[Handoff contract]         ← the fenced-block contract above
```

**Trust order: depends_on (bytes) → inputs → task → catalog
(pull on demand).** Resolved depends_on is your siblings'
validated work; quote values verbatim instead of recomputing
them. Inputs are this-worker-only briefing. The catalog at the
bottom is for late-binding edge cases — call `mission:get_handoff`
only if a ref outside your depends_on actually matters for your
task.

## Boot sequence — read the manual before acting

Your role may declare `autoload_skills` in its manifest entry. If
so, those skills are already loaded by the runtime BEFORE your
first turn — you'll see them in the `## Loaded skills` block of
your system prompt. Skip step 1 below for those. Skills not in
that block must still be loaded on demand.

**Boot sequence for any task that needs a domain skill:**

0. **`notepad:search(query=<key concept from your task>)`** — if
   your task references a concept the conversation has been
   discussing (a table name, data source, user preference,
   recurring query pattern), check the session notepad first.
   Prior missions may have already surfaced what you need; reuse
   beats re-deriving. Skip when the task is genuinely fresh
   ground.

1. **`skill:load("<skill-name>")`** — pulls the skill's tool surface
   into your session. SKIP this step for any skill already listed
   in `## Loaded skills` (your role pre-declared it via
   `autoload_skills`); calling load on an already-loaded skill is
   a wasted turn.

2. **`skill:files(name="<skill-name>", subdir="references")`** —
   list the reference documents the skill ships. Each domain skill
   ships a small library of `references/*.md` (schema patterns,
   syntax cheatsheets, gotchas) curated by humans for the model.

3. **`skill:ref(skill="<skill-name>", ref="<base-name>")`** — read
   the reference relevant to your task BEFORE any domain tool
   call. Each skill ships its own reference catalogue; what to
   read first is covered in the skill's body. Do NOT compose
   calls from memory — domain-specific syntax and gotchas live
   in the references.

4. Now make your domain calls. Use what the reference taught you.

Skipping the reference-read step is the single biggest cause of
malformed queries on weak models. Read the manual first, then act.

If you don't see a tool you expect AFTER skill:load, check
`skill:tools_catalog` to confirm the skill is loaded + what tools
it admits.

## What this skill does NOT grant

- `session:spawn_*` — workers are leaves under mission-PDCA;
  there is no fan-out tool at this tier.
- `session:parent_context` / `session:notify_subagent` /
  `session:wait_subagents` — removed under Phase H. Workers
  receive every relevant context fragment up front (resolved
  depends_on, inputs, plan_context, catalog); the legacy
  pull-side APIs are gone.
- `plan:*` — workers do not own a plan; the dispatching mission's
  planner role owns the wave shape.
- `whiteboard:*` — PDCA missions do not use the whiteboard;
  cross-wave state flows through the handoff store + plan_context
  journal.
