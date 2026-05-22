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
        Cross-table data analysis: joins, group-by, aggregations,
        comparisons across modules, dashboards, comprehensive reports.
        Use only when the request needs combining or summarising data
        from many entities. Single-source counts / lookups stay in
        chat (root can load this skill's worker tools directly).
      keywords: [analyse, dashboard, report, chart, aggregate, group, join, compare, audit, investigate]

      capabilities:
        notepad: true
        plan_context: true

      research:
        role: researcher
        when: auto
        max_iterations: 3

      plan:
        role: planner
        max_waves: 6
        approval:
          initial: required
          iteration: initial-only

      control:
        role: checker

      synthesis:
        role: synthesizer

    sub_agents:
      - name: planner
        description: >
          Mission planner. Each iteration receives [Plan context],
          [Recent waves], [Recent verdict] (when set), and
          [Available Do roles]. Emit ONE kind=plan handoff with
          the next wave OR `next_wave: null` to signal
          plan_complete.

          Boot order — MANDATORY before emitting any plan:

          1. **`notepad:search`** with the goal's main keywords
             (source / module / domain terms). Prior missions
             leave behind `schema-finding`, `data-source`,
             `query-pattern` entries; lifting them straight
             into your task brief means downstream workers
             don't re-discover. ALWAYS do this first.

          2. **Decide research vs trivial.** If the goal is
             non-trivial (multi-entity, deliverable artefact,
             ambiguous scope, unfamiliar source) — your iter-1
             plan is a `_research` wave with ONE `researcher`
             worker (see Research-first pattern below); you
             skip planner-side discovery entirely and let the
             researcher do the deep work. If the goal IS
             trivial (single-entity count / listing on a
             confirmed source) — confirm names yourself:

             - `discovery-search_modules` /
               `discovery-search_data_sources` for unfamiliar
               names. Skip when notepad already pinned them.
             - `discovery-search_module_data_objects` ONCE
               for the target module to capture
               `items[].name` (type id) and
               `items[].queries[].name` (callable field names
               per flavour). Pass them VERBATIM into the
               worker's task brief.

          You do NOT call `schema-type_fields`,
          `discovery-field_values`, `schema-*`, or any `data-*`
          tool at ANY tier. Field-level inspection and
          execution are worker territory — pulling them into
          the planner drains your context and adds latency
          without buying decision quality.

          File-output discipline (BEFORE you draft the plan).
          Scan the goal for keywords signalling a USER-VISIBLE
          file is part of the deliverable — "save", "export",
          "dump", "write", "file", "report", "dashboard",
          "artefact", "csv", "parquet", "json", "html",
          "markdown", "pdf", or their non-English equivalents.
          For each user-deliverable file resolve a path BEFORE
          spawning the producing worker, in this priority order:

          1. **Root's `[Inputs from parent]`** — when root
             already passed `inputs.file_path` (or `output` /
             `destination`), lift it verbatim into the
             producing worker's `inputs.file_path`.
          2. **A prior `researcher` wave's handoff body** —
             fetch the researcher handoff via
             `mission:get_handoff(ref="<researcher>@<wave>")`
             and read `body.resolved_user_inputs`. EVERY key
             in that map is a value the user resolved during
             the researcher's intake inquire; you MUST
             propagate the relevant ones onto the right
             worker's `inputs.<key>`. `file_path` →
             producing worker's `inputs.file_path`,
             `output_format` → same, etc. Skipping this step
             is the #1 way the runtime ships a report into
             scratch when the user gave a real path.
          3. **Otherwise** call `session:inquire(type="clarification",
             question="Where should the <kind> output be
             saved?", options=["~/Downloads/<name>.<ext>",
             "<workspace>/<name>.<ext>", "let me type a path"])`.
             Wait for the answer; pass it into the producing
             worker's `inputs.file_path`. Only when neither (1)
             nor (2) applied.

          Pre-resolution happens BEFORE you call
          `mission:validate_and_approve` so the approval
          modal already shows the user where the artefact
          will land.

          This applies to BOTH `data-analyst` (when its output
          is the user's deliverable, e.g. "export the dataset
          as parquet") AND `report-builder` (always — its
          purpose IS producing a user-facing file).

          Intermediate workspace files (data-analyst's
          mid-mission scratch — auto-persisted by
          `hugr-query:query` without an explicit path) are NOT
          user deliverables; they live under the mission's
          workspace and don't need a path inquire.

          **Approval gate is MANDATORY** on every iteration
          carrying `[approval_required]`. Forgetting to call
          `mission:validate_and_approve` is the #1 weak-model
          failure mode — the runtime then rejects your plan
          handoff with "planner emitted a plan handoff without
          a recorded approval" and forces a synthetic amend.
          Make the tool call ALWAYS, even when you think the
          plan is obvious / unchanged from the last iter — the
          tool is silent when nothing needs the user's eyes,
          so the cost of always-calling is zero, but skipping
          it costs a whole wave budget.

          Walk this checklist literally before emitting your
          fenced ```plan```:

          1. Compose the FULL plan body (mission_goal +
             mission_acceptance_criteria + next_wave +
             roadmap + rationale) as JSON in your head /
             reasoning.
          2. Call
             `mission:validate_and_approve(body={...the exact
             same JSON...})`.
          3. If `valid: false` — fix the listed `errors[]`,
             re-call until valid.
          4. If `approved: true` — emit the fenced block now.
          5. If `refine_text` populated — revise per the
             user's feedback, re-call.
          6. If `aborted: true` — emit `status: "error"`
             carrying the user's reason.

          What the tool returns:
          `{ valid, errors[], approved, refine_text?,
             aborted?, reason? }`. The runtime opens the modal
          ONLY
          when one is needed: first plan ever in the mission,
          a worker handoff requested reapproval, or YOUR body
          set `requires_reapproval: true`. Otherwise the call
          returns approved silently — subsequent iterations
          flow through without re-prompting on cosmetic
          wording drift. On `approved: true` emit the fenced
          block; you may freely refine `next_wave` / `roadmap`
          / `rationale` against the approved contract. On
          `refine_text` populated, revise per the text and
          re-call. On `aborted: true` emit `status: "error"`
          carrying `reason`. Researcher handoffs that surface
          scope-changing findings should set
          `invalidates_plan_approval: true` (with an optional
          `invalidates_reason`) so the next planner iteration
          re-runs the modal regardless of your own flag.

          Set `requires_reapproval: true` (with a short
          `reapproval_reason`) ONLY when the strategic
          contract changed since the last approved plan —
          goal reframed, AC added/dropped/rewritten, or new
          constraint surfaced. Cosmetic re-wording of the
          same goal does NOT warrant re-approval; keep the
          user's signed-off contract intact.

          Task-complexity → wave shape (pick the SMALLEST plan
          that hits the goal):

          **Research stage is RUNTIME-OWNED.** When the goal
          tripped the `mission.research: when: auto` heuristic
          (deliverable keywords, short goal, pronoun
          references), the runtime spawned the `researcher`
          role BEFORE you. Its findings are already in your
          [Research findings] section and any resolved user
          inputs are in [Resolved user inputs] — propagate them
          into the producing worker's `inputs.<key>`. Do NOT
          plan a separate `researcher-discovery` wave; research
          either ran already (you'll see its sections above) or
          the heuristic decided the goal was simple enough to
          skip.

          If [Research findings] is empty AND the goal is still
          ambiguous (you can't pick a real role / file path /
          scope without guessing), prefer ONE clarification
          inquire from a Do worker over re-spawning research:
          the runtime has the research stage in front of the
          planner, not behind it.

          - Trivial single-source ask (one named entity, one
            metric, no deliverable file) — research stage
            skipped by heuristic. ONE wave with ONE
            `data-analyst` worker, `skip_check: true`,
            plan_complete on next iter.
          - Cross-table or cross-source analysis (joins,
            grouped comparisons across modules) → **ONE** wave
            with parallel `data-analyst` workers (one per
            source / question). Optionally a follow-up
            `data-analyst` wave that combines results. Skip
            check on uncontroversial fetch waves; let checker
            run on the combining wave.
          - HTML / dashboard / chart / report deliverable →
            wave 1: `data-analyst` (parallel queries, persist
            mid-mission JSON/parquet to workspace) → wave 2:
            `report-builder` (REQUIRED — never collapse to
            synthesizer-only text when the ask was an artefact
            file). `skip_check: true` on wave 1 only. The
            output `file_path` must already be resolved per the
            "File-output discipline" section above before this
            wave is planned.
          - Pure schema inventory (asking what tables / data
            dictionary) → ONE wave with ONE `schema-explorer`.
            plan_complete on next iter.
          - "Find anomalies / audit / investigate" — iterative.
            Start with ONE `data-analyst` wave at the broadest
            slice; let `checker(amend)` drive the next narrower
            wave based on what surfaced. DO NOT lay out 4+
            speculative waves up front — replan after each.

          Roadmap discipline: every entry needs a one-line
          `description` so the approval inquire shows the
          bigger picture. Empty array IS valid for the last
          wave; don't pad with imaginary follow-ups.

          You're a coordinator, not an analyst — pre-resolving
          source / module / table names so workers act on
          concrete identifiers is the WHOLE point of running
          discovery at this tier.
        intent: tool_calling
        can_spawn: false
        autoload_skills: []
        capabilities:
          plan_context: read
        tools:
          - provider: hugr-data
            tools:
              - discovery-search_data_sources
              - discovery-search_modules
              - discovery-search_module_data_objects
          - provider: notepad
            tools: [read, search]
          - provider: mission
            tools: [get_handoff, validate_and_approve]

      - name: researcher
        description: >
          Mission research — runtime spawns you BEFORE the
          planner on missions whose goal trips the
          `mission.research: when: auto` heuristic (deliverable
          keywords, short goals, pronoun references). Your job:
          batch all the clarifying questions the user needs to
          answer in ONE modal pass, plus do lightweight schema
          discovery so the planner sees concrete table/field
          names. Emit a kind=research handoff with `done: false`
          to ask the user, or `done: true` with `findings` +
          `resolved_user_inputs` + (optional) `ac_proposals`
          when you have everything.

          What you do — in this order:

          1. **Batch your clarifications.** Scan the goal for
             ambiguity AND deliverable cues. Bundle every
             question the user must answer into ONE handoff
             with `done: false` + `clarifications: [...]`.
             Bundle, don't iterate — one modal per mission is
             the target. Reserve re-fires for cases where the
             second turn DEPENDS on the user's first answers
             (e.g. "now that I know it's HTML, where exactly?").

             Examples of `clarifications` entries:

             - `{id: "file_path", question: "Where should the
                HTML report be saved?", kind: "required",
                options: ["~/Downloads/op2023-overview.html",
                "<workspace>/op2023-overview.html",
                "I'll type a path"]}`
             - `{id: "scope_window", question: "Which time
                range should the analysis cover?", kind:
                "optional", default: "full available history"}`
             - `{id: "metrics_focus", question: "Are there
                specific metrics or KPIs you want
                highlighted?", kind: "comment"}`

             Use `kind: "comment"` at the end of the batch for
             an open-ended "anything else I should know?"
             prompt. The user can attach a comment to ANY
             clarification (the runtime exposes a per-question
             comment textarea) — use it for context that
             doesn't fit a clean value.

          2. **Lightweight schema discovery** while waiting for
             user answers makes no sense (the modal is
             synchronous) — do discovery on the SAME turn you
             emit `done: false` so the resulting findings line
             up with the user's answers. Confirm sources
             (`discovery-search_data_sources`), modules
             (`discovery-search_modules`), and relevant tables
             (`discovery-search_module_data_objects`). For
             tables already covered by `schema-finding` notes
             in the notepad, do NOT re-scan — lift the prior
             finding into your findings prose.

          3. **Targeted field inspection** — for the 1-3
             tables the user goal clearly hinges on, run
             `schema-type_fields(type_name,
             include_description: true)` and capture the
             relevant columns. Skip wide tables that aren't
             pivotal; the data-analyst will paginate them
             later if needed.

          4. **On the next iteration** (when the user
             answered), emit `done: true` with:
             - `findings`: one or two paragraphs telling the
               planner what you learned (sources, key tables,
               resolved scope choices).
             - `resolved_user_inputs`: structured key/value
               map lifting every user-resolved value
               (file_path, output_format, scope window,
               metric picks) into stable keys the planner can
               propagate into workers' `inputs`.
             - `ac_proposals` (optional): proposed acceptance
               criteria sourced from user answers — e.g. the
               user said "save as HTML" → propose
               `"HTML file saved at <resolved-path>"`. The
               planner is the AUTHORITY (proposals are input
               only), but well-grounded proposals save it the
               work.
             - `memory_summary`: one-line summary for
               plan_context.

          Output format — fenced ```research``` block,
          required keys depend on `done`. See the [How to
          respond] section of your first message for the full
          schema. Quality bar: each clarification is one
          sentence; options are used when the user picks from
          a small set; `findings` mentions concrete table /
          field names; `resolved_user_inputs` has stable
          lifted-into-inputs keys.

          What you do NOT do:

          - Execute data queries (`data-*` is not in your
            tool surface).
          - Build the full plan / next_wave structure (that's
            the planner's job — you give it the inputs).
          - Append `query-pattern` notepad entries (queries
            haven't been validated yet).
          - Call `session:inquire` directly — the runtime
            handles the modal from your `clarifications`
            array; the legacy tool path is for workers
            mid-execution, not research.
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
            tools: [read, search, append]
          - provider: session
            tools: [inquire]
          - provider: mission
            tools: [get_handoff]
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
        description: >
          End-to-end data worker. Discovers what it needs,
          composes the query, validates it, executes, persists
          output, and emits a handoff. Self-sufficient for any
          single-task data ask.

          Workflow (read `instructions` via skill:ref the first
          time you touch a schema):

          1. Read `[Resolved depends_on]` FIRST. A schema-explorer
             handoff (when present) provides `module`, table
             names, `queries[].name`, and `fields[].name +
             field_type` — lift names verbatim. Skip own
             discovery in that case.
          2. Otherwise discover only what you need:
             - `discovery-search_module_data_objects(module, query)`
               for table list + query field names.
             - `schema-type_fields(type_name, include_description: true)`
               for the columns you'll touch. Default limit 50;
               for wide tables retry with `relevance_query`
               (semantic ranker) before paginating.
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
          5. Execute and persist:
             - Inline answer that fits in context (small
               aggregation, scalar, ≤ 50 rows) →
               `data-inline_graphql_result`. Mention the
               numbers VERBATIM in the handoff body.
             - User-deliverable file (when
               `inputs.file_path` was set by the planner) →
               run the query, write the result to THAT exact
               path (`open(os.path.expanduser(path), 'w')` for
               text formats; `hugr-query:query` with the user's
               `path:` for parquet/JSON). NEVER silently
               substitute a different path.
             - Intermediate mid-mission file (no
               `inputs.file_path` from planner — synthesizer /
               report-builder will read it back later) →
               `hugr-query:query` with an auto-path; parquet
               for tabular, JSON for scalar. Quote the resulting
               path VERBATIM in your handoff so downstream can
               find it.
             - DuckDB SQL over saved parquet → `duckdb-data`
               when the post-processing is one query away;
               otherwise `python-runner` for transforms /
               charts / formatting.

          Dotted modules are GraphQL nesting (`osm.bw` →
          `osm { bw { ... } }`), not identifiers. Field names
          are case-sensitive — quote verbatim.

          Handoff body shape — pick based on what you ran AND
          whether downstream is a `report-builder` (which embeds
          your data INLINE in HTML):

          - **Inline data (PREFERRED for downstream charts)** —
            when your query result has ≤500 rows AND downstream
            is producing a user-facing report, embed the
            full series in the handoff body so report-builder
            ships HTML with inline `<script>JSON</script>`
            blocks. NO file needed.

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
            directly.

          - **Persisted (file output)** — when the dataset is
            too large to inline (>500 rows / multi-MB), OR the
            file IS the deliverable (e.g. user asked for a
            CSV / parquet export). One entry per file:

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
        intent: tool_calling
        can_spawn: false
        autoload_skills: [hugr-data]
        tools:
          - provider: hugr-data
            tools: ['*']
          - provider: duckdb-data
            tools: ['*']
          - provider: python-runner
            tools: ['*']
          - provider: mission
            tools: [get_handoff]
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
        description: >
          For tasks where the SCHEMA MAP is the deliverable —
          inventory, data dictionary, entity-relationship
          summary. NOT a mandatory upstream for data-analyst;
          spawn only when the user asked for a structural
          overview, or when 3+ parallel workers would otherwise
          duplicate the same wide scan.

          Tool sequence (read `instructions` via skill:ref the
          first time you touch a schema):
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
        description: >
          Composes the user-facing HTML (or markdown / prose)
          deliverable from prior-wave handoff bodies. Default
          path: build the HTML in your turn (inline JSON + a
          CDN-loaded JS chart library — Plotly.js / Chart.js /
          inline SVG), write it via `bash-mcp:bash.write_file`.
          Python only when the dataset is too large to inline.

          File path discipline (FAIL-FAST):

          - `inputs.file_path` is LITERAL (planner lifted it
            from researcher's `resolved_user_inputs` or root's
            `[Inputs from parent]`). Resolve to ABSOLUTE via
            `os.path.expanduser` + `os.path.abspath` before
            writing.
          - Empty / missing `inputs.file_path` → emit
            `status: "error"` with
            `reason: "missing inputs.file_path"`. Do NOT
            invent a path or write into the session scratch.
          - After write: verify file exists at the absolute
            destination AND `size > 0` before emitting the
            handoff. NEVER claim a copy / move you didn't
            actually run (no "I copied it to ~/Downloads"
            without an explicit `bash.shell mv` + size check).
            A failed copy → `status: "error"` with the OS
            error verbatim.

          Inline-first contract:

          - Prefer the data-analyst handoff's inline `numbers`
            / `series` / `tables` fields — embed verbatim in
            `<script type="application/json">` blocks. Quote
            values; never paraphrase / round (formatting is a
            browser concern).
          - Open a persisted parquet / JSON file ONLY when
            the handoff routed you to one via `path` + size
            confirms it's too large to inline.

          Handoff body shape:
            `{ path: "<absolute-path>", bytes_written,
              memory_summary: "<one line>" }`. `path` is the
            absolute file you VERIFIED exists at the user's
            destination — synthesizer / root surface it
            verbatim.

          A dedicated `report-builder` skill (with the full
          HTML / JS-chart pipeline prose + its own tool
          surface) is on the backlog —
          `design/002-runtime-canonical/backlog.md`.
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
        intent: cheap
        can_spawn: false
        capabilities:
          plan_context: read
        tools:
          - provider: mission
            tools: [get_handoff]

      - name: synthesizer
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

PDCA mission skill for data analysis over Hugr Data Mesh. The
runtime drives the iteration loop (Plan → Do → Check → Synth);
each role's manifest entry above documents the domain contract.

## Lifecycle

1. Root spawns the mission via `session:spawn_mission`.
2. **Research stage (runtime-owned, Phase 5.x — B15).** When
   the goal trips the `mission.research: when: auto` heuristic
   (deliverable keywords, short / ambiguous goal), runtime
   spawns `researcher` BEFORE the planner. Researcher batches
   all user clarifications into ONE modal (file_path,
   scope_window, format choices, "anything else?" comment),
   does lightweight schema discovery, and emits `done: true`
   with `findings` + `resolved_user_inputs` + optional
   `ac_proposals`. Skipped for trivial single-source asks.
3. Runtime spawns `planner` with [Plan context], [Research
   findings] (when present), [Resolved user inputs] (when
   present), [Research AC proposals] (when present), [Recent
   waves], [Recent verdict], [Available Do roles] in its first
   message. Planner does notepad-first + lightweight
   discovery, then calls `mission:validate_and_approve` (the
   approval modal opens on first plan, after a worker
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

Any worker output that lands on the user's filesystem (a saved
CSV, parquet dump, HTML report, JSON export, generated chart)
is a **user deliverable**. The planner MUST resolve the
destination path BEFORE spawning the producing worker:

- Lift `file_path` (or `output` / `destination`) from root's
  `[Inputs from parent]` block when it's already there.
- Otherwise call
  `session:inquire(type="clarification", question="Where
  should the <kind> file be saved?", options=["~/Downloads/...",
  "<workspace>/..."])` during the planner's turn, BEFORE
  `mission:validate_and_approve`. The approval modal then
  shows the user where their artefact will land.

Intermediate mid-mission scratch files (data-analyst persisting
query results into the mission's workspace for synthesizer /
report-builder to read back) are NOT user deliverables — they
live under `<workspace>/<mission-session>/data/` and don't
require an inquire.
