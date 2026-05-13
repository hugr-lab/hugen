---
name: _general
description: >
  Universal mission-tier dispatcher for tasks that don't fit a
  domain-specific mission skill (analyst, future search, etc).
  Catch-all fallback. Spawns generic workers that load whatever
  domain skill(s) they need on demand.
license: Apache-2.0
allowed-tools: []
metadata:
  hugen:
    requires_skills: []
    autoload: false
    tier_compatibility: [mission]

    mission:
      enabled: true
      summary: >
        Catch-all mission for non-data work — shell scripting,
        text manipulation, generic computation, ad-hoc tasks
        that don't match a specific domain mission. Use when
        the request does NOT involve Hugr / DuckDB / Python
        data analysis (those go through `analyst`).
      keywords:
        - script
        - bash
        - shell
        - text
        - format
        - file
        - generic
        - misc
        - other
        - general

      on_start:
        plan:
          body_template: |
            # {{ .UserGoal }}
            1. Explore — identify which skill(s) the worker needs
            2. Execute — fan out worker(s) to do the actual work
            3. Synthesize — package result for root
          current_step: Explore
        whiteboard:
          init: true
        # Phase 4.2.3 — _general advertises one extra category
        # (tool-result) on top of the universal set carried by
        # `_mission` (user-preference, deferred-question).
        notepad:
          tags:
            - name: tool-result
              hint: A useful tool output worth recalling rather than re-running — env values, file paths, stable identifiers. Skip outputs that change every invocation.
        first_message:
          template: |
            User goal (delegated by root): {{ .UserGoal }}

            You are running the `_general` mission — the
            catch-all fallback for tasks that don't fit a
            specific domain mission (data analysis goes through
            `analyst`; this skill is for everything else).

            ══════════════════════════════════════════════════════
            PRE-FLIGHT CHECKLIST — do this BEFORE the first
            spawn_wave.

            ☐ A. COUNT independent angles in the user goal.
              An "angle" = one sub-task that can be done without
              waiting on another. Most `_general` tasks are
              N=1 (one focused thing); use N>1 only when the
              goal clearly enumerates multiple independent
              sub-tasks.

            ☐ B. PUBLISH N to the plan via `plan:comment`
              ("Wave 1 decomposition: N=<n>, angles=[...]").

            ☐ C. WAVE 1 = ONE spawn_wave call with N entries.
              Multiple entries run IN PARALLEL. Do not chain
              sequential spawn_wave calls to fake parallelism.

            ☐ D. Sequential waves ONLY for true dependencies
              (execute → synthesize, etc).
            ══════════════════════════════════════════════════════

            Role catalogue (this skill):

              - `generic-worker` — free-form. Loads any
                worker-tier domain skill on demand
                (`bash-mcp`, `python-runner`, etc.) and runs
                whatever the task needs.

            Every `session:spawn_wave` entry sets
            `skill: "_general"` and `role: "generic-worker"`.
            Do NOT pass `skill: "_worker"` — that's a runtime
            primitive.

            Concrete call shape:

              session:spawn_wave({
                wave_label: "execute",
                subagents: [
                  {
                    skill: "_general",
                    role:  "generic-worker",
                    task:  "Load <skill-name>; skill:ref(skill=\"<skill-name>\", ref=\"<base>\") to read its references. Then <concrete instruction>. Write a structured result to the whiteboard."
                  }
                ]
              })

            After the wave: `whiteboard:read` to gather findings,
            `plan:comment` to log progress, decide whether to
            launch a synthesis wave or finalise directly. For
            simple single-step tasks, one wave + the mission's
            own final message is enough — no synthesis wave
            needed.

    sub_agents:
      - name: generic-worker
        description: >
          Free-form worker. Loads any worker-tier domain skill
          on demand (bash-mcp, python-runner, future
          web-search, etc.), reads its references per the
          worker boot sequence, and executes whatever the
          mission's task directive says. The mission's job is
          decomposition; the worker's job is to pick the right
          skill and run it end-to-end.
        intent: tool_calling
        can_spawn: false
        tools:
          - provider: whiteboard
            tools: [write, read]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _general

The `_general` mission is a catch-all for requests that don't fit
a domain-specific mission skill. Root delegates here when nothing
more specific applies — for example shell scripting, text
transformation, file manipulation, or any one-off compute the
user wants done.

## When to use this vs. `analyst`

Choose `_general` when the goal does NOT involve data exploration,
schema work, SQL/GraphQL queries, aggregations, or charts. Those
go through `analyst`, which has the staged pipeline and the
domain role catalogue tuned for Hugr data work.

Choose `_general` when the user wants something like:

- "Format this text into a table."
- "Compute X from these inputs."
- "Run a shell command and tell me what it printed."
- "Write a small script that does Y."

If you cannot tell, prefer `analyst` — it covers more of the
common path. `_general` is an explicit "not that" fallback.

## Pattern

Same wave-based loop as every mission skill:

1. Decompose the goal (PRE-FLIGHT CHECKLIST in `on_start`).
2. `spawn_wave` with N parallel `generic-worker` entries.
3. Each worker loads the skill(s) it needs via `skill:load(...)`,
   reads references via `skill:ref(...)`, then executes.
4. Read the whiteboard, optionally spawn a synthesis wave.
5. Return a tight final answer to root.

## Worker boot sequence

Per `tier-worker` constitution, every `generic-worker` MUST:

1. Receive the mission's `task` directive (must name a skill +
   concrete goal).
2. `skill:load(<skill-name>)` to bring the toolset.
3. `skill:files(name="<skill>", subdir="references")` if the
   skill ships references.
4. `skill:ref(skill="<skill>", ref="...")` to read the manual.
5. Then call tools.

Skipping this sequence is the #1 cause of workers hallucinating
tool calls. Write the boot steps into the `task` string
explicitly when you spawn — the worker reads its task as a
user message and follows it literally.

## Returning to root

Your final assistant message becomes the `result` field root
sees via `wait_subagents`. Keep it tight and structured. If
your output has facts worth remembering across user turns,
include them as a `notes:` JSON field so root's notepad gets
seeded.
