---
name: analyst
description: >
  Mission-PDCA coordinator for data work. Iterative planner-driven
  waves over Hugr Data Mesh; checker verdict routing; synthesizer
  produces the final report. Workers cover schema discovery, query
  composition, query execution + post-processing, and report
  assembly.
license: Apache-2.0
allowed-tools: []
metadata:
  hugen:
    requires_skills: []
    autoload: false
    tier_compatibility: [mission, worker]

    # Analyst-domain notepad categories. Universal chat / mission
    # categories (user-preference, deferred-question) come from the
    # autoloaded system skills; the skill extension de-dupes by
    # name across every loaded skill.
    notepad:
      tags:
        - name: data-source
          hint: 'Source / module inventory facts — which Hugr module holds data for domain X, what each registered source tracks, source ↔ module map. Distinct from `schema-finding`: data-source answers "where to look", schema-finding answers "what is inside".'
        - name: schema-finding
          hint: Discovered table structures, field semantics, soft-delete columns, naming conventions — facts INSIDE a chosen module. Source / module identity goes to `data-source`.
        - name: query-pattern
          hint: A validated SQL/GraphQL template (shape only) that produced useful results — reuse before re-deriving.
        - name: data-quality-issue
          hint: Anomalies, nulls, suspicious cardinalities observed during exploration.

    mission:
      summary: >
        Cross-table data analysis: joins, group-by, aggregations,
        comparisons across modules, dashboards, comprehensive reports.
        Use only when the request needs combining or summarising data
        from many entities. Single-source lists, counts, lookups, or
        "save this result as a file" requests should stay in chat;
        chat can load this skill's worker-tier tools directly without
        spawning a mission.
      keywords:
        - analyse
        - dashboard
        - report
        - chart
        - aggregate
        - group
        - join
        - compare
        - audit
        - investigate

      # Mission-tier surfaces analyst opts into. Plan context fed
      # iteration-over-iteration so the planner / checker /
      # synthesizer all see the journal. Notepad spans the root
      # tree for cross-mission findings.
      capabilities:
        notepad: true
        plan_context: true

      # Planner-driven PDCA loop. Runtime spawns the `planner` role
      # every iteration with [Plan context] + [Recent verdict] +
      # [Recent waves] sections; planner emits a kind=plan handoff
      # with the next wave (or plan_complete to exit).
      plan:
        role: planner
        max_waves: 6
        approval:
          initial: required
          iteration: initial-only

      # Checker reads each wave's handoffs and emits a kind=verdict
      # decision (continue | amend | inquire | finish). Runtime
      # routes; supervisor does not run a turn.
      control:
        role: checker

      # Synthesizer turns the accumulated handoffs + plan_context
      # into the mission's final user-facing answer. Runs once,
      # after `plan_complete` / `verdict: finish`.
      synthesis:
        role: synthesizer

    sub_agents:
      - name: planner
        description: >
          Mission planner. Each iteration receives [Plan context]
          (memory_summary entries from prior waves), [Recent waves]
          (statuses), and [Recent verdict] (when set). Job: emit
          exactly one fenced `handoff` block with kind=plan
          describing the next wave OR signal plan_complete by
          setting `next_wave: null`.

          Iteration 1 — secure user approval via session:inquire
          before returning the handoff (the spawn-applier injects
          [approval_required: initial] into your first message;
          mission ext validates the inquiry happened). Later
          iterations close silently.

          Boot order:
            1. notepad:search for known data-source / schema-finding
               notes that may skip an entire discovery wave.
            2. discovery-* / schema-* to pin down the data surface
               BEFORE drafting the wave shape. Resolve sources,
               modules, tables, columns; every Do worker
               (schema-explorer / query-builder / data-analyst)
               depends on these facts in its task brief.

          Wave shape inside `next_wave`:
            label:     "<short kebab tag>"
            subagents: [{ name, role, task, depends_on?, inputs? }]
          Group independent subagents into ONE wave; they run in
          parallel. Sequential waves are valid only when wave-K
          genuinely depends on wave-(K-1) handoffs. Roles available
          for Do work: schema-explorer / query-builder /
          data-analyst / report-builder.

          Hard rules:
            - NO data execution from inside planner. discovery-* +
              schema-* only.
            - `memory_summary` field on your handoff is REQUIRED —
              one line describing what this iteration learned. The
              runtime auto-extracts it into the plan_context
              journal so the next iteration sees the digest.
            - Single fenced block only; no narration before /
              after.
        intent: tool_calling
        can_spawn: false
        autoload_skills: [hugr-data]
        capabilities:
          plan_context: read
        tools:
          - provider: hugr-data
            tools:
              - discovery-search_data_sources
              - discovery-search_modules
              - discovery-search_module_data_objects
              - discovery-search_module_functions
              - discovery-field_values
              - schema-type_info
              - schema-type_fields
              - schema-enum_values
          - provider: notepad
            tools: [read, search]
          - provider: mission
            tools: [get_handoff]

      - name: schema-explorer
        description: >
          Discovers Hugr schema structure for one module / entity
          — types, fields, relationships, edge cases. ONLY uses
          discovery-* / schema-* tools; never executes data
          queries. Produces a tight schema-map handoff for
          downstream query-builder / data-analyst.

          On wide tables (100+ columns), the default schema-type_
          fields call returns the first 50 fields alphabetically.
          When looking for a field by meaning, retry with
          `relevance_query: "<NL phrase>"` and
          `include_description: true`.

          CATEGORY-shaped tasks ("payment types", "customer
          tables") differ from single-entity tasks. When the task
          names a domain CATEGORY, enumerate ALL matching tables
          via discovery-search_module_data_objects and emit ONE
          handoff per discovered table.
        intent: tool_calling
        can_spawn: false
        autoload_skills: [hugr-data]
        tools:
          - provider: hugr-data
            tools: ['*']
          - provider: mission
            tools: [get_handoff]
        on_close:
          notepad:
            prompt: |
              You're a schema-explorer wrapping up. For each
              NON-OBVIOUS schema fact a future mission would have
              to re-discover (soft-delete columns, status enums,
              status enum values, FK shapes, naming conventions),
              call `notepad:append` once with
              `category: "schema-finding"` and a one-line
              `content` phrased as an observation (e.g.
              `orders.deleted_at appears to mark soft-deletes`).
              Skip obvious facts. If nothing surprising was
              found, reply "done" without tool calls.
            skip_if_idle: true
            max_turns: 3

      - name: query-builder
        description: >
          Composes ONE focused GraphQL query against the schema
          info delivered in the task brief (resolved by the
          planner / lifted from depends_on handoffs). Validates
          syntax by running it once (small limit / count-only)
          and fixes any errors before emitting the handoff.

          Handoff body shape: `{ graphql: <query string>,
          sample_row: <one row>, memory_summary: <one line> }`.
        intent: tool_calling
        can_spawn: false
        autoload_skills: [hugr-data]
        tools:
          - provider: hugr-data
            tools: ['*']
          - provider: mission
            tools: [get_handoff]
        on_close:
          notepad:
            prompt: |
              You're a query-builder wrapping up. The query you
              validated is in your handoff; its SHAPE (which
              module, which fields, which filters) is reusable.
              Call `notepad:append` once with
              `category: "query-pattern"` and a one-line
              `content` describing the shape — NOT the result
              values (those go stale). Skip if the query was a
              trivial one-off. Then reply "done".
            skip_if_idle: true

      - name: data-analyst
        description: >
          Executes validated queries against real data and runs
          post-processing: Hugr GraphQL (via hugr-data + hugr-
          query), local DuckDB SQL (via duckdb-data), Python
          (via python-runner). Reuses the query proven by
          query-builder; focuses on results, not schema.

          Every fetched dataset MUST land on disk via
          `hugr-query:query` with a `path:` argument (parquet
          for tabular, JSON for scalar). `data-inline_graphql_
          result` is reserved for in-session probing; never the
          terminal fetch handed off to the synthesizer.

          Handoff body shape: `{ path, shape, columns: [...],
          headline, memory_summary }`. Emit one handoff entry
          per file produced.
        intent: tool_calling
        can_spawn: false
        autoload_skills: [hugr-data, duckdb-data, python-runner]
        tools:
          - provider: hugr-data
            tools: ['*']
          - provider: duckdb-data
            tools: ['*']
          - provider: python-runner
            tools: ['*']
          - provider: mission
            tools: [get_handoff]

      - name: report-builder
        description: >
          Synthesises prior-wave artefacts into a user-facing
          output (HTML / markdown / prose). Reads files referenced
          in `depends_on` handoffs, runs python for charts /
          tables / formatting, writes the result.

          Trust order: `[Resolved depends_on]` artefact bodies
          first (mission ext auto-injects them), then any
          remaining handoff catalog entries the planner
          surfaced. Never re-discover schema; never run new data
          queries — if a fact is missing, return a
          `status: "error"` handoff with `reason: "missing: <what>"`
          and let the checker route to amend.

          File path discipline: `inputs.file_path` is the LITERAL
          destination. Honour it via `open(os.path.expanduser(p),
          'w')`. NEVER silently substitute a different path and
          claim the requested one in the handoff body.

          Number formatting in user-facing output: humanise large
          magnitudes (`3.32B`, `1.5M`, `127K`). No scientific
          notation. Round floats to 2 decimals before suffix.
        intent: default
        can_spawn: false
        autoload_skills: [python-runner]
        tools:
          - provider: python-runner
            tools: ['*']
          - provider: mission
            tools: [get_handoff]

      - name: checker
        description: >
          Verdict-emitting role spawned after each non-planner
          wave. Reads [Handoffs to check] in your first message
          (the runtime injects every handoff produced by the
          just-completed wave) plus [Plan context] (the journal
          of prior iterations) and emits exactly one fenced
          `handoff` block with kind=verdict.

          Decision enum:
            - continue → wave outputs are sufficient; let the
              planner proceed to the next iteration.
            - amend    → wave produced something but it's
              incomplete or wrong. Provide `issues: [<one-line
              per issue>]`; the planner sees these in [Recent
              verdict] on its next spawn and replans.
            - inquire  → ambiguity requires user input. Call
              session:inquire from inside your turn BEFORE emitting
              the handoff (the runtime validates the inquiry
              happened when decision=inquire).
            - finish   → mission goal is met; route to synthesis
              immediately, no further iterations.

          Be terse. `reason` is a one-line explanation of the
          decision; `memory_summary` is one line for the journal.
          Single fenced block only.
        intent: cheap
        can_spawn: false
        capabilities:
          plan_context: read
        tools:
          - provider: mission
            tools: [get_handoff]

      - name: synthesizer
        description: >
          Mission's final assistant — runs ONCE after the planner
          emits `plan_complete` or the checker emits
          `decision: finish`. Reads [Plan context] (every
          iteration's memory_summary) and [Handoffs] (the
          accumulated wave outputs) and produces the user-facing
          answer that root surfaces verbatim.

          Cite concrete file paths verbatim where applicable;
          quote headline numbers. Mention any gap the checker
          flagged as `amend` but couldn't be resolved in
          remaining iterations. Keep it tight — 3-6 short
          paragraphs or a structured 3-section summary.

          Emit exactly one fenced `handoff` block with
          kind=synthesis carrying the final message in `body`.
          No narration before / after.
        intent: default
        can_spawn: false
        capabilities:
          plan_context: read
        tools:
          - provider: mission
            tools: [get_handoff]

compatibility:
  model: any
  runtime: hugen-phase-4
---

# analyst

PDCA-shaped mission skill for data analysis over Hugr Data Mesh.
Runtime drives the iteration loop; this body documents the
domain-specific contract per role.

## Lifecycle (runtime-driven)

Root spawns this skill via `session:spawn_mission`. The runtime
then:

1. Spawns the `planner` role with [Plan context], [Recent waves],
   [Recent verdict] sections in its first message. Planner emits a
   kind=plan handoff (next wave) OR `next_wave: null` to signal
   `plan_complete`.
2. Approval gate: iteration-1 planner calls `session:inquire`; the
   bubble routes through this mission to root → user. Subsequent
   iterations close silently per the manifest's `approval:
   initial-only` policy.
3. Executes the planner-emitted wave (Do workers in parallel).
4. Spawns the `checker` role with [Handoffs to check] + [Plan
   context]. Checker emits kind=verdict { decision }.
5. Routes on decision:
   - continue / amend → next iteration (planner re-spawns).
   - inquire           → checker raises session:inquire; runtime
                          awaits answer, then continues.
   - finish            → exits the loop, runs synthesis.
6. Spawns the `synthesizer` once; its handoff body becomes the
   mission's final assistant message root surfaces to the user.

The mission supervisor LLM never takes a turn in v1 — runtime
owns the dispatch. Do not put orchestration prose in worker
tasks: the worker_contract template gives every worker the
fenced-handoff contract; their description above adds domain
specifics.

## Handoff channels

- **Cross-wave (primary): handoffs by ref.** Every worker ends
  with a single fenced `handoff` block. The runtime stores it
  under `<name>@<wave>`; the next wave's planner / checker see
  the catalog + memory_summary entries automatically.
- **Plan context journal.** `memory_summary` field is auto-
  extracted from every handoff into a FIFO journal. Planner,
  checker, synthesizer (and any role that opts in via
  `capabilities.plan_context: read`) see the rolling digest in
  their first message.
- **Notepad** — root-tree-scoped, cross-mission. Workers write
  durable facts via `notepad:append` in their on_close turn;
  future missions read them at planner boot.

## Recording cross-mission findings

Workers' on_close prompts already cover the durable findings:
schema-explorer writes `schema-finding`, query-builder writes
`query-pattern`. Synthesizer doesn't append — its job is the
user-facing answer only. Live values (counts, sums, top-N) are
NEVER written to notepad; they go stale between missions and
the next planner re-runs the query for fresh numbers.

## When to abstain / inquire

- Planner inquires for ambiguity BEFORE drafting the wave (the
  approval gate is the canonical channel).
- Checker inquires via `decision: inquire` for mid-mission
  ambiguity that requires user judgement.
- Workers may inquire for data-level questions (e.g. which
  column when two match by meaning); runtime bubbles up.
- The mission itself does NOT inquire — runtime drives.
