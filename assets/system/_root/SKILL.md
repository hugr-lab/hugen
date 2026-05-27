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
  # Phase 6.1c — scheduled tasks. Root owns the full task surface
  # because cron tasks belong to the user-facing conversation; they
  # spawn cron-fire subagents under this root when they fire, and
  # the SubagentResult lands here in chat history.
  - provider: task
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
  runtime: hugen-phase-4
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
- `task:create` / `list` / `pause` / `resume` / `cancel` —
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
spawn for batch work". The triggers that justify a spawn:

- Combines results from **multiple sources / tables / modules**
  with joins, group-by, aggregation, or cross-entity
  comparison.
- The user explicitly asked for an **investigation, audit,
  dashboard, comparison study, or comprehensive report**.
- The user typed `/mission <name>` or named one from the
  catalogue ("run the X mission").
- The `## Available missions` keyword matcher fires hard.

NOT a mission trigger:

- "list / show / count" against a single source;
- "format / convert / dump / save to file" of a result you can
  produce in one query + one formatting pass;
- a single ad-hoc number / fact / lookup;
- writing an output file when the work behind the file is
  trivial — the artefact is not the threshold.

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

## Knob 6 — followup target check

Before calling `mission:notify`, scan your recent prompt for a
`[system: subagent_result]` block carrying the same id /
session_id. If the block is there, the mission is gone — you
cannot notify it. Either answer the user from the visible result
or spawn a fresh mission folding the context in.

`mission:notify` against a completed id returns `not_found`.

## Knob 7 — recipes (`## Available tasks`) and `task:*`

The `## Available tasks` block enumerates **recipes**: curated
task-eligible skills (`metadata.hugen.task.eligible: true`). Each
recipe is a worker-shape skill that does one concrete job. The same
recipe can be run two ways — pick the path from user intent.

### Path A — ad-hoc (user wants it NOW)

User described work that matches a recipe but did NOT name a
future time / cadence. Run it through the universal wrapper:

```
session:spawn_mission(
  skill="_run_task",
  goal="<short description of the run>",
  inputs={ task_skill: "<recipe-name>", task_inputs: { ... } }
)
```

`task_skill` MUST be a name from the `## Available tasks` block.
`task_inputs` is whatever the user already provided; the wrapper's
`input-collector` will ask the user for anything missing before the
recipe runs.

### Path B — scheduled recipe (user named a future time or cadence)

User asked for a recipe to run on a schedule. Use the scheduler:

```
task:create(
  kind="spawn",
  skill_ref="<recipe-name>",
  schedule_kind=..., schedule_spec=...,
  inputs={ ... }   # complete inputs — no input-collector at fire time
)
```

`skill_ref` MUST be from `## Available tasks`. Unlike Path A, the
scheduled fire has no live user — there is no `input-collector`. If
the recipe needs values you don't have, **ask the user for them
before calling `task:create`**, then pass the complete map.

### Path C — wake-only nudge (no recipe)

User wants a reminder / ping, no recipe involved:

```
task:create(
  kind="wake",
  wake_message="<literal text>",
  schedule_kind=..., schedule_spec=...
)
```

`wake_message` is the literal text that arrives as a fresh user
message at fire time.

### Decision tree (apply in order)

1. Did the user name a future time / cadence?
   ("remind me", "every morning", "in 30 minutes", or their
   equivalents in the user's language.)
   - **No** → Path A or just-answer-in-chat.
   - **Yes** → Path B or C.
2. Does the user's intent match a recipe in `## Available tasks`?
   - **Yes** → Path A (no schedule) or Path B (scheduled).
   - **No, but the user wants a reminder** → Path C.
   - **No, and the user wants real work** → no Path A/B available;
     either decline ("no matching recipe — closest match would be
     to spawn a regular mission") or spawn a mission with a
     suitable mission skill instead. Do NOT invent a `task_skill`
     name that isn't in `## Available tasks`.
3. Pre-fill `task_inputs` / `inputs` from what the user already
   said. The recipe / input-collector handles the rest in Path A;
   in Path B you ask up-front yourself.

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
- `cron` — full cron expression. Land in 6.2; today returns
  `not_yet_implemented` if used.

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

After successful `task:create`, emit a short user-visible
acknowledgement naming the schedule (≤ 1 sentence, match the
user's language). After Path A's `spawn_mission`, the regular
async-result projection handles the user-facing reply when the
recipe completes — no extra acknowledgement needed at spawn time.

## What this skill does NOT grant

- `session:spawn_subagent` / `session:spawn_wave` /
  `session:notify_subagent` / `session:parent_context` —
  removed under mission-PDCA (design 003). Root delegates
  singularly via `spawn_mission`; mid-mission notify is now
  `mission:notify`.
- `plan:set` / `plan:clear` / `whiteboard:init` — root reads
  but does not own a plan body or whiteboard channel. PDCA
  missions don't use the whiteboard at all.
