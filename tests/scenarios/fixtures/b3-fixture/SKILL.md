---
name: b3-fixture
description: >
  Test-only fixture for the phase-5.2 δ B3 per-role budget
  isolation scenario. Two `sub_agents` roles on deliberately
  different `max_tool_turns` overrides so the resolution chain
  surfaces each role's own budget — not the max across the
  pair, not the tier default.
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
        Test fixture: two workers in parallel, one tight on
        budget, one loose. Mission spawns one of each in a wave
        and returns their summaries.
      keywords:
        - b3-fixture
        - budget isolation test
      max_tool_turns: 10
      max_tool_turns_hard: 20
      on_start:
        plan:
          body_template: |
            # {{ .UserGoal }}
            Spawn one tight-worker + one loose-worker.
          current_step: Spawn

    sub_agents:
      - name: tight-worker
        description: >
          Performs a small scripted loop of discovery tool
          calls. Per-role per-invocation cap is set to 4; this
          worker would burn way more on a naive routing chain
          that took the max across loaded skills.
        intent: tool_calling
        can_spawn: false
        tools:
          - provider: hugr-data
            tools: ['*']
          - provider: whiteboard
            tools: [write, read]
        # The signature B3 numbers — small per_role override.
        max_tool_turns: 4
        max_tool_turns_hard: 8

      - name: loose-worker
        description: >
          Performs the same shape of work but with a generous
          budget. Per-role per-invocation cap 20, hard 40.
          Demonstrates that the tight-worker's cap does not
          leak across role boundaries.
        intent: tool_calling
        can_spawn: false
        tools:
          - provider: hugr-data
            tools: ['*']
          - provider: whiteboard
            tools: [write, read]
        max_tool_turns: 20
        max_tool_turns_hard: 40

compatibility:
  model: any
  runtime: hugen-phase-5
---

# b3-fixture (test-only)

You are the **b3-fixture** mission. The user's request is a
trivial harness check — spawn ONE wave with both workers and
return their summaries.

```
session:spawn_wave({
  wave_label: "b3-check",
  subagents: [
    {skill: "b3-fixture", role: "tight-worker",
     task: "<verbatim user goal>"},
    {skill: "b3-fixture", role: "loose-worker",
     task: "<verbatim user goal>"},
  ]
})
```

After `wait_subagents` returns, read the whiteboard and emit a
short final assistant message naming the two worker ids and
the tool-call counts they hit. The harness scores tool-call
counts directly from the event log; your final message is
informational.

# Role: tight-worker

You are a TIGHT-budget worker. Your `max_tool_turns` is 4.
Run up to 3 discovery calls (`discovery-search_data_sources`,
`discovery-search_modules`, `discovery-search_module_data_objects`
on northwind), write ONE whiteboard summary, exit. Do NOT
exceed 4 tool calls — your soft cap would force the runtime to
fire a soft warning, which is fine for the scenario but
shouldn't push you over.

Return a one-line summary: "tight-worker: ran N tool calls,
covered <module list>."

# Role: loose-worker

You are a LOOSE-budget worker. Your `max_tool_turns` is 20.
Run 6-8 discovery calls (cover more modules: northwind,
chinook, dvdrental, pagila if present), write ONE whiteboard
summary, exit. The point is to demonstrate you can use your
own budget — not the tight-worker's.

Return a one-line summary: "loose-worker: ran N tool calls,
covered <module list>."
