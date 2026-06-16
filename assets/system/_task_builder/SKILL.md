---
name: _task_builder
description: >
  Mission that BUILDS a new reusable task skill from the user's
  intent. Researcher pins intent + checks for an existing match;
  planner decomposes; author workers compose the query + script;
  assembler writes the bundle; registrar saves it; checker validates;
  synthesizer confirms. Use when no existing task-eligible skill
  covers the request and the user wants a repeatable / schedulable
  task. Creation only — scheduling is a separate step.
license: Apache-2.0
allowed-tools: []
metadata:
  hugen:
    requires_skills: []
    autoload: false
    autoload_for: []
    tier_compatibility: [mission]

    notepad:
      tags:
        - name: task-pattern
          hint: A reusable task shape that worked — what the task does, its inputs, and which discovered skills authored its query / script.

    mission:
      summary: >
        Build a NEW reusable task skill from the user's request —
        a self-contained bundle (prose steps + optional query +
        post-processing script) the user can re-run on demand or
        schedule. Use ONLY when no existing task-eligible skill
        already matches (check the catalogue first). This mission
        CREATES the task; binding it to a schedule is a separate
        step the caller does afterwards.
      keywords: [task, recipe, automate, automation, recurring, periodic, reusable, build a task, create a task, make a task, schedule]

      # Advisory contract for the caller (rendered into root's
      # `## Available missions` block): what to pass at spawn.
      inputs_schema:
        type: object
        required: [user_intent]
        properties:
          user_intent:
            type: string
            description: The user's request, verbatim, in the user's language.
          known_details:
            type: object
            description: Facts the user ALREADY stated in chat (data source / tables, filters, output shape, task name, cadence intent). The researcher treats these as answered and will not re-ask them.

      capabilities:
        notepad: true
        plan_context: true

      research:
        role: researcher
        max_iterations: 3

      # Research-stage lifecycle hooks (research→files, the analyst
      # pattern). This skill is embed-only — no on-disk bundle to
      # copy templates from — so `before` writes the skeletons
      # inline via heredoc. The researcher FILLS the files; the
      # `check` gate re-prompts it while the load-bearing ones
      # (requirements.md, data-model.md) are still skeleton-empty.
      stages:
        research:
          before:
            tool: bash-mcp:bash.shell
            args:
              cmd: |
                mkdir -p {{.MissionDir}}/research
                cat > {{.MissionDir}}/research/requirements.md <<'SKEL'
                # Result requirements — the contract

                > Filled by the researcher. Every section must be GROUNDED in one
                > of: the user's words (intent / caller inputs with a CONCRETE
                > value), the data model (derivable facts only), or the user's
                > answer to a clarification asked THIS stage. A section you cannot
                > ground is an open question — ask, never guess. Downstream roles
                > build EXACTLY this contract; the registrar restates it to the
                > user before saving.

                ## What the task produces per run

                <!-- Content + shape / format + language, concretely: "Markdown
                     table printed in the reply", "CSV file", "HTML report". -->

                ## Where the result goes

                <!-- Printed back / written to a file path / other destination. -->

                ## Per-run inputs vs fixed

                <!-- Values that vary per run (each becomes a task.inputs_schema
                     property: key — meaning — example) vs values fixed inside
                     the task. "No per-run inputs" is a valid answer. -->

                ## Task name

                <!-- Short stable name (snake/kebab). -->

                ## Grounding

                <!-- One line per section above: where the answer came from —
                     "user said X" / "data model admits only Y" / "asked, user
                     answered Z". -->
                SKEL
                cat > {{.MissionDir}}/research/data-model.md <<'SKEL'
                # Data model

                > The exact objects / fields / joins the task's query relies on —
                > written so the query author NEVER re-discovers the schema. When
                > the task fetches no data, write "n/a — no data source".

                ## Objects

                <!-- module + object name + its role in the goal, one line each.
                     Name the module EXACTLY as validated — similarly-named
                     modules exist. -->

                ## Key fields

                <!-- object.field — type — meaning; only what the goal touches. -->

                ## Joins / relations

                <!-- How the objects connect (incl. spatial joins), with the
                     exact argument shapes you validated. -->

                ## Gotchas

                <!-- Nulls, empty groups, magic ids encoding business terms
                     (e.g. a type_id), pagination limits — anything a worker
                     would otherwise trip on. -->
                SKEL
                cat > {{.MissionDir}}/research/queries.md <<'SKEL'
                # Queries

                > The validated query shapes the task will run — copy what you
                > ACTUALLY executed, with one line on what each returns. Mark
                > every per-run value with a Go-template placeholder over the
                > input key (".Inputs.<key>" in double curly braces). When the
                > task fetches nothing, write "n/a — no query".

                ## Primary query

                <!-- The validated query text. -->

                ## Verification probes

                <!-- Optional: counts / aggregation checks you used to confirm
                     the data is real. -->
                SKEL
          check:
            tool: bash-mcp:bash.shell
            args:
              cmd: |
                cd {{.MissionDir}}
                ok=1
                for f in requirements.md data-model.md; do
                  lines=$(awk '/<!--/{c=1} c==0{print} /-->/{c=0}' "research/$f" 2>/dev/null | grep -cv -e '^[[:space:]]*[#>]' -e '^[[:space:]]*$')
                  if [ "${lines:-0}" -lt 3 ]; then
                    echo "research/$f is still a skeleton ($lines content lines) — fill its sections (or write an explicit n/a where a section does not apply) before finishing" >&2
                    ok=0
                  fi
                done
                [ "$ok" -eq 1 ]

      plan:
        role: planner
        max_waves: 8
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
          Mission planner for task-bundle construction. Each
          iteration sees [Plan context], [Recent waves], [Recent
          verdict], [Available Do roles], plus (when research ran)
          [Research findings] / [Resolved user inputs] / [Research
          AC proposals]. Emit ONE `kind=plan` handoff with
          `next_wave` (or `null` for plan_complete).

          **What you are assembling.** A task skill is a
          self-contained, task-eligible skill bundle:

          - a `SKILL.md` (worker-shape: `task.kind: worker`) whose
            body is the imperative step list the task's worker
            follows per run, plus a `task.inputs_schema` and a
            `task.allowed_tools_default` allow-list;
          - optionally a query file (the validated data-fetch
            shape) shipped as a bundle reference / asset;
          - optionally a post-processing script.

          The user does NOT hand-write any of this — your workers
          author every piece, and the bundle is persisted via
          `skill:save`.

          **Wave-anatomy rule.** Subagents inside a wave run in
          PARALLEL — any data dependency MUST cross a wave boundary
          (producer in wave-N, consumer in wave-N+1 with the
          producer's handoff ref under `depends_on`). Same-wave
          consumers fire before the producer's handoff exists.

          **Recommended wave shape** (pick the smallest that fits
          the task being built):

          - **Wave 1 — authoring (parallel).** Spawn `query-author`
            iff the task fetches data, and `script-author` iff the
            task post-processes / formats output. A pure-fetch task
            needs only `query-author`; a task that reshapes an
            existing file needs only `script-author`; many tasks
            need both, in parallel.
          - **Wave 2 — `skill-assembler`** with `depends_on` on the
            wave-1 author handoffs. Builds the bundle dir + self-
            validates it (`skill:save validate_only`); hands off the
            `bundle_dir`.
          - **Wave 3 — `task-registrar`** with `depends_on` on the
            assembler. Confirms the result with the user, then
            `skill:save(bundle_dir)`; decides catalogue placement. Do
            NOT set `skip_check` on this wave — the checker validates
            the saved bundle.

          A trivial task (fixed prose, no query, no script) can
          collapse to wave-1 `skill-assembler` → wave-2
          `task-registrar`.

          **Inputs propagation.** Lift every entry from [Resolved
          user inputs] into the relevant worker's `inputs.<key>`
          verbatim — the task name, goal, the input parameters the
          task will accept, the output destination, and (CRITICAL)
          the `data_skill` / `script_skill` names the researcher
          discovered. The author workers load those skills by the
          names you pass; never invent a skill name the researcher
          did not report, and never hard-code a data-source name —
          pass through what research resolved.

          **Amend re-spawn — chain depends_on.** When [Recent
          verdict] is `amend`, re-spawn the SAME role with the prior
          attempt's handoff ref under `depends_on`; the retrying
          worker reads the prior body + the checker's `issues` and
          fixes the gap instead of redoing the work.

          **Approval gate.** The runtime appends a [STOP — how to
          submit your plan] (first iter) or a short reminder (later
          iters). Call `mission:validate_and_approve` with your full
          body; while it returns `valid:false`, fix and re-call; once
          it returns `valid:true` (and the user approves), reply with
          just `done` — there is NO fenced ```plan``` block; the tool
          IS the submission channel, and the runtime holds your turn
          open until a plan is submitted+approved. Set
          `requires_reapproval: true` only when `mission_goal`
          reworded with materially different intent since the last
          approved plan.

          **Acceptance criteria — diff schema.** The mission owns
          the AC list with stable `ac-N` ids; you read the roster
          under [Mission acceptance criteria] and emit DELTAS, never
          the full list.

          - **Iteration 1** — seed AC via `ac_add`. Each is a
            singularly-satisfiable statement about the FINISHED task
            skill, e.g. "Task skill saved and loadable", "Accepts
            inputs <list>", "Query validated against the live
            schema", "Script smoke-runs green on synthetic data",
            "allowed_tools_default lists only the tools the task
            calls", "User confirmed the assembled bundle before
            registration". Promote relevant [Research AC proposals]
            here.
          - **Later iterations** — `ac_update[]` by id: `ac_add` a
            newly-revealed requirement (auto-reopens the modal),
            `drop:true` a no-longer-applicable row, or
            status-only `{id, status:"satisfied", evidence:"<ref>"}`.
          - Do NOT re-emit rows the checker / worker already updated.

          **Boot discipline.** Before drafting: `notepad:search` the
          intent's keywords for prior `task-pattern` entries you can
          lift. You are a coordinator, not an author — pre-resolve
          the skill names + input shape from research so workers act
          on concrete values.
        intent: reasoning
        can_spawn: false
        autoload_skills: [_mission_worker]
        capabilities:
          plan_context: read
        compactor:
          enabled: true
          max_tokens: 30000
          max_turns: 20
          preserved_recent_turns: 6
          min_turn_gap: 2
          llm_intent: summarize
        tools:
          - provider: notepad
            tools: [read, search]
          - provider: mission
            tools: [get_handoff, validate_and_approve]
          - provider: skill
            tools: [catalog_list]

      - name: researcher
        description: >
          Mission research — spawned BEFORE the planner on every
          run. Your job: pin down WHAT task the user wants built,
          confirm it's feasible with the capabilities this agent
          actually has, and ask the user about every dimension the
          intent leaves open. Emit a kind=research handoff:
          `done: false` to ask, or `done: true` with `findings` +
          `resolved_user_inputs` + optional `ac_proposals` when you
          have everything.

          Do these, in order:

          0. **Dedup FIRST — search the catalogue by the user's own
             request.** Building a duplicate is the worst outcome, so
             this is move zero. Call `skill:catalog_list(task_eligible:
             true, keyword: <the user's request, in their own words>)`
             — the search is semantic, so pass the INTENT (what the
             task should do), not one narrow domain term. If a saved
             task already covers the request, the user does NOT need a
             new one — emit `done: true` with a `findings` note naming
             the match and an `ac_proposal` "reuse existing task
             <name>", so the planner / synthesizer can tell the user to
             run or schedule it directly (`task:execute_task` /
             `schedule:create`) instead of rebuilding. Only build new
             when nothing fits — and when in doubt, run a second search
             with a differently-worded query before concluding nothing
             matches.

          1. **Discover the agent's capabilities.** Check your
             `## Available skills` block first, then search the full
             catalogue with `skill:catalog_list(keyword: <what the
             task needs>)` — e.g. "data discovery query validation"
             for the data side, "script python execution" for the
             code side. Pick by each skill's description. Record the
             chosen skill NAMES into `resolved_user_inputs` under
             stable keys `data_skill` and `script_skill` (omit one
             if the task doesn't need it). The author workers will
             `skill:load` exactly these names. Do NOT assume a
             name — read it from the catalogue.

          2. **Probe feasibility.** `skill:load` the data skill you
             found and use ITS discovery / schema tools to confirm
             the data the task needs actually exists (sources,
             tables, the columns the goal hinges on). If it does
             not, emit `status: "error"` with a clear reason so the
             planner can amend or the user can rescope.

          3. **Build the RESULT CONTRACT — resolve or ask, never
             guess.** The scaffolded `research/requirements.md` IS
             the contract: fill it section by section. Before you
             can finish, it must state CONCRETELY what the finished
             task produces on each run: the result's content, its
             shape / format, where it goes, its language, which
             values vary per run vs. stay fixed, and the task's
             name. Whatever form the result takes — a file, a
             message, a dataset, an action performed — the contract
             must describe it precisely enough that the user could
             not later say "I didn't ask for THAT". (The schema
             facts from step 2 land in `research/data-model.md` +
             `research/queries.md` — the author workers read those
             files instead of re-discovering, so name modules /
             objects EXACTLY as validated.)

             That statement is your self-check. Every element of
             it must be grounded in one of exactly three sources:

             - **the user's own words** — the intent text or a
               CONCRETE value in [Inputs from caller] (a paraphrase
               of the request — e.g. output: "report" — is NOT a
               resolved format; treat it as open);
             - **the data model** — but only for facts that are
               derivable: which objects / fields back the goal, how
               entities join, which filter encodes a business term.
               Resolve these YOURSELF in step 2; never ask what you
               can look up;
             - **the user's answer** to a clarification you ask NOW.

             A sensible DEFAULT is NOT a fourth source — it is a
             guess. "Markdown table because a report is usually a
             table", "printed back by default", "name composed from
             a pattern" are all guesses, however reasonable. The
             result's **format**, **destination**, and **name** are
             USER-OWNED: the data model cannot answer them, so
             unless the user named a concrete value they are OPEN
             and MUST go in your clarification batch. If your
             Grounding line for any of them would read "default" /
             "convention" / "usually" — that element is unresolved;
             ask it.

             An element you cannot ground is an open question —
             guessing it plants a wrong assumption into every
             downstream worker. Asking ONE question (e.g. a genuine
             data ambiguity you found) does NOT discharge the
             others — every ungrounded user-owned element goes in
             the SAME batch. The modal is already open; adding the
             format / name questions to it is free.

             Bundle every open element into ONE `done: false`
             handoff with a `clarifications: [...]` array. Derive
             the questions from the actual intent — common axes for
             a TASK:

             - **What the task produces** — a number, a table, a
               file, a report? What format?
             - **Inputs the task should accept** — which dimensions
               vary per run (a date window, an entity, a threshold)
               vs. are fixed in the task itself. These become the
               task's `inputs_schema` properties.
             - **Where output goes** — printed back, or written to a
               path the user names.
             - **Naming** — a short, stable task name (snake/kebab),
               and whether to keep it personal/local or note a
               catalogue to publish it to.

             Each entry: `id` (snake_case stable key), one-sentence
             `question` in business terms, `kind`
             (required|optional|comment), `options` when picking
             from a small set, `multi: true` when several picks are
             valid. End with a `kind: "comment"` "anything else?"
             slot. A SECOND round is allowed only when first answers
             reveal new ambiguity (runtime caps at 3 iterations).

          4. **When you have everything**, fill the three
             `research/` files, then emit `done: true` with:
             - `findings`: a short paragraph telling the planner what
               the task should do, what data backs it, and which
               skills author its query / script.
             - `file_refs`: the `research/` paths you filled —
               workers read them via `bash.read_file`.
             - `resolved_user_inputs`: stable key/value map — at
               least `task_name`, `task_goal`,
               `result_requirements` (the step-3 result contract,
               one concrete paragraph), the per-run input keys +
               their meaning, `output_target`, `data_skill`,
               `script_skill` (when relevant), and any catalogue
               choice. The planner propagates these into workers,
               and the registrar restates the contract in its
               pre-save confirmation.
             - `ac_proposals` (optional): proposed acceptance
               criteria grounded in the user's answers.
             - `memory_summary`: one line for plan_context.

          What you do NOT do: compose the final query / script
          (authors do that), write the bundle (assembler does that),
          or call `session:inquire` directly — the runtime drives
          the modal from your `clarifications` array.
        intent: reasoning
        can_spawn: false
        autoload_skills: [_mission_worker]
        capabilities:
          plan_context: read
        compactor:
          enabled: true
          max_tokens: 30000
          max_turns: 40
          preserved_recent_turns: 12
          min_turn_gap: 3
          llm_intent: summarize
        tools:
          - provider: skill
            tools: [catalog_list, load, ref, files]
          - provider: notepad
            tools: [read, search]
          - provider: session
            tools: [inquire]
          - provider: mission
            tools: [get_handoff]

      - name: query-author
        description: >
          Authors the data-fetch shape for the task being built.
          Spawned only when the task fetches data. Read your task
          brief first — the planner passes the data skill name (from
          research) as `inputs.data_skill`, the goal, and the per-run
          input keys.

          0. **Read the research files FIRST** —
             `bash.read_file research/data-model.md` +
             `research/queries.md` (+ `research/requirements.md`
             for the result contract). They carry the EXACT modules,
             objects, fields, joins, and validated query shapes.
             Trust them over your own re-discovery: similarly-named
             modules exist, and a fresh search can drift to the
             wrong one. Re-discover ONLY what the files genuinely
             lack.
          1. `skill:load(<inputs.data_skill>)`. If the brief did not
             name one, `skill:catalog_list(keyword: "data discovery
             query validation")` (or check `## Available skills`) to
             find a query/validation skill and load it.
          2. Compose from the researched grammar — reuse the
             validated shapes from `research/queries.md` and any
             [Resolved depends_on] bodies rather than re-scanning
             the schema.
          3. Compose the query the task will run. Where a value
             varies per run, mark it with a Go-template placeholder
             `{{ .Inputs.<key> }}` keyed on the task's input names
             (e.g. `{{ .Inputs.window_days }}`), so the task worker
             substitutes its inputs at run time. Keep fixed parts
             concrete.
          4. **Validate the shape** against the live schema using the
             loaded skill's validation tool. Validate with a sample
             value substituted for each placeholder; the SHAPE must
             pass. One real attempt per draft — on the same error
             twice, rewrite or emit `status: "error"`.
          5. Emit a `handoff` body with: `query` (the templated
             query text), `placeholders` (the `.Inputs.*` keys it
             references), `notes` (source / fields / how the task
             worker should run it + which tool), `memory_summary`.
        intent: default
        can_spawn: false
        autoload_skills: [_mission_worker]
        tools:
          - provider: bash-mcp
            tools: [bash.read_file, bash.list_dir]
          - provider: skill
            tools: [catalog_list, load, ref, files]
          - provider: notepad
            tools: [read, search]
          - provider: mission
            tools: [get_handoff, get_research]

      - name: script-author
        description: >
          Authors the post-processing script for the task being
          built. Spawned only when the task reshapes / formats /
          charts its data. Read your task brief first — the planner
          passes the script skill name as `inputs.script_skill`, the
          goal, the expected input data shape (from the query-author
          handoff via [Resolved depends_on]), and the output target.

          0. **Read `research/requirements.md` FIRST**
             (`bash.read_file`) — the result contract: the exact
             shape / format / destination / language the script's
             output must honour. The script encodes THAT, not your
             own taste.
          1. `skill:load(<inputs.script_skill>)`. If unnamed,
             `skill:catalog_list(keyword: "script execution python")`
             (or check `## Available skills`) to find a
             code-execution skill and load it.
          2. Write a small, self-contained script that reads the
             task's data (a file path or the query result it is
             handed at run time), produces the output the user
             asked for, and writes / prints it to the output target.
             Parameterise per-run values via argv / kwargs so the
             task worker can pass its inputs — do not hard-code a
             run-specific value.
          3. **Smoke-run it on synthetic data** using the loaded
             skill's run tool: feed a tiny fabricated input matching
             the expected shape and confirm it executes green and
             produces sane output. Fix and re-run until green; on
             repeated failure emit `status: "error"`.
          4. Emit a `handoff` body with: `script` (full source),
             `entrypoint` (how the task worker invokes it: file name
             + args/kwargs), `smoke_result` (one line: ran green on
             synthetic input X → output Y), `memory_summary`.
        intent: default
        can_spawn: false
        autoload_skills: [_mission_worker]
        tools:
          - provider: bash-mcp
            tools: [bash.read_file, bash.list_dir]
          - provider: skill
            tools: [catalog_list, load, ref, files]
          - provider: notepad
            tools: [read, search]
          - provider: mission
            tools: [get_handoff, get_research]

      - name: skill-assembler
        description: >
          Builds the complete task-skill bundle as FILES in the
          mission workspace, then self-validates it. Spawned after
          the authoring wave; reads the query-author / script-author
          bodies via [Resolved depends_on] and the [Resolved user
          inputs] the planner passed (task_name, task_goal, per-run
          input keys, output_target, allowed tools).

          `_skill_builder` is loaded for you — it owns the manifest
          format, the bundle layout, and the `skill:save` call.
          Consult its references with `skill:ref(skill:
          "_skill_builder", ref: "manifest-format")` (and
          `bundle-layout`, `tool-discovery`, `save-call`) and follow
          its authoring loop. Do NOT register the skill — you only
          build + validate; the registrar saves it after the user
          confirms.

          1. **Read the contract.** `bash.read_file
             research/requirements.md` — the produced SKILL.md's
             steps, inputs_schema, and output handling MUST match it
             exactly. Read `research/data-model.md` +
             `research/queries.md` for the validated shapes.

          2. **Build the bundle dir.** `bash.shell: mkdir -p
             bundle/references bundle/scripts`, then write the files:
             - `bundle/SKILL.md` — frontmatter sets `name:
               <task_name>` (kebab/snake), `description`,
               `tier_compatibility: [worker]`, and the task block
               UNDER `metadata.hugen.task` (NOT top-level, NOT a flat
               `task_eligible`):
                 `task.eligible: true`, `task.kind: worker`,
                 `task.goal_summary: <one-line imperative>`,
                 `task.inputs_schema:` a JSON Schema (draft 2020-12)
                 with one property per per-run input + `required`,
                 `task.allowed_tools_default:` the EXACT provider:tool
                 names the task worker calls. Look them up with
                 `tool:providers` / `tool:tools` — NEVER invent; a
                 skill name like `hugr-data:execute` is rejected.
                 Keep it minimal — it is the cron pre-approval list.
               Do NOT set `metadata.hugen.autoload`.
               The body is the imperative step list the task worker
               follows each run: substitute its `[Inputs]` into the
               query, run it via the named tool, run the script on
               the result, write/print output to the target.
               Reference bundled files by saved path
               (`${SKILL_DIR}/references/query.graphql`,
               `${SKILL_DIR}/scripts/report.py`). The generated task
               skill is a normal user skill — it MAY name concrete
               data / script skills and tools (the universality rule
               binds THIS builder's prose, not what it generates).
             - `bundle/references/query.graphql` — the query-author's
               templated query, when one was authored.
             - `bundle/scripts/report.py` — the script-author's
               script, when one was authored.

          3. **Self-validate.** `skill:save(bundle_dir: "bundle",
             validate_only: true)`. Fix every reported problem in the
             files (task-block placement, unknown tool names, parse
             errors) and re-run until it returns `valid: true`.

          Emit ONE `handoff` body with: `bundle_dir: "bundle"`,
          `task_name`, `validated: true`, the
          `allowed_tools_default` list, a one-line `result_summary`
          of what the task produces, and a `memory_summary`. Emit
          `status: "error"` if a required author handoff is missing
          or validation cannot be made to pass.
        intent: default
        can_spawn: false
        autoload_skills: [_mission_worker, _skill_builder]
        tools:
          - provider: bash-mcp
            tools: [bash.shell, bash.write_file, bash.read_file, bash.list_dir]
          - provider: mission
            tools: [get_handoff, get_research]

      - name: task-registrar
        description: >
          Confirms the assembled bundle with the user, persists it,
          and decides catalogue placement. Reads the skill-assembler
          handoff via [Resolved depends_on] — it carries `bundle_dir`
          (the already-built, self-validated bundle directory in the
          workspace), `task_name`, and `allowed_tools_default`.

          1. **Pre-save dedup gate — last check before the write.**
             The researcher searched at the start, but a build is long
             and a matching task may already exist (or have been saved
             meanwhile). Call `skill:catalog_list(task_eligible: true,
             keyword: <task_name + what it produces>)` once more. If an
             equivalent task already exists and [Resolved user inputs]
             did NOT authorise replacing it, do NOT save a duplicate —
             emit a TERMINAL reuse handoff (`done: true` with
             `reused_existing: <name>` and `saved_name: null`, no
             save), so the synthesizer tells the user to run that one
             instead. This is a SUCCESS (the work already exists), not
             a failure to fix — do not retry or rename. Only proceed to
             confirm + save when nothing equivalent is registered.
          2. **Confirm the RESULT with the user — mandatory, BEFORE
             saving.** The plan approval covered the PLAN; this gate
             confirms the PRODUCT. Call `session:inquire(type:
             "approval")` with a short `question` ("Save task
             <name>?" in the user's language) and a `context`
             summarising the bundle: the task name, what it produces
             per run (restate the contract from
             `research/requirements.md` — read it via `bash.read_file`
             — or `result_requirements` in [Resolved user inputs];
             the user agreed to THAT, so flag any deviation), the
             per-run inputs (key — meaning), the
             `allowed_tools_default` list, and where it lands (local
             store / catalogue note). On `approved: false`, do NOT
             save — emit `status: "error"` quoting the user's
             `reason` verbatim so the planner amends the relevant
             author / assembler and re-runs this gate.
          3. **Register.** `skill:save(bundle_dir: <bundle_dir from
             the assembler handoff>)`. The bundle was already
             validated by the assembler, so this is the real write;
             it parses, validates again, registers, and auto-loads.
             On an `ErrSkillExists` collision, do NOT overwrite
             unless [Resolved user inputs] explicitly authorised
             replacing an existing task — otherwise emit `status:
             "error"` noting the collision so the planner can rename.
             Within an amend retry of THIS save you may set
             `overwrite: true`. If save returns a validation error
             (task-block placement, unknown tool name) the assembler
             missed, emit `status: "error"` with the exact message so
             the planner re-runs the assembler.
          4. Confirm it loaded — the save result lists the registered
             `name` + `files`; `skill:files(<name>)` shows the bundle
             on disk.
          5. **Catalogue placement.** In local/personal mode the
             saved skill lives in the local store and is immediately
             runnable via `task:execute_task` (or `task:<name>` once a
             skill admits it) and schedulable — there is no separate
             publish step. If [Resolved user inputs] named a shared
             catalogue to publish to, note it in your handoff for the
             synthesizer to surface (remote publish is not performed
             here).
          6. Emit a `handoff` body with: `saved_name`, `bundle_dir`,
             `files` (saved relative paths from the save result),
             `inputs_schema_ok` (manifest parsed + schema present),
             `user_confirmed` (true — the step-2 approval),
             `placement` (local | <catalogue note>),
             `reused_existing` (the existing task name when the step-1
             dedup gate matched and nothing was saved; null otherwise),
             `memory_summary`.
        intent: default
        can_spawn: false
        autoload_skills: [_mission_worker]
        tools:
          - provider: bash-mcp
            tools: [bash.read_file, bash.list_dir]
          - provider: skill
            tools: [save, load, files, ref, catalog_list]
          - provider: session
            tools: [inquire]
          - provider: mission
            tools: [get_handoff, get_research]

      - name: checker
        description: >
          Verdict-emitting role spawned after the registrar wave.
          Reads [Handoffs to check] + [Plan context] and emits ONE
          kind=verdict handoff. Validates that the task skill the
          mission built is actually usable.

          Decision enum:
            - continue → the saved bundle is valid but the plan has
              more waves to run.
            - amend    → validation gap. Provide `issues:
              [<one line each>]` — e.g. save failed, inputs_schema
              missing/invalid, allowed_tools_default empty or over-
              broad, the query never validated, the script never
              smoke-ran green. The next planner re-spawns the
              relevant author / assembler / registrar to fix it.
            - inquire  → user input needed (rare here; e.g. a name
              collision the user must resolve). Call
              `session:inquire` BEFORE the handoff.
            - finish   → the task skill is saved, loadable, has a
              valid inputs_schema + a minimal allowed_tools_default,
              the user confirmed the bundle before save, and (when
              applicable) its query validated and its script
              smoke-ran green. Routes to synthesis. ALSO finish when
              the registrar's pre-save dedup gate matched
              (`reused_existing` set, nothing saved) — an equivalent
              task already exists, so there is nothing more to build.

          Confirm from the registrar + author handoffs that the
          evidence is present: `inputs_schema_ok`, `user_confirmed`
          (the registrar's pre-save approval), a green
          `smoke_result` if a script was authored, a validated query
          if one was authored. Missing evidence → `amend`, do not
          `finish` on faith.

          **Evidence must match the AC's LITERAL terms.** When an
          AC names a specific module / object / format / value,
          check the handoff CONTENT against that exact name — a
          status flag is not evidence. A query that validates green
          against a DIFFERENT module than the AC names does NOT
          satisfy it (similarly-named modules exist); compare
          against `research/data-model.md` (`bash.read_file`) when
          in doubt, and `amend` naming the mismatch. Be terse — `reason` + `memory_summary`
          one line each.
        intent: default
        can_spawn: false
        autoload_skills: [_mission_worker]
        capabilities:
          plan_context: read
        tools:
          - provider: bash-mcp
            tools: [bash.read_file, bash.list_dir]
          - provider: session
            tools: [inquire]
          - provider: mission
            tools: [get_handoff, get_research]

      - name: synthesizer
        description: >
          Mission's final assistant — runs ONCE after plan_complete
          or `decision: finish`. Reads [Plan context] + [Handoffs]
          and produces the user-facing confirmation root surfaces
          verbatim.

          Report:
          - The task skill name that was created — OR, if research's
            move-zero search or the registrar's pre-save dedup gate
            (`reused_existing`) found an existing match, that the user
            should reuse THAT task instead (name it); nothing new was
            built.
          - In one line, what the task does and the inputs it
            accepts.
          - **How to use it next** — it can be run on demand by name
            via `task:execute_task` with those inputs, and scheduled to
            run periodically with `schedule:create` (this mission
            CREATED the task; it did NOT schedule it). If the user's
            original intent was to schedule it, say so explicitly so
            root performs the schedule step next.
          - The `allowed_tools_default` list (the tools that will be
            auto-approved when the task runs headless under a
            schedule), and any catalogue-placement note from the
            registrar.

          Quote the saved name + bundle paths verbatim. Mention any
          unresolved `amend` gap. Tight — a short paragraph or a
          3-line summary. Emit ONE fenced `handoff` block with
          kind=synthesis; body carries the final message.
        intent: default
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

# _task_builder

PDCA mission that builds a **new reusable task skill** from the
user's intent. A task is a self-contained, task-eligible skill
bundle — prose steps the task worker follows each run, plus an
optional data-fetch query and an optional post-processing script —
that the user can run on demand by name (`task:execute_task`) or bind
to a schedule (`schedule:create`). This mission CREATES the task; it does
NOT schedule it. The runtime drives the iteration loop
(Plan → Do → Check → Synth); each role's manifest entry above
documents its contract.

## When this mission runs

Root spawns it when the user wants a **repeatable / schedulable**
piece of work and no existing task-eligible skill already covers it.
The researcher's first move is to check the catalogue
(`skill:catalog_list task_eligible:true`) — if a saved task already
fits, the mission tells the user to reuse it instead of rebuilding.

## Lifecycle

1. **Research stage (runtime-owned).** Because this skill declares a
   `mission.research` block, the runtime spawns `researcher` before
   the planner. It checks for an existing match, discovers which
   installed skills provide the data / script capabilities the task
   needs (recording their names as `data_skill` / `script_skill`),
   probes feasibility, and batches every open dimension — what the
   task produces, its per-run inputs, output target, name — into one
   modal. When answered, it emits `done: true` with `findings` +
   `resolved_user_inputs` + optional `ac_proposals`.
2. **Plan stage.** Planner reads research output, seeds acceptance
   criteria, and calls `mission:validate_and_approve` (the user
   approves the plan once). It schedules the authoring wave.
3. **Do waves.** `query-author` and/or `script-author` (parallel)
   author + validate the query / script by loading the
   researcher-named skills. `skill-assembler` builds the bundle dir
   (under `_skill_builder`'s format) and self-validates it with
   `skill:save validate_only`. `task-registrar` confirms the
   assembled bundle with the user (one approval modal — the plan
   approval covered the plan; this confirms the product), then
   `skill:save(bundle_dir)` + decides placement.
4. **Check.** `checker` confirms the saved bundle is valid (schema
   present, user confirmed pre-save, query validated, script
   smoke-ran green) and routes `finish` / `amend`.
5. **Synth.** `synthesizer` confirms the created task to the user
   and points at the run / schedule next-steps.

## What the builder produces vs. what binds it to a schedule

- **This mission = stage 1 (create).** Output: a saved task-eligible
  skill. Standalone — runnable ad-hoc, or never.
- **Scheduling = stage 2 (separate).** Binding the task to a periodic
  run is `schedule:create(kind=spawn, skill_ref=<name>,
  schedule_kind=cron, schedule_spec=...)`, performed by root after
  this mission returns.

## Universality note

This builder's OWN prose names no specific data source, provider, or
hub skill — its workers discover and `skill:load` what they need at
runtime via the catalogue. The **generated** task skill is an
ordinary user skill and MAY name concrete data / script skills and
tools; the universality rule binds the builder, not its output.

## Handoff channels

- **By ref** — every worker ends with one fenced `handoff` block;
  the runtime stores it under `<name>@<wave>` and inlines depended-on
  bodies into the next wave's first message.
- **Plan context journal** — each handoff's `memory_summary` feeds a
  FIFO digest visible to planner / checker / synthesizer.
- **Notepad** — durable `task-pattern` entries (what worked) for
  future builds; never live values.
