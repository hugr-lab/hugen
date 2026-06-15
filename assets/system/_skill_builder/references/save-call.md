# skill:save — the call, its verdict, and every error

`skill:save` is path-based: you build the bundle as files, then point
the call at the directory. Validation runs BEFORE any write, so a
rejected save changes nothing — fix the files and re-call.

## Arguments

```
skill:save(
  bundle_dir:    "<dir>",   # required — dir with SKILL.md (+ references/scripts/assets)
  validate_only: false,     # true = dry-run, validate + return verdict, do NOT register
  overwrite:     false       # true = replace an existing skill of the same name
)
```

- `bundle_dir` — relative (resolved against your session workspace) or
  absolute (must stay inside the workspace). Must contain `SKILL.md`.
- `validate_only` — run the full check and return the verdict without
  registering. Always do a `validate_only: true` pass first.
- `overwrite` — only set after a collision, and only with the user's
  agreement (or inside your own validate→fix→save iteration loop).

## The verdict

On success the result carries `{ name, directory, files, valid }`
(plus `validate_only: true` on a dry run). `files` is the bundle's
file list — use it to confirm the right scripts / references were
picked up. After a real save the skill is auto-loaded in your session.

## Errors and how to fix each

Every error is actionable. Read the message, fix the FILES in your
workspace, re-run `skill:save`.

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

- **`ErrSkillExists` / already in the store** — the name is taken. Ask
  the user, then either rename the skill or re-call with
  `overwrite: true`. Do NOT silently retry.

- **bundle_dir escapes the workspace / does not exist / no SKILL.md** —
  a path problem. Build the bundle inside your session workspace and
  make sure `<dir>/SKILL.md` exists.

## Update vs remove

- **Update** a saved skill: re-run `skill:save(bundle_dir, overwrite:
  true)` with the revised bundle. No separate edit call.
- **Remove** it: `skill:uninstall(name)` — destructive, approval-gated.
  On a local-only store with no removable backend it returns
  `skill_uninstall_unsupported`; use overwrite-save to update instead.
