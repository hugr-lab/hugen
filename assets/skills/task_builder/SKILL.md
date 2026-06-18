---
name: task_builder
description: >
  Work with reusable TASKS — create a new one from the user's intent, or
  change / fix / update / delete an existing one. Load this whenever the
  user wants to build a repeatable task, or to edit, correct, rename, or
  remove a task that already exists. It is the single entry point for the
  task lifecycle: it drives an interactive builder for creation and the
  authoring skill for edits / removal; the heavy lifting lives in the
  tool and skill it pulls in.
license: Apache-2.0
allowed-tools:
  - task:build_task
  - task:search
  - task:describe
  - skill:load
  - skill:catalog_list
  - skill:ref
  - session:inquire
metadata:
  hugen:
    requires_skills: []
    autoload: false
    autoload_for: []
    tier_compatibility: [root, mission, worker]
compatibility:
  model: any
  runtime: hugen
---

# task_builder — work with reusable tasks

Load this skill when the user wants to **create**, **change**, **fix**,
or **delete** a reusable task. A task is a task-eligible skill the user
re-runs by name (and can later schedule). This skill is the single entry
point for the whole task lifecycle; it delegates the mechanics to the
tool and the skill it pulls in — you coordinate, they do the work.

## Create a NEW task

The user wants a repeatable piece of work that no existing task covers.

1. **Dedup first.** `task:search(query: <the user's request>)`. If a
   task already does the WORK, tell the user to reuse it (run it by name
   / schedule it) and stop — a duplicate is the worst outcome. Unsure
   whether a near-match fits → `session:inquire` "reuse `<name>` or build
   a new one?".
2. **Run the builder.** Call `task:build_task` with the user's intent:

   ```
   task:build_task({
     inputs: {
       user_intent: "<the user's request, verbatim, in their language>",
       known_details: { ... }   # only facts the user already stated
     }
   })
   ```

   It is an interactive worker: it designs and AGREES the algorithm with
   the user, authors the bundle, validates it, smoke-tests it, confirms
   the result, and registers it. Its inquiries surface to the user in
   chat; when it returns, the new task is runnable by name (and
   schedulable).

   - `known_details` carries ONLY facts the user has ALREADY stated (a
     data source, filters, the output shape, a task name, a cadence) —
     each a CONCRETE answer, never a paraphrase of the request. Every
     key you pass is treated as already answered, so a guessed value
     silently suppresses a question the builder would otherwise ask.
     When in doubt, leave it out. Omit `known_details` entirely when the
     request carries no extra facts.
   - **Do NOT do the builder's work yourself.** Don't explore the data,
     load domain skills, discover a schema, or sketch the output here —
     discovering, designing, and authoring is exactly what the builder
     does (it has full catalogue access and asks the user). Pass the
     intent and STOP.

## Change / fix / update an existing task

The user wants to correct a value, change the output, fix a bug, or
otherwise edit a task that already exists. This is the authoring
surface. `skill:load` the **`skill_builder`** skill and follow its
"Updating an existing skill" flow:

export the task → edit the file that's wrong → run it once to verify
(smoke test) → `skill:save` with `overwrite: true`.

`skill_builder` owns the bundle format, the validate / save / export
calls, and the read-the-real-API + smoke-test discipline. Keep the edit
minimal — change only what the user named, and save under the SAME name.
For a wholesale rework (the task should do something materially
different), prefer building a fresh task via the create flow above.

## Delete a task

`skill:load` `skill_builder` and use its `skill:uninstall(name)` — it
removes the task from the store. Destructive and approval-gated; prefer
the edit flow above for fixes, and reserve uninstall for retiring a task
the user no longer wants.

## What this skill does NOT do

- It does NOT decide that a task should exist or be changed — the user
  does. You execute the lifecycle once asked.
- It does NOT author or hand-edit a bundle from memory — creation goes
  through `task:build_task`; the authoring mechanics live in
  `skill_builder`.
