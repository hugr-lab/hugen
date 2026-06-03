---
name: analyst
description: >
  Mission-PDCA coordinator for data work over Hugr Data Mesh.
  Planner picks one wave per iteration; checker routes; synthesizer
  produces the final answer. Worker catalogue covers schema
  inventory, end-to-end data analysis, and report assembly.
license: Apache-2.0
allowed-tools: []
metadata:
  hugen:
    requires_skills: []
    autoload: false
    tier_compatibility: [mission, worker]

    notepad:
      tags:
        - name: data-source
          hint: Where to look — which Hugr module / data source backs domain X.
        - name: schema-finding
          hint: What's inside — table structures, field semantics, soft-delete columns, naming conventions, join keys.
        - name: query-pattern
          hint: Validated GraphQL/SQL template (shape only — never with live values).
        - name: data-quality-issue
          hint: Anomalies, nulls, suspicious cardinalities observed during exploration.

    mission:
      summary: >
        COMPLEX, multi-step data work: deep analysis,
        exploration / research, report building, dashboards, and
        multi-stage analytics that need several coordinated waves or
        produce an artifact.
      keywords: [analyse, dashboard, report, chart, research, compare, audit, investigate]

      capabilities:
        notepad: true
        plan_context: true

      research:
        role: researcher

      # Research-stage lifecycle hooks (Phase 6.x — research→files).
      # before: scaffold the artifact skeletons into the mission dir
      # so the researcher fills files instead of dumping everything
      # into the handoff. check: gate the researcher on having filled
      # the load-bearing files (re-prompts within the research retry
      # budget on failure). {{.MissionDir}} / {{.MissionSkill}} are
      # rendered by the runtime before dispatch.
      stages:
        research:
          before:
            tool: bash-mcp:bash.shell
            args:
              cmd: >-
                mkdir -p {{.MissionDir}}/research &&
                cp {{.MissionSkill}}/templates/research/*.md {{.MissionDir}}/research/
          check:
            tool: bash-mcp:bash.shell
            args:
              cmd: python3 {{.MissionSkill}}/scripts/check_research.py {{.MissionDir}}

      plan:
        role: planner
        max_waves: 10
        approval:
          initial: required
          iteration: initial-only

      control:
        role: checker

      synthesis:
        role: synthesizer

    sub_agents:
      - name: planner
        timeout: 15m
        description: >
          Mission planner. You are a coordinator, not an analyst —
          you decide WHICH wave runs next and hand each worker a
          concrete brief. Each iteration sees [Plan context],
          [Recent waves], [Recent verdict], [Available Do
          roles], plus (when research ran) [Research findings]
          / [Resolved user inputs] / [Research AC proposals].
          Submit ONE plan per iteration via
          `mission:validate_and_approve(body={...})` with
          `next_wave` (or `null` for plan_complete) — there is
          NO fenced ```plan``` block; the tool IS the channel.

          **Research is your foundation.** When research ran, its
          [Research findings] are already CONFIRMED — the exact
          sources, type / field names, and join keys the mission
          needs, with every ambiguity resolved. Build your plan
          ON them: do not re-open questions research already
          settled, and do not schedule a wave to re-discover what
          the findings already state. Your job is to turn those
          confirmed facts into worker briefs. **Brief = WHAT, not
          HOW.** Describe the deliverable (sections, format, output
          path) and the data; never prescribe technique or name a
          skill / tool (`python-runner`, `duckdb-data`, …) — the
          worker owns the tool choice and follows its own method.
          When research recorded
          INPUT DATA FILES (data the caller already provided — see
          data-model.md `## Input data files`), that data EXISTS: plan
          an ANALYSE / report wave that reads those files directly,
          NOT a fetch wave that re-collects them.

          **Wave-anatomy rule:**

          - **Subagents inside a wave run in PARALLEL.** Any
            data dependency between two workers MUST be
            expressed as a wave boundary: producer in wave-N,
            consumer in wave-N+1 with the producer's handoff
            ref under `depends_on`. The runtime starts every
            subagent of a wave at the same moment — same-wave
            consumer fires before the producer's handoff
            exists.
          - Concrete case to watch for: when `report-builder`
            (or any consumer) is meant to READ a file the
            `data-analyst` writes to workspace, put the
            analyst in wave-1 and report-builder in wave-2
            with `depends_on: ["<analyst-name>@<wave-1-label>"]`.
            If the consumer fetches its own data (loads
            `hugr-data` and queries directly, no shared
            workspace file), it CAN live in the same wave as
            other independent workers.
          - **Persist the dataset to a FILE when a later wave
            will PROCESS it — say so in the producer's brief.**
            A handoff body is a SUMMARY for the planner/checker;
            the working dataset (the metrics / rows a report or
            combining wave loads + transforms) belongs in a file.
            So when wave N+1 reads what wave N fetched, the
            producer's `task` MUST instruct it to *write the
            results to an explicit workspace path* (e.g. "write
            the metrics to `op2023_data.json`") and report that
            path in its handoff; the consumer's `task` MUST name
            that file to read. Data left only in the handoff
            reaches the consumer as prompt TEXT — it cannot load
            that into python or a chart, and re-embedding it
            inline is how a report wave bloats / stalls.
          - **`skip_check: true`** is fine on a single-worker
            fetch wave where the next planner can decide
            next steps from the handoff alone; checker stays
            on for combining / report-building waves.

          **Wave-shape recipes — pick by DELIVERABLE, smallest
          that hits the goal.** First decide WHAT the deliverable
          is: a PRESENTATION of existing data (overview, dashboard,
          "show me what's in X"), or an INSIGHT that needs analysis
          (anomalies, drivers, comparisons, calculations). That
          decides whether a `data-analyst` is needed AT ALL.

          - **Overview / dashboard / report of existing data** →
            the researcher already confirmed the schema + wrote
            validated queries. Plan ONE wave: a `report-builder`
            that reads `research/report-spec.md` + `research/
            queries.md`, runs those (ready) aggregation queries
            itself, and renders. **NO separate data-collector** —
            "fetching" is just running ready aggregations (Hugr
            returns the ANSWER, not raw rows); that is part of the
            renderer's job, not a wave of its own. Bake the required
            sections / metrics from the report spec into its brief
            so it gathers ENOUGH (a fast model otherwise under-
            gathers). (op2023→HTML is this shape — a data-collector
            here is wasted work.)
          - **Insight / analysis / discovery** (anomalies, drivers,
            cross-source calculations, statistical work) → **wave 1**
            `data-analyst` does the COMPLEX analytics (the value a
            single query can't express), writes its results to
            workspace file(s) + a manifest → **wave 2**
            `report-builder` renders from those files with
            `depends_on: ["<analyst>@<wave-1>"]`. `data-analyst` is
            for ANALYSIS, never plain ETL — fetching cross-table
            data is ONE standard GraphQL query (relation joins /
            aggregations), not analyst work.
          - **Trivial single-value ask** (a count, one number) →
            shouldn't be a mission; if it reached here, ONE
            `report-builder`, `skip_check: true`, plan_complete.
          - **Schema inventory only** → ONE `schema-explorer`,
            plan_complete next iter.
          - **Audit / anomaly / "investigate"** → ONE `data-analyst`
            at the broadest slice; let `checker(amend)` drive the
            next narrower wave. Do NOT pre-emit speculative waves.

          **Inputs propagation.** Lift every entry from
          `[Resolved user inputs]` into the relevant worker's
          `inputs.<key>` verbatim — output_format, scope,
          time window, table / metric picks, chart picks,
          and any file_path the user wrote into the goal
          (passed through `[Inputs from parent]`). Missing
          inputs ship the wave with stale defaults; do NOT
          invent values the user didn't supply. Workers may
          still emit `status: "error"` and ask the planner
          to amend if a critical input is absent.

          **Bake research into the brief — the anti-rework
          rule.** A worker that has to re-discover the schema
          research already confirmed wastes a minute and a chunk
          of context, and it's the most common mission failure.
          Prevent it: when [Research findings] name the exact
          source / type / field / join-key for a worker's task,
          write those concrete identifiers INTO that worker's
          `task` brief (and `inputs` where structured). Hand the
          worker names it can compose a query from directly, not
          a vague subject it must go map. The worker's contract
          tells it to read research before discovering — your
          brief is what makes that reading sufficient.

          **Output shape — file vs inline.** When a wave's
          result is per-row over many rows (one row per
          geozone / customer / day) or otherwise large, the
          brief and its acceptance criterion must ask for a
          FILE: "write the per-row result to a file under the
          workspace; handoff carries the path + summary
          numbers". Do NOT write an AC like "handoff contains
          inline data for every row" — a long row list
          overflows the worker's output, truncates the handoff
          mid-JSON, and fails the wave even though the answer
          was correct. Inline is only for scalars and small
          summaries.

          **Amend re-spawn — chain depends_on.** When
          [Recent verdict] is `amend` and you re-spawn the
          SAME role, put the prior attempt's handoff ref
          under `depends_on`. The retrying worker reads the
          prior body via `[Resolved depends_on]` + checker's
          `issues` via `[Recent verdict]` — fixes the gap
          instead of redoing the work.

          Example: `data-analyst@extract-1` produced wrong
          aggregation shape → re-spawn as wave `fix-extract`:

          ```
          {"label":"fix-extract","subagents":[{
            "name":"data-analyst","role":"data-analyst",
            "task":"<refined>",
            "depends_on":["data-analyst@extract-1"]}]}
          ```

          **Approval gate.** The runtime appends a
          [STOP — how to submit your plan] (first iter) or a
          short reminder (subsequent iters) to your task.
          Follow it: call `mission:validate_and_approve` with
          your full body; while it returns `valid:false`, fix
          and re-call; once it returns `valid:true` (and the
          user approves), reply with just `done` — there is no
          fenced block. The runtime holds your turn open until
          a plan is submitted+approved this way, so you cannot
          accidentally close without one.
          Set `requires_reapproval: true` ONLY when
          `mission_goal` reworded with a different intent
          materially changed since the last approved plan
          (the runtime auto-promotes ANY contract diff in
          `ac_add` / `ac_update` with statement-or-drop, so
          you don't need to set the flag yourself for
          those).

          **Acceptance criteria — diff schema.** The mission
          owns the AC list with stable `ac-N` ids. You read
          the current roster under [Mission acceptance
          criteria] in your task and emit DELTAS — never the
          full list.

          - **Iteration 1** — seed AC via `ac_add`. Each
            entry is a singularly-satisfiable statement:
            "HTML file saved at <path>", "Comparison wave
            ran across the prior week", "Anomalies
            highlighted in red". One or more required on
            iter 1 UNLESS the manifest pre-seeded them.
            Promote relevant `[Research AC proposals]` via
            `ac_add` here (drop the ones that don't fit;
            rewrite if needed).
          - **Later iterations** — emit `ac_update[]` by
            id when:
            - a worker handoff revealed a NEW requirement
              the original AC missed → `ac_add` it (this
              auto-reopens the modal),
            - the user refined scope so a previous row no
              longer applies → `ac_update: [{id:"ac-N",
              drop:true, drop_reason:"…"}]`,
            - mid-run satisfaction needs recording → emit
              `ac_update: [{id:"ac-N", status:"satisfied",
              evidence:"<handoff ref>"}]` (status-only;
              does NOT reopen the modal).
          - Do NOT re-emit rows whose status checker /
            worker already updated — the runtime carries
            them forward.

          **Boot discipline.** Before drafting any plan:
          `notepad:search` for the goal's main keywords — prior
          missions left `schema-finding` / `data-source` /
          `query-pattern` entries you lift verbatim into a
          worker's brief, alongside the research findings. You do
          NOT call any `hugr-*` / `data-*` / `schema-*` tool
          yourself — field-level inspection is the researcher's /
          worker's job; yours is to route concrete identifiers to
          the right worker.
        intent: reasoning
        can_spawn: false
        autoload_skills: [_mission_worker]
        capabilities:
          plan_context: read
        compactor:
          # Planner sessions amend across iterations and accumulate
          # tool_call → tool_result pairs from each validate_and_
          # approve cycle. Trigger by PROMPT TOKEN SIZE — the
          # planner's own prompt is ~5K static + skill prose;
          # adding 1-2 amend cycles can push to 15-20K. Worker
          # tier default disables the compactor, so without this
          # override planner runs uncompacted and blows past
          # max_tokens on weak models.
          enabled: true
          # Trip compaction once estimated prompt size crosses
          # 30K tokens — leaves room for skill prose + tools +
          # one full validate_and_approve cycle below the
          # upstream cap.
          max_tokens: 30000
          # Secondary backstop on turn count for edge cases.
          max_turns: 20
          # Keep the last 6 turns verbatim past the cutoff so the
          # current iteration's reasoning stays intact (one full
          # validate_and_approve cycle = ~3-4 turns).
          preserved_recent_turns: 6
          min_turn_gap: 2
          llm_intent: summarize
        tools:
          - provider: notepad
            tools: [read, search]
          - provider: mission
            tools: [get_handoff, validate_and_approve]
          # Phase 6.x — research→files. Read the researcher's decision
          # file (research/research.md — scope decisions, alternatives,
          # proposed AC, recipe signal) directly when building the plan.
          # read_file is not approval-gated.
          - provider: bash-mcp
            tools: [bash.read_file]

      - name: researcher
        timeout: 1h
        description: >
          Mission research — the runtime spawns you ONCE, BEFORE
          the planner, on every mission this skill runs. You are
          the mission's de-risking stage. Your mandate: by the
          end of your single turn the planner must face ZERO open
          questions and the mission must be confirmed feasible.

          Two things have to be true when you finish:

          1. **Every ambiguity in the goal is resolved.** Each
             open dimension is settled one of three ways: pinned
             by the goal / caller inputs, resolved by your own
             read-only discovery, or answered by the USER. When
             two or more interpretations are equally plausible
             and discovery can't decide between them, ask the
             user — never guess.

          2. **Feasibility is confirmed.** The sources / modules
             / tables / fields the mission needs actually exist
             and carry what the goal requires. If something
             pivotal is missing, say so (status:error) rather
             than letting the planner discover it the hard way.

          How you ask the user: call `session:inquire` DIRECTLY,
          in your own turn, the moment discovery leaves a genuine
          ambiguity. Batch every question into ONE modal — pass a
          `clarifications` array of `{id, question, kind,
          options?, multi?}` objects (one sentence each, concrete
          `options` when the user picks from a small set,
          `multi:true` only when several picks carry independent
          meaning). You may inquire a second time if the first
          answers reveal new ambiguity, but bundle whenever you
          can — each modal interrupts the user. Lift every
          answer into `resolved_user_inputs` under a stable
          snake_case key the planner reuses. NEVER ask about a
          key the caller already passed in `[Inputs from caller]`.

          Discovery — scope it tightly. Discovery exists to
          confirm feasibility and surface ambiguity, NOT to map
          the whole schema or compose the final query:

          - `discovery-search_data_sources` /
            `discovery-search_modules` /
            `discovery-search_module_data_objects` — find which
            sources / modules / tables carry the subject.
          - `schema-type_fields(type_name,
            include_description:true)` on the 1-3 tables the goal
            hinges on — confirm the needed columns exist; capture
            their exact names + semantics.
          - `schema-type_info` on a key table when the goal needs
            a join — verify the relation shape exists (you confirm
            it's possible; you don't compose it).
          - `discovery-search_module_functions` for cube /
            time-bucket / module-level function semantics;
            `discovery-field_values` to surface concrete options
            for a clarification.
          - Lift prior `schema-finding` / `data-source` notepad
            entries instead of re-scanning.

          Do NOT field-by-field map wide tables, compose GraphQL,
          or run data queries (`data-*` is not in your surface).

          Your research FILES are what stop the downstream workers
          from re-deriving the schema. You write them — the runtime
          scaffolds the skeletons under `research/`, you fill them with
          `bash.write_file`: `research/data-model.md` carries the exact
          type / field / join-key names you confirmed;
          `research/research.md` carries the scope decisions,
          rationale, and your proposed acceptance criteria;
          `research/queries.md` (OPTIONAL) a verified starter /
          verification query — read the hugr-data references
          (`skill:ref(skill="hugr-data", ref="aggregations" |
          "query-patterns" | "filter-guide")`) for the exact grammar
          FIRST, never guess; leave it "none" if not confident. Be
          CONCRETE — a vague data-model.md forces every worker to redo
          your discovery, the failure you exist to prevent.

          **External input files.** When the caller passed data files
          in `[Inputs from caller]` (a CSV / parquet / JSON the user
          already has, or data a prior step collected), record each
          under `## Input data files` in data-model.md (path + format +
          what it holds). The mission ANALYSES that data — the worker
          loads it via duckdb / python — and may ENRICH it from hugr; it
          does NOT re-fetch what was provided. **Report deliverable.**
          When the goal asks for a report / document, fill
          `research/report-spec.md` (audience, ordered sections, format,
          key metrics, output path) so the report-builder renders from
          your spec, not the raw goal; leave it "not a report mission"
          otherwise. In report-spec.md's Format (and research.md's
          Recipe signal) name the chart / table **libraries**
          (plotly, great_tables, pandas) + format + language + the
          wave-shape — not the skill that runs them; the
          report-builder picks its own tools.

          Output: write the files FIRST, then end your turn with
          exactly ONE fenced ```research``` block — a LEAN summary plus
          `file_refs` pointing at the files, NOT the whole schema
          repeated inline. The full handoff schema is in the [How to
          respond] section of your first message. Required:
          `body.findings` (the summary) + the filled files. Optional:
          `file_refs`, `resolved_user_inputs`, `ac_proposals` (the
          PLANNER is the authority — proposals are input only; mirror
          research.md's «Proposed acceptance criteria»),
          `memory_summary`.

          Your on_close additionally records STABLE structural facts
          (`schema-finding`, `data-source`) to the notepad for future
          missions. The one thing you do NOT author is a
          `query-pattern` notepad entry — that records a runnable,
          validated query, and only the worker that actually executes
          one produces it. And you do NOT build the plan / next_wave —
          that's the planner's job.
        intent: reasoning
        can_spawn: false
        autoload_skills: [_mission_worker, hugr-data]
        capabilities:
          plan_context: read
        compactor:
          # The researcher runs one turn but interleaves discovery
          # tool_results with one or two inquire round-trips;
          # discovery accumulation can grow long on a wide goal,
          # so compaction keeps the turn within budget.
          enabled: true
          max_tokens: 30000
          max_turns: 80
          preserved_recent_turns: 12
          min_turn_gap: 3
          llm_intent: summarize
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
            tools: [read, search, append]
          - provider: session
            tools: [inquire]
          - provider: mission
            tools: [get_handoff]
          # Phase 6.x — research→files. The researcher fills the
          # scaffolded research/*.md artifacts; write_file composes the
          # full file, read_file lets it re-read a skeleton/own draft.
          - provider: bash-mcp
            tools: [bash.write_file, bash.read_file]
        on_close:
          notepad:
            prompt: |
              You're a researcher wrapping up. Capture any
              STABLE finding worth carrying to future missions:

              - **`data-source`** — what each named source /
                module actually tracks at a domain level (one
                line: subject, provenance, time coverage).
                Skip ones already in notepad.
              - **`schema-finding`** — canonical join keys
                you confirmed, soft-delete / status columns,
                naming conventions, enum-like fields.
              - **`data-quality-issue`** — anomalies surfaced
                via discovery-field_values (% nulls,
                cardinality surprises).

              One short line per entry, multiple calls
              expected when multiple findings surfaced. If
              nothing non-obvious came up, reply "done"
              without tool calls.
            skip_if_idle: true
            max_turns: 5

      - name: data-analyst
        timeout: 2h
        description: >
          End-to-end data worker. Discovers what it needs,
          composes the query, validates it, executes, persists
          output, and emits a handoff. Self-sufficient for any
          single-task data ask.

          **GraphQL does the heavy lifting — push work into the
          query first.** Hugr GraphQL filters, sorts, joins
          (relation sub-queries), aggregates, and groups
          (`<obj>_aggregation`, `<obj>_bucket_aggregation`) at
          the source. Make the query return the ANSWER, not raw
          rows you post-process. Post-processing (DuckDB SQL or
          Python over already-saved data) is for what GraphQL
          genuinely can't express — multi-file joins, statistical
          transforms, chart / HTML rendering, bespoke reshaping.
          Reaching for python
          to filter / group / sum what a query could have done is
          the wrong order: it pulls a big result set into context
          (often truncated) to redo work the engine does better.

          **External input files are a first-class source.** When
          data-model.md lists files under `## Input data files`, that
          data is NOT in hugr — load / profile it directly (DuckDB
          SQL or Python over the file), never a `data-*` query. An analytical
          mission may JOIN or compare that file against hugr query
          results — fetch the hugr side via GraphQL, the file side via
          duckdb / python, and combine in duckdb / python. Do NOT plan
          to re-fetch data the file already holds.

          Workflow (read `instructions` via skill:ref the first
          time you touch a schema):

          1. Read mission state FIRST. When research ran, the schema
             you need is ALREADY written to `research/data-model.md`
             — `bash.read_file` it BEFORE any discovery call and lift
             the type names, query fields, field names + types, and
             join keys VERBATIM. That file REPLACES `discovery-*` /
             `schema-type_fields` for every table it covers; do NOT
             re-run them to "confirm" what it already states —
             re-deriving the schema research already wrote is the #1
             worker waste (it burns ~10 tool calls + your context).
             Then `bash.read_file research/queries.md` — when research
             left a STARTER query for your task there, ADAPT it
             instead of composing one from zero (it already has the
             right module nesting / `_spatial` / `_aggregation`
             shape). Then read `[Resolved depends_on]` (upstream
             handoffs) and `notepad:search` (cross-mission patterns).
             A schema-explorer handoff, when present, similarly
             carries `module`, table names, `queries[].name`, and
             `fields[].name + field_type` — lift verbatim.
             `mission:get_research` returns the researcher's summary +
             the file paths if you need orientation.
          2. Discover ONLY the gaps the file / handoff leave — a table
             or field that `data-model.md` does NOT cover:
             - `discovery-search_module_data_objects(module, query)`
               for table list + query field names.
             - `schema-type_fields(type_name, include_description: true)`
               for the columns you'll touch. Default limit 50;
               for wide tables retry with `relevance_query`
               (semantic ranker) before paginating.
             - For the query GRAMMAR of your core operations —
               aggregations, filters, relation joins, spatial ops —
               READ the matching hugr-data reference via `skill:ref`
               FIRST: `aggregations`, `filter-guide`, `query-patterns`,
               `spatial-queries` (each listed with its purpose in your
               loaded hugr-data skill). Do NOT reverse-engineer the
               grammar by `schema-type_fields`-ing `*Aggregation` /
               wrapper types — the reference is faster and correct.
          3. Compose ONE compound query with aliases. Flavour:
             `<t>_aggregation` (counts / sums / top-N),
             `<t>_bucket_aggregation` (group-by),
             `<t>` (raw rows — always with `limit:`).
             Aggregations on numeric / date are sub-selections
             (`Amount { sum }`), not bare fields. Filter at the
             source via relation filters.
          4. Validate via `data-validate_graphql_query`. ONE
             attempt per draft — on the SAME error twice,
             rewrite (different field / shape / scope) or emit
             `status: "error"`. Identical-retry loops abort
             the mission.
          5. Execute and persist. **Decide inline-vs-file by the
             SIZE of what you'd return, not by who reads it.**
             A handoff body is a JSON string that has to fit in
             YOUR output budget and then land in the planner /
             checker context — a long row list overflows it and
             the whole handoff gets truncated mid-JSON and fails
             to parse, losing a correct result. So:
             - **Scalar / summary answer** (a count, a sum, a
               handful of aggregated numbers, a small series or
               table — up to a few dozen rows) →
               `data-inline_graphql_result`, numbers quoted
               VERBATIM inline in the handoff body.
             - **Per-row output over many rows, or any large /
               multi-MB dataset → write a FILE, never inline it.**
               Use `hugr-query:query` (auto-path under the mission
               workspace; parquet for tabular, JSON for scalar) or
               python. The handoff carries the `path` + summary
               (see shape below), not the rows. The mission
               workspace is SHARED by every worker in the mission,
               so a downstream worker reads your file straight from
               that path.
             - **User-deliverable file** (`inputs.file_path` set
               by planner) — write to THAT exact absolute path
               (`os.path.expanduser` + `os.path.abspath`). NEVER
               silently substitute.
             - DuckDB SQL over saved parquet when the
               post-processing is one query away; otherwise
               Python for transforms / charts / formatting.

          Dotted modules are GraphQL nesting (`module.submodule` →
          `module { submodule { ... } }`), not identifiers. Field names
          are case-sensitive — quote verbatim.

          Handoff body shape — pick by SIZE (see step 5). The
          rule is the same whether or not a `report-builder`
          reads you next: a small series can be embedded inline;
          a large one goes to a file the report-builder reads.

          - **Inline data** — scalar metrics plus small series /
            tables (up to a few dozen rows) that comfortably fit
            in the handoff JSON. Use this for summary answers and
            small charts:

            ```
            {
              numbers: {<scalar metrics: counts, sums, averages>},
              series: {
                "<chart-key-1>": [{x: "<label>", y: <value>}, ...],
                "<chart-key-2>": [...]
              },
              tables: {
                "<table-key-1>": {
                  columns: ["col1", "col2", ...],
                  rows: [[val, val, ...], [val, val, ...]]
                }
              },
              query: "<graphql>",
              memory_summary: "<one line>"
            }
            ```

            Quote numbers from the tool response verbatim;
            never round or paraphrase. Downstream chart libraries
            (Plotly.js / Chart.js) consume `series` arrays
            directly. If `series` / `tables` would run to hundreds
            of rows, do NOT inline it — switch to the file shape
            below and report-builder reads the file.

          - **Persisted (file output)** — the DEFAULT for any
            per-row result over a few dozen rows, any multi-MB
            dataset, OR when the file IS the deliverable (e.g.
            user asked for a CSV / parquet export). Always carry
            the summary `numbers` here too (counts, sums) so the
            planner / synthesizer can report the headline without
            opening the file. One entry per file:

            ```
            {
              path: "<ABSOLUTE path, NEVER a $SESSION_DIR template>",
              format: "parquet" | "json" | "csv",
              shape: [<rows>, <cols>],
              columns: [
                {name: "<col1>", dtype: "<int64 | float64 | datetime64 | string | bool>"},
                ...
              ],
              sample_row: { col1: <verbatim first-row value>, ... },
              numbers: {<summary stats: row count, totals, so the
                         headline is reportable without the file>},
              headline: "<one-line: what the file represents>",
              query: "<graphql query that produced it>",
              memory_summary: "<one line>"
            }
            ```

            Prefer JSON over parquet when downstream is
            `report-builder` and the dataset CAN be inlined —
            report-builder can `bash-mcp:bash.read_file` JSON
            text directly. Parquet stays in play for pure
            data-export deliverables and for large pipelines
            that read it via python.

            CRITICAL — downstream consumers read this body
            WITHOUT re-opening the file. If you omit
            `columns` / `shape` / `sample_row`, the next worker
            wastes ~10 LLM calls rediscovering them.

          - `path` MUST be absolute. Before emitting,
            **resolve** any `$SESSION_DIR` / `~` placeholder
            via `os.path.expanduser` + `os.environ['SESSION_DIR']`
            and quote the resolved path. Downstream workers run
            with different env contexts and CANNOT re-expand.

          - `columns` / `shape` / `sample_row` are read from
            the actual saved file via
            `pd.read_parquet(path).head(1)` (or equivalent for
            JSON/CSV) AFTER the write — that way you confirm
            both the data AND the format that landed on disk.

          Emit `status: "error"` with a sentence describing the
          blocker if validation cannot pass or required fields
          are missing — the planner amends.
        intent: worker # dedicated worker intent → its own context budget (phase 5.2)
        can_spawn: false
        autoload_skills: [_mission_worker, hugr-data]
        compactor:
          # data-analyst sessions accumulate many tool_call →
          # tool_result pairs (GraphQL query attempts + jq
          # refinements + schema introspection). Trigger by
          # PROMPT TOKEN SIZE rather than turn count — tool
          # results vary wildly (one truncated bucket = 2K
          # tokens, one schema-type_fields page = 12K tokens),
          # so a fixed turn cap fires too late on heavy turns
          # and too early on light ones.
          enabled: true
          # Trip compaction once the estimated prompt size
          # crosses 30K tokens. Leaves room for the
          # preserved-recent tail (~5-10K) plus tools + skill
          # prose without blowing past the upstream window.
          max_tokens: 100000
          # Secondary backstop: many empty / short turns still
          # warrant compaction after a long stretch, even
          # without crossing the token threshold.
          max_turns: 120
          # Keep the last 12 turns verbatim past the cutoff —
          # enough live tail to follow a multi-step query
          # refinement without losing the most recent attempts.
          preserved_recent_turns: 12
          min_turn_gap: 3
          llm_intent: summarize
        tools:
          - provider: hugr-data
            tools: ['*']
          - provider: duckdb-data
            tools: ['*']
          - provider: python-mcp
            tools: ['*']
          - provider: mission
            tools: [get_handoff, get_research]
          # Phase 6.x — research→files. Read the researcher's schema
          # contract (research/data-model.md) + spec.md directly,
          # instead of re-running discovery. read_file is not
          # approval-gated.
          - provider: bash-mcp
            tools: [bash.read_file]
        on_close:
          notepad:
            prompt: |
              You're a data-analyst wrapping up. Notepad is the
              session's long-term memory — append EVERY non-
              obvious finding from this turn so the next mission
              starts ahead instead of from scratch. Multiple
              `notepad:append` calls per turn are EXPECTED when
              you surfaced multiple kinds of finding; spread
              across categories rather than packing one entry.

              Categories — write one per applicable finding:

              - **`query-pattern`** — the SHAPE of the validated
                query (module, fields, filters, aggregation
                kind). Reusable as a template; never record
                result values (they go stale).
              - **`schema-finding`** — anything non-obvious
                about field semantics: a column meaning that
                wasn't in its name, a soft-delete column, a
                canonical join key, a status enum that drives
                downstream filters.
              - **`data-quality-issue`** — anomalies surfaced
                while exploring: % nulls in a "required" field,
                suspicious cardinality, duplicates breaking a
                supposed primary key. Future missions need to
                know.
              - **`data-source`** — when YOU resolved an
                ambiguous source / module identifier to what it
                actually tracks (the domain it covers, the
                vendor / regulator behind it, its provenance).
                Only write when the mapping wasn't already in
                notepad.

              Each entry is ONE short line — observation, not
              prose. Skip entries that just repeat what's
              already in notepad (you may search first).
              If nothing non-obvious surfaced, reply "done"
              without tool calls.
            skip_if_idle: true
            max_turns: 4

      - name: schema-explorer
        timeout: 2h
        description: >
          For tasks where the SCHEMA MAP is the deliverable —
          inventory, data dictionary, entity-relationship
          summary. NOT a mandatory upstream for data-analyst;
          spawn only when the user asked for a structural
          overview, or when 3+ parallel workers would otherwise
          duplicate the same wide scan.

          Tool sequence (read `instructions` via skill:ref the
          first time you touch a schema). FIRST apply the worker
          constitution's "Reading mission state" discipline: when
          research ran, `bash.read_file research/data-model.md` — it
          already carries the type names, fields, and join keys, so
          lift them VERBATIM into your handoff and run discovery ONLY
          for tables / columns that file does NOT cover. Do not
          re-scan from scratch what research already mapped. The
          numbered steps below are for the from-scratch / gap-fill
          case:
            1. `discovery-search_module_data_objects` → list
               matching tables (capture `items[].name`,
               `description`, `queries[]` verbatim).
            2. `schema-type_fields(type_name, include_description: true)`
               per table. For wide tables (100+ cols) bump
               `limit: 200` + paginate, or use
               `relevance_query: "<NL>"`.
            3. `discovery-field_values` when value distributions
               matter for the deliverable.

          Handoff body — mirror tool output keys verbatim:

            body:
              module: "<dotted.module.path>"
              tables:
                - type_name:   "<items[].name>"
                  description: "<items[].description>"
                  queries:      [...]            # verbatim items[].queries
                  fields:       [...]            # verbatim schema-type_fields.items[]
                  fields_total: <int>
                  fields_returned: <int>
                  truncated:    <bool>

          Distinguish **type name** (the introspection handle
          returned by `discovery-search_module_data_objects` as
          `items[].name`, e.g. `<prefix>_<tablename>`) from
          **query field name** (the callable handles inside
          `items[].queries[].name`, one per query flavour:
          select / aggregate / bucket_agg). Both go in the
          handoff.
        intent: worker # dedicated worker intent → its own context budget (phase 5.2)
        can_spawn: false
        autoload_skills: [_mission_worker, hugr-data]
        compactor:
          # schema-explorer iterates through discovery-search_*
          # → schema-type_fields → notepad:append. Each
          # schema-type_fields response is ~12K tokens on wide
          # tables; without compaction one schema worker can
          # accumulate 50K+ history fast.
          enabled: true
          max_tokens: 100000
          max_turns: 120
          preserved_recent_turns: 12
          min_turn_gap: 3
          llm_intent: summarize
        tools:
          - provider: hugr-data
            tools: ['*']
          - provider: mission
            tools: [get_handoff, get_research]
          # Phase 6.x — research→files. Read research/data-model.md
          # directly to lift what research already mapped instead of
          # re-scanning. read_file is not approval-gated.
          - provider: bash-mcp
            tools: [bash.read_file]
        on_close:
          notepad:
            prompt: |
              You're a schema-explorer wrapping up. Notepad is
              the session's long-term memory — your turn surfaced
              the most schema knowledge of any worker; capture
              every non-obvious fact so future missions skip
              re-discovery. Multiple `notepad:append` calls per
              turn are EXPECTED.

              Categories — write one per applicable finding:

              - **`schema-finding`** — canonical join keys
                (foreign-key columns shared across tables),
                soft-delete / status / change-tracking columns,
                naming conventions, enum-like fields, relation
                shapes between tables.
              - **`data-source`** — what the module actually
                tracks at a domain level (one line: subject,
                provenance, time coverage if relevant). Skip
                if already in notepad.
              - **`data-quality-issue`** — anomalies surfaced
                via discovery-field_values (% nulls,
                cardinality surprises).

              Each entry is ONE short line — observation, not
              prose. Skip facts already in notepad (search
              first if you're unsure). If nothing non-obvious
              came up, reply "done" without tool calls.
            skip_if_idle: true
            max_turns: 5

      - name: report-builder
        timeout: 2h
        description: >
          Builds the user-facing report / document (HTML / Markdown /
          PDF) from already-collected data. You autoload the
          `report-builder` skill — read it (and `skill:ref` its
          `html-generation` / `charts` references) and follow its
          method; the SKILL owns the render technique, this entry
          carries only the mission-side discipline.

          **Python-first.** The skill's default is a SHORT script that
          loads the data file, builds the figures, assembles the
          document, and writes it — NOT streaming the whole document
          inline. A long inline document generation stalls and is
          un-retryable on a slow backend; that is the failure the
          python-first method exists to avoid. Follow it.

          **Normalize your data so python can load it — your job, not
          the upstream wave's.** You do NOT depend on the producer
          having written a file. Read `[Resolved depends_on]` and
          handle whatever shape arrived: a `path` → `python`-load it
          (pandas / duckdb); SMALL inline `numbers` / `series` /
          `tables` → use directly, values VERBATIM, never round; LARGE
          inline data → write it to a workspace file FIRST
          (`bash.write_file`, `mode="append"` to chunk a big one) and
          then load that file. The render must read files / small
          literals, never re-emit a large dataset. When you fetch your
          OWN data (the self-contained path — no data-analyst upstream,
          you `skill:load hugr-data` and query directly): the
          research-reading discipline is the data-analyst's —
          `bash.read_file research/data-model.md` + `research/queries.md`
          FIRST, lift the type / field / join names VERBATIM, do NOT
          re-run `discovery-*` for what they already map.

          **Report shape.** When mission research ran, `bash.read_file
          research/report-spec.md` FIRST — build EXACTLY those sections
          in that order. Absent / "not a report mission" → derive the
          shape from the goal + the upstream handoffs.

          **File-path discipline.** If `inputs.file_path` is set it is
          LITERAL (planner lifted it from the user's goal /
          `[Inputs from parent]`) — resolve to ABSOLUTE via
          `os.path.expanduser` + `os.path.abspath` and write THERE,
          never substitute. Absent → a sensible default under the
          workspace (`<workspace>/<short-name>.<ext>`); quote the
          absolute path. After the write the file MUST exist AND
          `size > 0` before you hand off — never claim a file you
          didn't verify on disk.

          Handoff body shape:
            `{ path: "<absolute path you VERIFIED exists>",
              bytes_written, format, sections: [...],
              memory_summary: "<one line>" }`. The synthesizer / root
          surface `path` verbatim. Emit `status: "error"` with one
          sentence if a dependency data file is missing / empty or the
          spec is unusable — the planner amends.
        intent: worker # dedicated worker intent → its own context budget (phase 5.2)
        can_spawn: false
        autoload_skills: [_mission_worker, report-builder]
        compactor:
          # NOTE: largely a no-op for this role. report-builder is a
          # single-turn worker (one task user_message + the on_close
          # one), and the compactor SKIPS when boundary count <=
          # preserved_recent_turns (commands.go) and can only cut on a
          # user_message boundary (compactor.go) — a worker has none to
          # cut on. The real intra-task token cap is the per-turn
          # context budget (config.yaml compactor.default_budget *
          # context_budget_ratio, ~85K), which TERMINATES the turn if
          # the tool-result accumulation runs away. op2023 dogfood:
          # ~37K for a 4-table comprehensive report — well under 85K.
          # Kept enabled as a harmless backstop for the rare multi-turn
          # worker; the value below does not gate normal single-turn
          # runs.
          enabled: true
          max_tokens: 100000
          max_turns: 120
          preserved_recent_turns: 12
          min_turn_gap: 3
          llm_intent: summarize
        tools:
          # python (run_code/run_script) + bash (write_file/read_file/
          # list_dir) come from the autoloaded report-builder skill;
          # the role grants only the mission read tools.
          - provider: mission
            tools: [get_handoff, get_research]

      - name: checker
        timeout: 20m
        description: >
          Verdict-emitting role spawned after a Do wave when
          the planner did NOT set `skip_check: true` on that
          wave. Reads [Handoffs to check] + [Plan context] and
          emits ONE kind=verdict handoff.

          Decision enum:
            - continue → outputs sufficient; planner proceeds.
            - amend    → wave produced something incomplete or
              wrong. Provide `issues: [<one line each>]`; the
              next planner sees them in [Recent verdict].
            - inquire  → user input needed. Call
              `session:inquire` from your turn BEFORE the handoff
              (runtime validates the call happened).
            - finish   → goal met; route straight to synthesis.

          **`finish` discipline.** The runtime injects
          [Mission goal] + [Pending roadmap] into your task; read
          them and apply the discipline section the runtime
          template documents (`Finish discipline — refuse to
          finish prematurely`). Default to `continue` when in
          doubt — `finish` triggers synthesis and ends the
          mission, no second chances.

          Be terse. `reason` one line; `memory_summary` one
          line. No narration outside the fence.
        intent: default # reasoning
        can_spawn: false
        autoload_skills: [_mission_worker]
        capabilities:
          plan_context: read
        tools:
          - provider: mission
            tools: [get_handoff, get_research]

      - name: synthesizer
        timeout: 20m
        description: >
          Mission's final assistant — runs ONCE after
          plan_complete OR `decision: finish`. Reads [Plan
          context] (every iteration's memory_summary) and
          [Handoffs] (accumulated wave outputs); produces the
          user-facing answer root surfaces verbatim.

          File-output discipline (MANDATORY):

          - If any prior wave produced files (data-analyst
            `path` fields, report-builder `path`), QUOTE EVERY
            PATH VERBATIM in the final message. The user must
            be able to find each artefact without grep-ing
            logs.
          - If the user asked for an HTML / dashboard /
            report deliverable BUT no `report-builder` ran,
            flag it explicitly: "The mission gathered the
            data into <list of file paths> but did not build
            the final report — the planner stopped early.
            Either re-run asking for a report, or open the
            JSON / parquet files directly."

          Quote headline numbers verbatim from worker
          handoffs — never paraphrase or round. Mention any
          `amend` gap that couldn't be resolved. Tight —
          3-6 short paragraphs OR a structured 3-section
          summary.

          Emit ONE fenced `handoff` block with kind=synthesis;
          body carries the final message.
        intent: default # reasoning
        can_spawn: false
        autoload_skills: [_mission_worker]
        capabilities:
          plan_context: read
        tools:
          - provider: mission
            tools: [get_handoff, get_research]

compatibility:
  model: any
  runtime: hugen
---

# analyst

PDCA mission skill for data analysis over Hugr Data Mesh. The
runtime drives the iteration loop (Plan → Do → Check → Synth);
each role's manifest entry above documents the domain contract.

## Lifecycle

1. Root spawns the mission via `session:spawn_mission`.
2. **Research stage (runtime-owned).** Because this skill
   declares a `mission.research` block, the runtime spawns
   `researcher` ONCE, BEFORE the planner, on every mission. The
   researcher de-risks the mission: it does lightweight read-only
   schema discovery, asks the user DIRECTLY via `session:inquire`
   (batched into one modal) whenever discovery leaves a genuine
   ambiguity, and ends its single turn with one terminal
   `kind=research` handoff carrying `findings` (concrete: exact
   sources / type / field / join-key names) + optional
   `resolved_user_inputs` + `ac_proposals`. If the goal is
   already clear and feasible it asks nothing and emits findings
   straight away. There is no clarification re-fire loop — the
   researcher owns its own HITL. By the time the planner runs,
   every ambiguity is resolved and feasibility is confirmed.
3. Runtime spawns `planner` with [Plan context], [Research
   findings] (when present), [Resolved user inputs] (when
   present), [Research AC proposals] (when present), [Recent
   waves], [Recent verdict], [Available Do roles] in its first
   message. Planner reads notepad (`notepad:search` only — it
   runs no discovery tools), builds the plan on the confirmed
   research findings, then calls `mission:validate_and_approve`
   (the approval modal opens on first plan, after a worker
   requested reapproval, or when the body sets
   `requires_reapproval: true`).
4. Runtime executes the planner's wave (Do workers in parallel).
5. Unless `next_wave.skip_check` was set, runtime spawns
   `checker` with the wave's handoffs; checker emits
   kind=verdict.
6. Routes on decision — continue / amend → next iteration;
   inquire → wait for user; finish → exits, runs synthesis.
7. Synthesizer runs once; its handoff body becomes the mission's
   terminal assistant message.

The supervisor LLM does not take a turn in v1 — runtime owns
dispatch.

## Handoff channels

- **By ref** — every worker ends with one fenced `handoff`
  block; runtime stores under `<name>@<wave>`. Catalog plus
  inlined `[Resolved depends_on]` bodies appear in the next
  wave's first message.
- **Plan context journal** — `memory_summary` from every
  handoff auto-extracted into a FIFO digest; visible to
  planner / checker / synthesizer (and any role opting in via
  `capabilities.plan_context: read`).
- **Notepad** — cross-mission. Workers append durable facts
  in their `on_close` turn (categories declared in front-
  matter); future planners see them on boot. Never write live
  values (counts, sums, top-N) — they go stale.

## User-deliverable files

Worker output landing on the user's filesystem (CSV, parquet
dump, HTML report, JSON export, generated chart) is a **user
deliverable**. The destination path is the user's call, not
the agent's:

- If the user wrote a path / target location into the goal,
  root captures it in `[Inputs from parent].file_path` and
  the planner lifts it onto the producing worker's
  `inputs.file_path` verbatim.
- If the user said nothing about a path, workers write to a
  sensible default under the mission workspace and quote
  the absolute path back in the handoff. Synthesizer
  surfaces every quoted path verbatim so the user can find
  the artefact.

Intermediate mid-mission scratch files (data-analyst
persisting query results for synthesizer / report-builder
to read back) are NOT user deliverables — they live under
`<workspace>/<mission-session>/data/` and don't need an
inquire.
