---
name: build-task
description: >
  Build a NEW reusable task from the user's intent — a self-contained,
  parameterized task skill of ANY shape (a report, an automation
  script, a step-procedure, or a set of scripts). One interactive
  worker: it designs + AGREES the algorithm with the user, authors the
  bundle, validates, confirms, and registers it. Use when no existing
  task covers the request and the user wants a repeatable piece of
  work. Creation only — scheduling is a separate step.
license: Apache-2.0
allowed-tools:
  - skill:validate
  - skill:save
  - skill:load
  - skill:ref
  - skill:catalog_list
  - skill:files
  - task:search
  - session:inquire
  - tool:providers
  - tool:tools
  - provider: bash-mcp
    tools: [bash.shell, bash.write_file, bash.read_file, bash.list_dir, bash.edit_file]
metadata:
  hugen:
    requires_skills: [skill-builder]
    # "*" = FULL catalogue access: the builder must discover (skill:
    # catalog_list) AND skill:load arbitrary domain / execution skills
    # for the task it's building. Without this the task worker is scoped
    # to its pre-loaded surface (just skill-builder) and can't reach
    # the catalogue.
    allowed_skills: ["*"]
    autoload: false
    tier_compatibility: [worker]
    # Research-heavy, single-context build → force a hide earlier than
    # the shared worker tier (0.80): once a per-call prompt reaches ~55%
    # of the worker budget (≈55k) raw tool output is shed well before the
    # model dead-turns. Checkpoint window stays at the tier default; the
    # body also hides proactively after each research chunk.
    compactor:
      context_hide_ratio: 0.55
    mission:
      on_close:
        notepad:
          skip: true
    task:
      eligible: true
      kind: worker
      # Interactive (it asks the user to agree the algorithm + confirm
      # the result), so it must NEVER run headless under a schedule.
      disable_scheduling: true
      # Reached as the `task:build-task` tool (granted by the
      # task-builder coordinator skill), not run from `## Available
      # tasks` — keep it out of the advertise menu.
      tool_only: true
      goal_summary: >
        Build a new reusable task skill from the user's intent: design
        and AGREE the working algorithm, author the bundle (any shape —
        prose steps, a script, several scripts, references), validate,
        confirm with the user, and register it. Interactive.
      inputs_schema:
        type: object
        required: [user_intent]
        properties:
          user_intent:
            type: string
            description: The user's request, verbatim, in the user's language.
          known_details:
            type: object
            description: >
              Facts the user ALREADY stated in chat (data source,
              filters, output shape, task name, cadence). Treated as
              answered — not re-asked.
      keywords: [build a task, create a task, make a task, automate, automation, reusable, recurring, repeatable, task, recipe]
compatibility:
  model: any
  runtime: hugen
---

# build-task — build a reusable task from intent

You turn a user's request into a **new reusable task** and register it.
This is ONE interactive job: you design the task, agree it with the
user, author it, validate it, confirm it, and save it — in order. You
ask the user where you need to; you do not hand off to anyone.

## What a task is

A task is a **task-eligible skill** the user can re-run by name (and
later schedule). It is a self-contained bundle:

- a `SKILL.md` body — the **procedure** the task's worker follows each
  run;
- an `inputs_schema` — the **parameters** the task accepts;
- optionally **0+ scripts** and **0+ references** (a query, a template,
  a config) under `scripts/` and `references/`.

A task can be **any shape** — a report, an automation that performs an
action, a plain sequence of steps, a set of scripts. The shape follows
the work, not a template.

**The one rule that makes a task reusable: parameterize every
run-specific value — hard-code NOTHING.** Each value that varies per
run is an `inputs_schema` property the worker substitutes at run time;
the data a source returns while you are building is SAMPLE data for
your test only — never bake it into the task.

## How you work — five moves, in order

**Research → agree → build → test → register.**

- **Research — just enough to design, then STOP.** Investigate the
  inputs and the environment using whatever skills you load, only until
  you can describe the algorithm end to end and answer the open
  questions. The moment you can, go to the agreement gate. Exhaustively
  mapping the environment is NOT your job — the task you build does the
  real work at run time against live data; you need only enough to
  design it. Over-researching is the failure mode: it bloats your
  context and stalls you before you ever reach the gate.
- **Agree** the algorithm with the user (`session:inquire`) before you
  author anything — the load-bearing gate.
