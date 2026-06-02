---
name: _root
description: Built-in skill granting a root session the surface it needs to chat with the user directly, load data skills, and delegate batch / analytical work to mission coordinators.
license: Apache-2.0
allowed-tools:
  - provider: session
    tools:
      - spawn_mission
      - wait_subagents
      - subagent_runs
      - subagent_cancel
      - inquire
  - provider: mission
    tools:
      - notify
  - provider: plan
    tools:
      - comment
      - show
  - provider: whiteboard
    tools:
      - read
  # Root has the full notepad surface so user-driven memory
  # ("remember this for our conversation") does not require a
  # spawn_mission round-trip. `show` is root-only by design
  # (user-facing rendering).
  - provider: notepad
    tools:
      - append
      - read
      - search
      - show
  # Phase 6.1c — scheduled tasks. Root owns the schedule management
  # surface because scheduled fires belong to the user-facing
  # conversation; they spawn cron-fire subagents under this root
  # when they fire, and the SubagentResult lands here in chat
  # history. Recipe execution itself flows through the `task` ext
  # (synthetic `task:<recipe>` tools admitted by category skills).
  - provider: schedule
    tools:
      - create
      - list
      - pause
      - resume
      - cancel
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [root]
    tier_compatibility: [root]
    # Universal notepad categories the chat carries regardless of
    # which data / domain skill is loaded. The skill extension
    # walks every loaded skill's notepad.tags into Block A; this
    # block is the chat-tier baseline that pairs with whatever
    # domain categories a subsequently-loaded data skill brings.
    notepad:
      tags:
        - name: user-preference
          hint: Stated by the user — region, currency, language, naming, formatting, or output preference. Stable for the conversation.
        - name: deferred-question
          hint: An open thread the user surfaced but is not asking about right now. Capture it so a later turn can resume it without re-prompting.
compatibility:
  model: any
  runtime: hugen
---

# _root skill

Autoloaded into every root session. Read your tier manual in the
constitution for the full operating rules (default-action,
delegation, followup, result surfacing, inquire); this skill body
documents the **knobs and tool-surface specifics** the universal
manual references but does not detail.

## Tool surface (granted by this skill)

- `session:spawn_mission` — singular delegation to a mission
  coordinator. Required: `name` (addressable handle) + `goal`;
  optional `skill` / `inputs` / `wait`. Runtime returns
  `{ session_id, name, mission_id, status, depth }`.
- `mission:notify(name, text)` — followup into an in-flight
  mission's plan_context journal.
- `session:wait_subagents` — block on in-flight children.
- `session:subagent_runs` — paginated transcript pull-through
  for status questions / richer surfacing.
- `session:subagent_cancel` — terminate with a reason
  (cascades to descendants).
- `session:inquire` — clarification / approval bubble to user.
- `plan:comment` / `plan:show` — cross-turn breadcrumbs (root
  does not own a plan body; these are for user-visible
  progress notes).
- `whiteboard:read` — observability into legacy mission state
  (PDCA missions don't use the whiteboard; field stays for
  diagnostic value).
- `notepad:append` / `read` / `search` / `show` — session-
  scoped working memory.
- `schedule:create` / `list` / `pause` / `resume` / `cancel` —
  scheduled tasks. `kind="wake"` synthesises a UserMessage into
  THIS root at fire time (a reminder / nudge). `kind="spawn"`
  spawns a cron subagent under this root against the named
  `skill_ref` and projects the SubagentResult back into history.
  See Knob 7 for the full schema; the minimum is
  `{kind, schedule_kind, schedule_spec, name}` plus `wake_message`
  for wake / `skill_ref`+`inputs` for spawn.

Granted by `_system` (always present): shell (`bash-mcp:*`),
skill catalogue (`skill:load` / `unload` / `ref` / `files`),
admin (`policy:*` / `tool:*` / `runtime:reload`).

