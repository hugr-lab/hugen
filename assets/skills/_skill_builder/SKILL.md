---
name: _skill_builder
description: Two protocols. Discovery — find unloaded local skills via skill:tools_catalog before composing procedures from scratch. Save — persist session work as a reusable local skill bundle on explicit user request, with mandatory post-save validation.
license: Apache-2.0
allowed-tools:
  - skill:save
  - skill:load
  - skill:unload
  - skill:tools_catalog
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [root]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _skill_builder

Two protocols this skill teaches you:

1. **Discovery** — before composing a procedure from scratch for any
   non-trivial task, check whether a saved local skill already covers
   it. Local skills do **not** autoload; you find them through
   `skill:tools_catalog`.
2. **Save** — when the user explicitly requests crystallising the
   current session's work into a reusable skill, follow the save +
   validate + collision-handling protocol below.

References for deep-dives: `discovery-flow.md`, `bundle-layout.md`,
`manifest-cheatsheet.md`, `validation-protocol.md`. Read them via
`skill:ref` when you need detail.

## Discovery — first move on a non-trivial request

When a user request looks like it might match an existing local
skill (analytical / reporting / data-fetch patterns — not pure
conversational answers):

1. Call `skill:tools_catalog(pattern: <distinctive keyword>)` where
   the keyword is a domain term from the request ("movement",
   "report", "summary", a module name, etc.).
2. Scan the result for entries with non-empty `available_in_skills`.
3. If a listed skill name looks like a fit (matches the request
   shape, name is plausible):
   - `skill:load <name>`.
   - Your prompt now shows a "Loaded skill bundles" block with
     `directory:` and bundled files (scripts/references/assets).
   - Read the saved skill body and follow its procedure with the
     user's actual inputs.
4. If nothing matches, proceed with composing from scratch.

This is the ONLY way local skills get used in fresh sessions — they
do not autoload. If you skip discovery, the user will repeat work
they already crystallised. See `discovery-flow.md` for worked
examples.

## Save — only on explicit user request

Do **not** propose this yourself — the user owns this decision. If
the user is happy with one-time output, leave it. The save action
is triggered by phrases like:

- "сохрани это как скилл / save this as a skill"
- "давай сделаем скилл что бы ... / let's make a skill that ..."
- "запомни этот процесс / remember this procedure"

### Before save — clarify in normal conversation

Before composing the bundle, ask the user:

- **Skill name** — derive a kebab-case name from the request domain
  (e.g. `material-movement-report`); confirm with the user.
- **Distinctive description** — used by future-you to decide
  whether to load this skill via `available_in_skills`. Make it
  specific. "PDF-отчёт о движении материала в модуле логистики" —
  good. "отчёт" — bad.
- **What to parameterise** — example:
    user: «движение материала AB-1234 в логистике»
    you:  «Параметризуем по material code? Или ещё по дате?»
- **Which session artefacts** (scripts, query templates, render
  templates) to bundle alongside the procedure body.

### Compose the bundle

Use the agentskills.io layout (see `bundle-layout.md` for detail):

```text
SKILL.md            — manifest + procedure (body)
references/*.md     — deep-dive notes
scripts/*           — executable artefacts (python, sql, sh, ...)
assets/*            — text data (templates, configs)
```

**Generalise.** Do NOT hard-code specific values from this session
into the body or scripts. Use placeholders / arguments:
`{material_code}`, `{date_from}`. The skill must work for ANY input
matching the parameter shape, not just what the user did right now.

**Reference bundled files in body via `${SKILL_DIR}`.** The loaded
skill's directory appears in your system prompt under "Loaded
skill bundles" → `directory:`. Use it as the on-disk path:

```text
bash:run python ${SKILL_DIR}/scripts/query_movement.py --material {material_code}
```

**Ground `allowed-tools` against the deployment.** Call
`skill:tools_catalog` to confirm tool names actually exist. Don't
guess `python-mcp:run_code` if the actual provider is
`python:run_code`.

**Do NOT request `autoload: true`.** Autoload is reserved for
system / admin skills. Local skills load on demand — the user
explicitly `/skill load`s them in future sessions. The
`skill:save` tool rejects autoload manifests with
`ErrAutoloadReserved`.

### Save itself

Call `skill:save` with the structured bundle:

```json
{
  "skill_md":   "---\nname: ...\n---\n\nbody",
  "references": {"howto.md": "..."},
  "scripts":    {"query.py": "..."},
  "assets":     {"template.html": "..."},
  "overwrite":  false
}
```

The tool returns `{name, directory, files: [...]}` and **auto-loads**
the skill in the current session.

### Mandatory validation after save

The skill is auto-loaded after a successful save. Then **always**:

1. Construct test parameters — synthetic, NOT the user's actual
   data (e.g. `material_code: "TEST-001"`).
2. Run the skill's documented procedure end-to-end with those
   parameters via `bash:run` / `python:run_script` invoking
   `${SKILL_DIR}/scripts/...`.
3. If anything fails (script error, query error, missing tool
   admission, wrong output shape):
   a. `skill:unload <name>`
   b. Fix the issue (edit the bundle).
   c. `skill:save(..., overwrite: true)` — within this validation
      iteration loop, overwrite is authorised (the user is in an
      active skill-creation flow).
   d. Repeat from step 1.
4. Only after a clean test run, report success to the user:

   > Saved as `<name>`, tested with sample params, ready.
   > Load it in a future session via `/skill load <name>`.

A skill that hasn't been validated end-to-end is not done. See
`validation-protocol.md` for worked examples.

### Naming collisions

`skill:save` defaults to `overwrite: false`. If the tool returns
`ErrSkillExists`, do NOT retry silently. Ask the user:

> Skill `<name>` already exists with these files: <listing>.
> Overwrite, or pick a different name?

Wait for explicit user reply. Only then either:

- re-call `skill:save` with `overwrite: true`, or
- re-compose the bundle under the new name.

The `overwrite=true` exception during the validation iteration
loop above is exactly that — an exception inside an authorised
flow. First saves always honour the user's choice.

## Errors from skill:save

| Error | What it means | What to do |
|-------|---------------|------------|
| `ErrManifestInvalid` | SKILL.md doesn't parse / lacks required fields | fix manifest, re-save |
| `ErrAutoloadReserved` | manifest has `metadata.hugen.autoload: true` | remove autoload, re-save |
| `ErrSkillExists` | name collision and `overwrite=false` | ask user (see above) |
| `ErrInvalidPath` | a key in references / scripts / assets is unsafe | use simple `name.ext` keys, no `..`, no leading `/`, no leading `.` |

## What this skill does NOT do

- Does NOT publish to a remote / shared catalogue. `skill:save` is
  local-store only. Hub-mode publish lands with hub integration.
- Does NOT propose saves. The user initiates; you follow.
- Does NOT replace the future memory layer. Memory is for implicit
  pattern reuse; this skill is for explicit user-curated procedures.
