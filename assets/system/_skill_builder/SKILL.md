---
name: _skill_builder
description: >
  Knowledge + capability for authoring a hugen skill bundle and
  registering it with skill:save. Teaches the manifest format (where
  the task block goes), the on-disk bundle layout, the path-based
  skill:save call + every validation error and its fix, and how to
  look up real tool names instead of inventing them. Load it whenever
  you build, update, or remove a skill.
license: Apache-2.0
allowed-tools:
  - skill:save
  - skill:export
  - skill:uninstall
  - skill:load
  - skill:unload
  - skill:files
  - skill:ref
  - skill:catalog_list
  - tool:providers
  - tool:tools
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

# _skill_builder

Load this skill when you need to **author, update, or remove a hugen
skill**. It grants the authoring surface (`skill:save`,
`skill:uninstall`, `tool:providers`, `tool:tools`, …) and bundles the
canonical references so you build the bundle correctly the first time
instead of guessing the format.

You are NOT the policy owner: a mission (`_task_builder`) or the user
decides *that* a skill should be saved and *what* it should do. This
skill owns *how* — the format and the call.

## The authoring loop (always these three steps)

1. **Build the bundle as files** in your session workspace — a
   directory holding `SKILL.md` plus optional `references/`,
   `scripts/`, `assets/` subdirs. Write the files with the bash /
   filesystem tools (relative paths resolve against your session
   workspace). See `references/bundle-layout.md`.

2. **Self-validate** with a dry run — `skill:save(bundle_dir: "<dir>",
   validate_only: true)`. This runs the full check (manifest parse +
   task-block placement + tool-name check) and returns the verdict
   WITHOUT registering. Fix every reported problem in the files and
   re-run until it returns `valid: true`.

3. **Register** — `skill:save(bundle_dir: "<dir>")`. On success the
   skill is written to the store and auto-loaded in your session.
   Collision (name already exists) returns an error: ask the user,
   then either pick a new name or pass `overwrite: true` to update.

`bundle_dir` may be relative (resolved against your session
workspace) or absolute (it must stay inside the workspace). Full call
detail + every error and its fix: `references/save-call.md`.

## The one mistake that produces a dead skill

A **task** skill (one that `schedule:create` / `task:<name>` can run)
MUST carry its task config under `metadata.hugen.task`, with
`eligible: true` nested inside that block:

```yaml
metadata:
  hugen:
    task:
      eligible: true
      kind: worker
      goal_summary: ...
      inputs_schema: { ... }
      allowed_tools_default: [ ... ]
```

Writing a **top-level** `task:` key, or flattening it to
`metadata.hugen.task_eligible`, makes the manifest PARSE but NOT be
task-eligible — a silent dud. `skill:save` now rejects both shapes
with a "task block misplaced" error naming what it found; if you see
it, move the whole block under `metadata.hugen.task`. See
`references/manifest-format.md`.

## Never invent tool names

`allowed_tools_default` (and any `allowed-tools` grant) entries are
exact `provider:tool` names from the LIVE registry — never a skill
name, never a guess. `hugr-data:execute` and `python-runner:run` are
NOT tools (those are skill names). Look up the real names first:

- `tool:providers()` → the registered providers + tool counts.
- `tool:tools(provider: "<name>")` → that provider's real tool names
  (add `detailed: true` for argument schemas).

`skill:save` rejects unknown names with `skill_unknown_tool` and a
hint. See `references/tool-discovery.md`.

## Updating an existing skill

To change a skill that already exists, edit its real bundle — don't
re-author from memory:

1. **Export it** — `skill:export(name: "<name>")` copies the skill's
   SKILL.md + references / scripts / assets into a directory in your
   workspace (defaults to the skill name; pass `dest_dir` to choose).
   The result gives you `dir` + the file list.
2. **Edit** the files in that directory with the bash / filesystem
   tools — change only what needs changing.
3. **Re-register** — `skill:save(bundle_dir: "<dir>", overwrite:
   true)`. Validate first with `validate_only: true` if you touched
   the manifest. Overwrite is the ONLY update path; there is no
   in-place edit call.

## Removing a skill

`skill:uninstall(name: "<name>")` — removes it from the store
entirely. Destructive and approval-gated. Prefer the export → edit →
overwrite-save flow for fixes; uninstall is for retiring a skill you
no longer want.

## What this skill does NOT do

- Does NOT decide that a skill should be saved — the user / mission
  owns that. You execute the authoring once asked.
- Does NOT publish to a remote / shared catalogue — `skill:save` is
  local-store only (hub publish lands with hub integration).
- Does NOT replace the memory layer — that's for implicit pattern
  reuse; this is for explicit, curated skills.