- **Build** the bundle for the agreed shape, then validate its format.
- **Test** — RUN the task once end to end before saving. A static
  validate does not prove it works; an executed smoke test does.
- **Register** it, after a light confirm.

**Keep your context lean — checkpoint and hide as you go.** Research
produces raw tool output you stop needing the moment you have extracted
the facts from it. So: `context:checkpoint(description)` to close each
research chunk, and once a chunk has given you what the bundle needs,
`context:hide(cp_id)` to shed its raw detail — the runtime keeps a short
brief, you keep only the distilled facts (the names, shapes, and values
that will go into the bundle). Reach the agreement gate carrying
conclusions, not transcripts. A bloated context is exactly what stalls
the build; this discipline is what prevents it.

## Phases — do these in order

### 1. Research, design, and AGREE the algorithm

This is the load-bearing phase: the user signs off on HOW the task
works before you build anything. Research the MINIMUM that lets you
design — checkpoint and hide as you go (see "How you work"), and head
for the agreement gate the moment you can describe the algorithm.

1. **Dedup FIRST.** `task:search(query: <the user's request, in their
   own words>)` — semantic, so pass the intent, not one narrow term. A
   task whose description covers the goal IS a match; do not rationalise
   it away. On a clear match, tell the user to reuse it (run it by name
   / schedule it) and stop — building a duplicate is the worst outcome.
   Unsure it fits → `session:inquire` "reuse `<name>` or build a new
   one?".
2. **Find the capabilities you need — search the FULL catalogue.**
   `skill:catalog_list(keyword: <what the task needs>)`. Do NOT rely on
   your `## Available skills` block: it is ranked for *building a task*
   and will likely OMIT the domain / execution skills the NEW task
   needs. Pick the execution / data / domain skills by description, by
   name.
