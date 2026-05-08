---
name: _planner
description: Persistent plan + comments primitive for long autonomous tasks. Survives history compaction and process restarts.
license: Apache-2.0
allowed-tools:
  - provider: plan
    tools:
      - set
      - comment
      - show
      - clear
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [root]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _planner skill

The _planner skill lets you anchor your work-in-progress in a small,
durable artefact that survives every form of context loss the
runtime experiences: history truncation, session resume after
process restart, even a hard crash. The runtime re-renders your
plan into the system prompt at the top of every turn — the model
always sees its own plan first, before any conversation history.

## When to use a plan

Always for tasks where the work spans more than a handful of turns
or more than one fan-out / wait cycle. Examples:

- multi-step investigation that branches into sub-agents and merges
  their findings;
- an autonomous run with more than ~10 expected tool calls;
- any task where the user described a goal that itself contains
  sub-goals.

Skip the plan for one-shot Q&A and trivial single-tool requests —
the overhead is not worth it.

## The four tools

- `plan:set` — write or replace the body. Wipes the
  in-memory comment log; events stay for audit. Use this at the
  start of a task, and any time the goal materially changes (a
  scope expansion, a pivot, a new constraint from the user).
- `plan:comment` — append a one-line progress note. Optional
  `current_step` moves the focus pointer. Use at every meaningful
  inflection: tool call returned a key result, sub-agent spawned,
  branch ruled out.
- `plan:show` — read back the full plan plus retained
  comments. Use when you've drifted long enough that you may have
  forgotten what you wrote.
- `plan:clear` — drop the plan when the task is genuinely
  done. After clear the prompt block disappears.

## What the plan looks like in the prompt

When a plan is active the runtime prepends a block like this to the
top of the system prompt on every turn:

```text
## Active plan
Current focus: <current_step>

<body>
```

Comments are NOT rendered — they live in the events log and
`plan:show` retrieves them on demand. This keeps the per-turn
overhead bounded; a 30-comment log doesn't bloat every prompt.

## Caps and what they mean

- 8 KB body — generous; if you're approaching this you're probably
  trying to use the plan as a notebook. Use `notepad:append` for
  detail; keep the plan body focused on the goal and the high-level
  shape of the work.
- 2 KB per comment — about a paragraph. Same advice: keep it
  one-line-ish; longer prose belongs in the notepad.
- 30 comments retained — older comments stay in the events log
  (`subagent_runs` / future `events_search` can find them) but
  drop out of `plan:show` and the projection.

When you exceed any cap the runtime appends a small truncation
marker in the visible projection.

## Working with sub-agents

If you spawn sub-agents that themselves do significant work, opt
those sub-agent skills into `_planner` via `requires_skills:
[_planner]` so each child has its own independent plan. The plans
do not share state — every session has its own.

## What this skill does NOT grant

- The `_planner` skill itself doesn't authorise sub-agent spawn,
  whiteboard, or any other orchestration primitive — it's a pure
  planning surface. Combine with `_root` / `_subagent` / `_whiteboard`
  for the rest.