Lazy via `skill:load`: data tools per the loaded skill (the
`## Available skills` block lists what's loadable on root).

## Knob 1 — when to delegate (mission triggers)

The tier-root manual already says "default-answer-directly /
spawn for multi-step, batch, or artifact work" — of ANY domain,
not just data. The triggers that justify a spawn:

- The work needs SEVERAL coordinated sub-tasks / steps / waves, or
  combines multiple sources gathered in SEPARATE steps — NOT
  merely because a single operation is internally rich (it
  combines / filters / computes in one call — that stays in chat).
- It is BATCH — the same operation over many items / a large set.
- The user explicitly asked for an **investigation, audit,
  dashboard, comparison study, or comprehensive report / artifact**.
- The user typed `/mission <name>` or named one from the
  catalogue ("run the X mission").
- The `## Available missions` keyword matcher fires hard.

NOT a mission trigger:

- a request a loaded (or one-`skill:load`-away) skill answers in a
  handful of calls — a lookup, a listing, ONE operation (however
  rich internally), one conversion, one formatting pass;
- a single ad-hoc number / fact / lookup;
- "format / convert / dump / save to file" of a result you can
  produce in one query + one formatting pass;
- writing an output file when the work behind it is trivial — the
  artefact is not the threshold.

When in doubt, answer in chat. If the chat reply ends up
inadequate, the user will say "go deeper" — *that's* the cue
to spawn, with context already gathered.

## Knob 2 — `inputs` payload for spawn_mission

`inputs` is free-form JSON the runtime prepends as
`[Inputs from parent]` to the mission's first message. Pass
**facts the user already gave you** — never anything you'd have
to probe for.

Useful keys (use only those the user actually named):

- `file_path` — the output destination the user typed
  (`"~/Downloads/report.html"`).
- `data_files` — absolute path(s) to data the mission should USE
  rather than fetch: data YOU already collected this run (a file
  you wrote, an artifact), OR an external file the user gave to
  analyse (a CSV / parquet / JSON named in their goal). Pass them
  so the mission builds ON / analyses them — without this the
  planner derives a collect wave and redoes (or ignores) the data.
- `data_source` / `module` / `tables` — when the user named
  the source verbatim. Skip when the user spoke in generic
  terms — the planner discovers it.
- `output_format` — `"html"` / `"markdown"` / `"csv"`.
- `time_range` / `filters` / `limit` — when the user
  constrained scope.
- `language` — when answering in a non-default language so
  the mission's artefacts match.

Do NOT put in `inputs`: anything you'd have to probe for, the
goal itself (use `goal`), long prose, credentials / secrets.

Empty / trivial `inputs` is harmless but adds noise — omit
entirely when nothing concrete was named.

## Knob 3 — name shape

`name` is `^[a-z0-9][a-z0-9-]{1,31}$` (2–32 chars, leading
alphanumeric, no trailing dash). Pick 2–4 words from the user's
ask. The runtime sanitises arbitrary input toward that shape and
auto-suffixes on collision with a live sibling; the resolved
name comes back in the response.

## Knob 4 — announce-on-spawn (mandatory after async spawn)

After every successful `spawn_mission(wait="async")` you MUST
emit a short user-visible assistant message naming what was
kicked off. The adapter's spawn marker is faint — without an
assistant acknowledgement the chat looks like you typed nothing.

Shape:

- ≤ two sentences.
- Name the goal in 4–8 words.
- Do NOT promise an ETA.
- Match the language the user wrote in.

If you spawned **two** missions in the same turn, name both in
the same acknowledgement.

## Knob 5 — sync vs async

Pick `wait="sync"` (and let `wait_subagents` finish in the same
turn) only when:

- The user explicitly asked to block ("don't reply until you
  have the answer", "wait for it").
- The task is small enough to land within ~10 s AND a sync
  reply reads better than two turns.

Otherwise: async (the default). Cost is one extra turn boundary
when the result arrives; benefit is root stays responsive to
follow-ups in the meantime.

## Knob 6 — follow up an in-flight mission via `mission:notify`

When the user refines, extends, or redirects work an in-flight
mission ALREADY owns, do NOT spawn a duplicate — route the
follow-up into that mission:

```
mission:notify({ name: "<name or session_id>", text: "<directive>" })
```

TRANSLATE the user's message into a fully-scoped directive in the
mission's own terms — a short anaphoric phrase ("and those too")
becomes an explicit, self-contained instruction; do NOT quote
verbatim. The note lands in the mission's plan_context journal;
the NEXT planner spawn reads it and replans. You do not drive the
mission's supervisor turn directly.

- Spawn a FRESH mission (not notify) only for genuinely
  independent new work.
- Cancel + respawn (`session:subagent_cancel` then spawn) only
  when the follow-up FUNDAMENTALLY invalidates the running goal.

Target check: before notify, scan your recent prompt for a
`[system: subagent_result]` block with the same id / session_id.
If present, the mission is already gone — `mission:notify` returns
`not_found`. Answer from the visible result or spawn fresh,
folding the context in.

## Knob 7 — recipes (`task:*`) and schedules (`schedule:*`)

A **recipe** is a small task-eligible skill that does one concrete
job (count rows, summarise a dashboard, generate a daily report).
Recipes are bundled into **category skills** (`data_utils`,
`pr_workflows`, …) that you load on demand. Loading a category
admits its recipes' synthetic tools — `task:<recipe-name>` — into
your tool catalog with typed parameters.

### Path A — ad-hoc (user wants it NOW)

User described work that matches a recipe but did NOT name a
future time / cadence:

1. Find the `(recipe catalog)` skill in `## Available skills` whose
   domain covers what the user wants. Load it via
   `skill:load("<category>")` (only if it isn't already loaded).
   Prefer this over hand-rolling the job with raw tools you may
   already have loaded — recipes are tested (constitution rule).
2. The recipe's synthetic tool `task:<recipe-name>` is now in your
   tool catalog with its typed `inputs_schema`. Call it directly
   with the user's parameters:

```
task:<recipe-name>({ key1: "...", key2: "..." })
```

The runtime spawns the recipe as a subagent under THIS root,
awaits its terminal handoff, and the result projects back into
chat history as a SubagentResult. Then summarise the result to
the user in one sentence.

### Path B — scheduled recipe (user named a future time or cadence)

User asked for a recipe to run on a schedule:

```
schedule:create(
  kind="spawn",
  skill_ref="<recipe-name>",
  schedule_kind=..., schedule_spec=...,
  inputs={ ... }   # complete inputs — no input-collector at fire time
)
```

`skill_ref` is the recipe's skill name (the same name you'd use
with `task:<recipe-name>` ad-hoc). The scheduled fire has no live
user — ask the user up-front for every required input the recipe
declares, then pass the complete map. Tip: the recipe's synthetic
tool's schema lists exactly the same keys; consult it before
prompting the user.

### Path C — wake-only nudge (no recipe)

User wants a reminder / ping, no recipe involved:

```
schedule:create(
  kind="wake",
  wake_message="<literal text>",
  schedule_kind=..., schedule_spec=...
)
```

`wake_message` is the literal text that arrives as a fresh user
message at fire time.

### Path D — build a new reusable task (no recipe matches yet)

User wants repeatable / schedulable work but no recipe in
`## Available skills` covers it. Don't decline, and don't hand-roll
it as a one-off — create the task skill first, then run or schedule
it:

```
session:spawn_mission(
  name="build-<short>",
  goal="Build a reusable task for: <user request, verbatim>",
  skill="_task_builder",
  inputs={ user_intent: "<the user's full request, verbatim>" }
)
```

`_task_builder` interviews the user for the task's inputs / output /
name, authors + validates the bundle, and saves it as a task-
eligible recipe. When it returns, the new `task:<name>` tool is
available — run it ad-hoc (Path A) or bind it to a schedule
(Path B) per what the user originally asked.

Use this when the work is worth keeping (the user said "every…",
"regularly", "make a task that…") — not for a single one-off,
which is just a mission (or chat).

### Decision tree (apply in order)

1. Did the user name a future time / cadence?
   ("remind me", "every morning", "in 30 minutes", or their
   equivalents in the user's language.)
   - **No** → Path A or just-answer-in-chat.
   - **Yes** → Path B or C.
2. Does the user's intent map to a recipe?
   - **Yes** but recipe's category not yet loaded → `skill:load`
     the category, then Path A (no schedule) or Path B (scheduled).
   - **Yes** and the recipe's synthetic tool is already in catalog
     → call it directly (Path A) or `schedule:create` (Path B).
   - **No, but the user wants a reminder** → Path C.
   - **No, but it's repeatable / schedulable work worth keeping**
     → Path D: spawn `_task_builder` to create the recipe, then run
     (Path A) or schedule (Path B) it. Do NOT invent a recipe name.
   - **No, and it's a one-off** → no recipe needed; spawn a regular
     mission with a suitable mission skill, or just answer in chat.
3. Pre-fill `inputs` from what the user already said. Anything
   missing — ask the user once via `session:inquire` BEFORE the
   call (recipes have no input-collector at runtime; they expect
   a complete inputs map).

### NOT a task / recipe trigger

- User wants the thing done now and there is **no recipe** for it
  → just do it in chat or spawn a regular mission.
- User wants a single piece of work that happens to take a while
  → spawn a mission, do not schedule it.

### `schedule_kind` (Paths B + C)

- `once_in` / `once_at` — single fire. `schedule_spec` is a Go
  duration (`"30m"`, `"1h"`) for `once_in`, or an RFC3339
  timestamp for `once_at`.
- `interval` — repeating cadence. `schedule_spec` is a Go
  duration (`"24h"`, `"15m"`).
- `cron` — standard 5-field cron expression (`"0 9 * * 1"` =
  every Monday 09:00). Recurring; honours `end_condition`.
  Optional `timezone` (IANA name, e.g. `"Europe/Berlin"`) defaults
  to UTC. Cron fires run headless — there is no live user at fire
  time, so the recipe must resolve everything from its `inputs`.

### Optional knobs (Paths B + C)

`initial_planned_at`: USUALLY OMIT. The runtime derives the first
fire from `schedule_spec`: `now+duration` for once_in / interval,
the timestamp itself for once_at. Pass an explicit RFC3339 UTC
override only when you need to anchor on a specific moment
unrelated to the schedule cadence.

`end_condition`: OPTIONAL — defaults to `{"kind":"until_cancel"}`.
For one-shot kinds (once_in / once_at) the value is structurally
ignored; the task auto-cancels after the first fire. Pass
explicitly only for recurring kinds: `{"kind":"count","spec":"<N>"}`
for "do this N times", `{"kind":"until","spec":"<RFC3339>"}` for
"stop on this date".

### Acknowledgement

After successful `schedule:create`, emit a short user-visible
acknowledgement naming the schedule (≤ 1 sentence, match the
user's language). After Path A's `task:<recipe>` call, summarise
the recipe's projected result for the user in one sentence —
don't just dump the raw SubagentResult fence.

## What this skill does NOT grant

- `session:spawn_subagent` / `session:spawn_wave` /
  `session:notify_subagent` / `session:parent_context` —
  removed under mission-PDCA (design 003). Root delegates
  singularly via `spawn_mission`; mid-mission notify is now
  `mission:notify`.
- `plan:set` / `plan:clear` / `whiteboard:init` — root reads
  but does not own a plan body or whiteboard channel. PDCA
  missions don't use the whiteboard at all.
