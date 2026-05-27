---
name: _run_task
description: >
  Universal mission wrapper for running a task-eligible recipe
  ad-hoc. Routes one recipe spawn through a PDCA shell: optional
  input-collector clarifies missing recipe inputs, the runner wave
  is the recipe itself, the synthesizer formats the result for the
  user.
license: Apache-2.0
allowed-tools:
  - provider: mission
    tools:
      - get_handoff
      - finish
metadata:
  hugen:
    requires_skills: []
    autoload: false
    tier_compatibility: [mission, worker]

    mission:
      summary: >
        Run a task-eligible recipe ad-hoc — root passes
        `{task_skill, task_inputs}` and the mission handles input
        gathering + recipe spawn + synthesis. Use when the user
        names a recipe from the `## Available tasks` block but
        wants it now (no schedule).

      research:
        role: input-collector
        when: auto
        max_iterations: 3

      plan:
        inline:
          waves:
            - label: run
              subagents:
                - name: runner
                  skill: "{{ .Inputs.task_skill }}"
                  task: |
                    Run the recipe `{{ .Inputs.task_skill }}` once with the
                    inputs from `[Inputs from parent]`. Emit your terminal
                    handoff per the recipe's normal contract. If the recipe
                    is mission-shaped (not yet supported in this wrapper),
                    abort with status=fail and a one-line explanation in
                    memory_summary.
                  inputs_from_resolved: true

      synthesis:
        role: synthesizer

    sub_agents:
      - name: input-collector
        description: >
          Inspect the recipe `{{ .Inputs.task_skill }}` (via
          `skill:ref` / `skill:files` if needed) to learn its
          required inputs. Compare against `.SpawnInputs.task_inputs`
          — anything missing OR ambiguous, batch-inquire the user via
          your standard research-role output contract. Emit
          `resolved_user_inputs` carrying the final structured input
          set the recipe expects. Be conservative: when the recipe
          accepts a value verbatim from spawn (already present and
          unambiguous), echo it back rather than re-asking. Empty
          `clarifications` + `done: true` is the right outcome when
          everything was supplied at spawn time.

      - name: synthesizer
        description: >
          You see the recipe's terminal handoff under the
          `runner@run` ref. Render the recipe's body / artefacts /
          status as a short user-facing answer. If the recipe wrote
          to a file, name the file. If the recipe produced a number,
          surface the number with one sentence of context. Do NOT
          editorialise — the recipe owns correctness; you own
          presentation.

compatibility:
  model: any
  runtime: hugen-phase-6
---

# `_run_task` — universal ad-hoc task runner

System mission that lets the root execute any task-eligible recipe
without scheduling. Three roles, fixed plan:

1. **`input-collector`** (research stage) — figures out what the
   recipe needs and asks the user only for what's missing.
2. **`runner`** (executor wave) — the recipe itself, spawned as a
   single worker with the resolved inputs.
3. **`synthesizer`** — turns the recipe's terminal handoff into the
   user-facing answer.

## Caller contract

Root spawns this mission via:

```
session:spawn_mission(
  skill="_run_task",
  goal="Run the X recipe now",
  inputs={
    task_skill: "<recipe-name>",     # required — must be task-eligible
    task_inputs: { ... }              # optional — caller's pre-filled inputs
  },
  wait="async" (default)
)
```

The `task_skill` value is templated into the runner's `skill` field
at executor time (see `mission.plan.inline.waves[0].subagents[0].skill`).
The `task_inputs` map is the seed the input-collector compares against
the recipe's actual schema; missing keys trigger inquires.

## Universal vs. recipe-owned

This skill owns ONLY the PDCA shell. The recipe owns:

- What inputs it accepts (declared in its own manifest).
- How it executes (its `## Body` section and tool grants).
- The shape and meaning of its terminal handoff.

The wrapper is intentionally thin — adding behaviour here couples
every recipe to the wrapper. When a recipe needs richer orchestration
(multi-wave PDCA, checker stage, etc.), it should declare its own
mission and skip `_run_task` entirely.
