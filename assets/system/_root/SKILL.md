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

## Knob 7 — skill-save trigger (pre-classification)

Before deciding chat vs spawn, scan for explicit save phrases:
"сохрани это как скилл / save this as a skill", "давай сделаем
скилл что бы / let's make a skill that", "запомни этот процесс
/ remember this procedure". If matched, do NOT proceed with the
normal default — `skill:load(name: "_skill_builder")` and
follow its save protocol. A save request that gets
`spawn_mission`'d as a new task is a classifier failure.

## What this skill does NOT grant

- `session:spawn_subagent` / `session:spawn_wave` /
  `session:notify_subagent` / `session:parent_context` —
  removed under mission-PDCA (design 003). Root delegates
  singularly via `spawn_mission`; mid-mission notify is now
  `mission:notify`.
- `plan:set` / `plan:clear` / `whiteboard:init` — root reads
  but does not own a plan body or whiteboard channel. PDCA
  missions don't use the whiteboard at all.
