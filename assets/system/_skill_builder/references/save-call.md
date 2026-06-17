# skill:validate / skill:save — the calls, the verdict, every error

Authoring is two path-based tools — you build the bundle as files,
then point the calls at the directory:

- **`skill:validate`** — the dry-run CHECK. Runs the full validation
  and returns the verdict WITHOUT writing. It cannot register; use it
  to iterate until the bundle is clean.
- **`skill:save`** — the REGISTER. Re-runs validation atomically, then
  writes the bundle to the store and auto-loads it. Validation runs
  BEFORE any write, so a rejected save changes nothing.

Always `skill:validate` until `valid: true`, then `skill:save` once.

## Arguments

```
skill:validate(
  bundle_dir: "<dir>"       # required — dir with SKILL.md (+ references/scripts/assets)
)

skill:save(
  bundle_dir: "<dir>",      # required — same bundle
  overwrite:  <omitted>     # see below — OMIT it and a name collision asks the user
)
```

- `bundle_dir` — relative (resolved against your session workspace) or
  absolute (must stay inside the workspace). Must contain `SKILL.md`.
- `overwrite` — OMIT it (the default) and a name collision pauses to
  ASK the user (overwrite / save under a new name / cancel); it never
  clobbers silently. Pass `true` only when the user has authorised
  replacing the existing skill (e.g. an export→edit→re-save update);
  pass `false` to hard-fail a collision without asking.

## The verdict

`skill:validate` returns `{ name, directory, files, validate_only:
true, valid: true }`. `skill:save` returns `{ name, directory, files,
valid: true }` after registering (the skill is auto-loaded in your
session). `files` is the bundle's file list — use it to confirm the
right scripts / references were picked up.

## Errors and how to fix each

Every error is actionable. Read the message, fix the FILES in your
workspace, re-run `skill:validate` (then `skill:save`).

- **manifest does not parse** — the SKILL.md frontmatter is malformed
  YAML, or a required field (`name`, `description`) is missing. Fix
  the frontmatter.

- **task block misplaced** — a top-level `task:` key, or a flat
  `metadata.hugen.task_eligible`. Move the whole task config under
  `metadata.hugen.task` with `eligible: true` nested in it. See
  `manifest-format.md`.

- **`skill_unknown_tool`** — an `allowed_tools_default` entry is not a
  real `provider:tool` in the registry. The message names the bad
  entry and, when the culprit is a skill name used as a provider,
  says so. Look up real names with `tool:providers` / `tool:tools`
  (see `tool-discovery.md`) and replace the invented one.

- **autoload reserved** — the manifest sets `metadata.hugen.autoload:
  true`. Drop it; local skills load on demand.

- **`ErrSkillExists` / already in the store** — the name is taken.
  With `overwrite` omitted, `skill:save` already asked the user and you
  are acting on their answer: if they chose a NEW name, change `name:`
  in SKILL.md and re-save; if they cancelled, stop. This error only
  surfaces directly when you passed `overwrite: false` explicitly —
  rename or re-save with the user's authorisation.

- **bundle_dir escapes the workspace / does not exist / no SKILL.md** —
  a path problem. Build the bundle inside your session workspace and
  make sure `<dir>/SKILL.md` exists.

## Update vs remove

- **Update** a saved skill: re-run `skill:save(bundle_dir, overwrite:
  true)` with the revised bundle. No separate edit call.
- **Remove** it: `skill:uninstall(name)` — destructive, approval-gated.
  On a local-only store with no removable backend it returns
  `skill_uninstall_unsupported`; use overwrite-save to update instead.