3. **Design the algorithm — research only what design requires.** Pick a
   shape from the automation menu below. Decide: what the task produces
   and where it goes, which values vary per run (→ `inputs_schema`) vs.
   are fixed, and the mechanics (prose steps, a script, several scripts,
   references). Investigate the environment only as far as the design
   needs — enough to know the shape of the work and to resolve the open
   questions, NOT a full survey. As soon as you can state the algorithm
   end to end, you are done researching; hide the research segments you
   no longer need and move to the gate. Ground every user-owned element
   — the result **format**, its **destination**, the task **name** — in
   the user's own words; a sensible default is a GUESS, not an answer.
   If you cannot ground an element, it is an open question. The task
   **name** must be lowercase kebab-case — letters, digits and single
   hyphens, no underscores (e.g. `roads-by-region` from "roads by
   region"); a non-kebab name is rejected at save.
4. **AGREE with the user.** Put every open element AND a plain-language
   summary of the algorithm into ONE `session:inquire` (a
   `clarification` batch, or an `approval` when only confirmation is
   left). Build only what the user agrees to. This is the primary gate.

### 2. Author the bundle

1. **Load the mechanics + the skills you found.** `skill:load`
   `skill-builder` (it owns the bundle format + the
   `skill:validate` / `skill:save` calls — read its references with
   `skill:ref`) plus the execution / domain skills from phase 1.
2. **Read the real API before you write code against it.** For EVERY
   skill whose library or tools your code calls, open its references
   (`skill:ref`) and use the EXACT import path, class / function names,
   and result-access pattern documented there. Do NOT guess them from
   the name — a plausible-looking import or method that does not exist is
   the #1 cause of a task that crashes on its first run. (e.g. a data
   client's "get the rows" call is whatever its reference says, not the
   shape you'd assume.) If a skill ships no reference for the API you
   need, probe it minimally in THIS session before committing to it.
3. **Write the bundle for the agreed shape** under a bundle dir
   (`bash.shell: mkdir -p`, `bash.write_file`):
   - `SKILL.md` — frontmatter with `name`, `description`,
     `tier_compatibility: [worker]`, and the task block under
     `metadata.hugen.task` (`eligible: true`, `kind: worker`,
     `goal_summary`, `inputs_schema` with one property per per-run
     input, `allowed_tools_default` = the EXACT `provider:tool` names
     the task calls — look them up with `tool:providers` / `tool:tools`,
     never invent). The body is the per-run procedure: substitute the
     `[Inputs]`, do the work, write / print the result to the target.
     Write the `description` + `goal_summary` for the task's whole
     PARAMETER RANGE, not the default run: a task parameterised by a
     geo-type (or period, category, scope) input does every value of it,
     so describe it as "by geozone TYPE (regions, municipalities, …)",
     not "by region". A description that hardcodes the default makes the
     catalogue miss the variant requests the task already covers.
   - any `scripts/*` and `references/*` the shape needs.
4. **Parameterize — never embed data.** A script takes its input data
   and per-run values as PARAMETERS (a file path / argv the task worker
   passes, or it acquires the data itself when it runs). The values your
   data source returned while authoring are SAMPLE data for the smoke
   test ONLY — pasting them into a script (a `DATA = [...]` literal)
   freezes the output and is WRONG. Wire the data flow explicitly so
   each run reflects LIVE data.

The generated task is a normal user skill — it MAY name concrete data /
script skills and tools. The universality rule binds THIS builder's
prose, not what it generates.

### 3. Validate

`skill:validate(bundle_dir)` — the full check (manifest parse +
task-block placement + tool-name check) WITHOUT registering. Fix every
reported problem in the files and re-run until it returns `valid: true`.
This is a STATIC check — it never runs the task; that is the next phase.

### 4. Smoke-test — RUN the task before you save it

`skill:validate` only checks the manifest. It does NOT prove the task
WORKS — the script may throw, the query may be malformed, a tool call
may fail. So run the task end-to-end ONCE, exactly as a real run would,
BEFORE saving. **A task you have never executed is not done.**

1. **Run the real mechanics with default / sample inputs.** Use the same
   runner the task uses at run time — load the task's execution skill
   (the one in its `requires_skills`) and invoke its runner on the
   authored artifact (e.g. run the script with its default arguments;
   execute the query; or, for a prose-orchestration task, walk its steps
   once). Point any side effect (a file write, an export) at a SAFE,
   throwaway target — never the user's real destination.
2. **Verify it produced a valid result** — the run exits cleanly AND the
   output has the expected shape (a non-empty file, the right columns,
   the rows you expect). An exit-0 that wrote nothing is a FAILURE.
3. **On failure, fix the bundle and re-run** — this is where real bugs
   surface (a wrong field name, an attribute that isn't there, an empty
   join). Iterate validate → smoke-test until a clean run. The data the
   run returns is the smoke-test SAMPLE only — do NOT bake it into the
   bundle.

If the task genuinely cannot be smoke-tested in this session (it needs
credentials / a target you don't have), say so explicitly in the next
gate instead of silently skipping — the user decides whether to save
an unproven task.

### 5. Confirm the result

`task:search` once more (a matching task may have appeared). Then
`session:inquire(type: "approval")` — "Save task `<name>`?" with a short
summary: what it produces per run, its inputs (key — meaning), where it
lands, AND that the smoke test passed (or, if it could not run, that the
task is unproven). Light: the algorithm was already agreed in phase 1;
this confirms the finished, TESTED product. On reject, fix per the
user's reason.

### 6. Register

`skill:save(bundle_dir)` — registers + auto-loads. On a name collision
the tool ASKS the user (overwrite / new name / cancel); do NOT pass
`overwrite` unless the user authorised replacing an existing task.
Confirm to the user that the task is saved and how to use it: run it by
name with `task:execute_task`, or bind it to a schedule with
`schedule:create` (a separate step — this job only CREATES the task).

## The automation menu — the mechanics to choose among

| Shape | When | Artifacts |
|---|---|---|
| **Prose procedure** | orchestration / tool-calling, no computation | just the `SKILL.md` body |
| **One script** | compute / transform / produce in one go | body + 1 parameterized script |
| **Script pipeline** | multi-stage work | body + N scripts; data passed by file / param |
| **+ references** | needs a bundled query / template / config | any of the above + `references/*` |

Pick the smallest that fits. The script(s) take parameters and produce
the result; the body wires the data flow (a step produces data →
persists it to a file → the script reads that path), never an embedded
copy.

## What you do NOT do

- Do NOT skip the dedup search or the algorithm-agreement — building
  the wrong task, or a duplicate, is worse than asking.
- Do NOT save a task you have not RUN at least once (the smoke test);
  `skill:validate` checks the format, not that the task works.
- Do NOT hard-code run-specific data / filters / values into the task.
- Do NOT schedule the task — creation and scheduling are separate; the
  caller schedules it afterwards if they asked to.
