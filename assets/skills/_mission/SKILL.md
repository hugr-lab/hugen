---
name: _mission
description: Built-in skill granting a mission-tier session its coordination surface — wave-based worker fan-out, plan, whiteboard, parent context.
license: Apache-2.0
allowed-tools:
  - provider: session
    tools:
      - spawn_wave
      - subagent_runs
      - subagent_cancel
      - parent_context
  - provider: plan
    tools:
      - set
      - comment
      - show
      - clear
  - provider: whiteboard
    tools:
      - init
      - write
      - read
      - stop
  # Phase 4.2.3 — missions append working hypotheses across waves
  # and read prior session findings. `show` is root-only.
  - provider: notepad
    tools:
      - append
      - read
      - search
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [mission]
    tier_compatibility: [mission]
    # Phase 4.2.3 — universal notepad categories advertised on
    # every mission regardless of dispatcher. Domain-specific
    # tags (schema-finding, query-pattern, …) live on the
    # dispatching skill (analyst, _general). Block A's renderer
    # walks all loaded skills and de-dupes by name, so listing
    # the conversation-level categories here keeps every mission
    # consistent without each dispatcher repeating them.
    mission:
      on_start:
        notepad:
          tags:
            - name: user-preference
              hint: Stated by the user — region, currency, time zone, naming or formatting preference. Stable for the conversation.
            - name: deferred-question
              hint: An open question worth answering in a follow-up mission; deferred to keep the current task focused.
      # Phase 4.2.3 ε — deterministic close turn. Every mission
      # gets one constrained turn before SessionTerminated whose
      # only job is to persist findings to the notepad. The empty
      # `notepad: {}` block opts in with all runtime defaults:
      # AllowedTools = [notepad:append], MaxTurns = 2, prompt =
      # the runtime's built-in "review your work; record stable
      # findings" copy. SkipIfIdle: true short-circuits when the
      # mission emitted no tool calls (trivial paths like
      # simple-answerer with one cheap turn).
      on_close:
        notepad:
          skip_if_idle: true
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _mission skill

The _mission skill is autoloaded into every mission-tier session
— the depth-1 session a root spawns to coordinate one user
request. Mission is structurally a **coordinator**, not an
executor. It decomposes the goal into focused worker tasks, fans
them out one wave at a time, and synthesises their findings into
a final result for root.

The autoloaded surface is built around one primitive:

- `session:spawn_wave` — atomic spawn-and-wait fan-out. One call
  spawns N workers and blocks until each terminates, returning
  per-worker `{session_id, status, result, reason}` rows. The
  *only* fan-out primitive a mission needs — there is no separate
  `spawn` and `wait` pair to forget.
- `session:subagent_runs` — pull a worker's mid-flight transcript
  when you need to see intermediate state before deciding the
  next wave (long-running explorers, suspected drift).
- `session:subagent_cancel` — terminate a stuck worker with a
  reason. Cascades to the worker's children if it spawned any.
- `session:parent_context` — read root's user-facing conversation
  when the spawned `goal` + `inputs` aren't enough. Filtered to
  user / assistant messages only.
- `plan:set` / `plan:comment` / `plan:show` / `plan:clear` — full
  plan ownership. Set the body at boot (or have it set for you by
  the dispatching skill's `on_start` hook in phase γ); comment at
  every wave boundary.
- `whiteboard:init` / `whiteboard:write` / `whiteboard:read` /
  `whiteboard:stop` — full whiteboard ownership. Workers
  participate; the mission hosts.

## The wave pattern

Your job is **decomposition + synthesis**:

1. Read your `goal` (your first user message) and `inputs`
   (structured payload from root). They are authoritative; do
   not second-guess them. If something is genuinely ambiguous,
   read `parent_context` or call `session:abstain` (phase ζ)
   rather than guessing.

   Before composing your first wave, scan the **notepad
   snapshot** in your system prompt (the `## Notepad snapshot`
   section the runtime injects). Prior missions in this same
   conversation may have already surfaced what you need — a
   schema-finding, a validated query-pattern, a user-preference.
   If a snippet looks directly relevant, call `notepad:search`
   for full content before spawning a worker to re-derive it.
   Each saved worker is one less round of latency.
2. Init the whiteboard so all workers share findings.
3. For each wave:
   - Decide which workers run *in parallel*. Workers in the same
     wave should be independent — they all see what the previous
     wave's whiteboard writes produced, but not each other's
     concurrent writes mid-wave.
   - Call `spawn_wave({wave_label, subagents: [{skill, role, task,
     inputs}, ...]})` once. Wait phase is built in.
   - Read the whiteboard. Comment on the plan. Decide whether
     another wave is needed.
4. When you have enough to answer, produce a final assistant
   message — that becomes the `result` field root sees in its
   `wait_subagents` call. Keep it tight and structured; root
   consumes it programmatically.

## What this skill does NOT grant

- Domain data tools (hugr-*, python-*, duckdb-*, bash-*) — not
  loadable at mission tier (`tier_forbidden`). Mission
  coordinates; workers do data work. If you find yourself
  wanting to query a database directly, spawn a worker with a
  data-skill role instead.
- `session:spawn_mission` — only root delegates singularly. A
  mission spawning another mission would re-create the
  decisional shape we eliminate at the topology level.
- Raw `session:spawn_subagent` / `session:wait_subagents` — the
  `spawn_wave` primitive subsumes both. A role with explicit
  `can_spawn: true` and a `tools:` block granting them can opt
  back into the raw surface for non-wave patterns.
