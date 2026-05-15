# Save protocol — full procedure

Triggered by an explicit user request to crystallise current session
work as a reusable local skill. Phrases that fire the protocol:

- "сохрани это как скилл / save this as a skill"
- "давай сделаем скилл что бы ... / let's make a skill that ..."
- "запомни этот процесс / remember this procedure"

Run the steps in order. Skipping the post-save validation is not
allowed — a skill that hasn't been validated end-to-end is not done.

## 1. Clarify in normal conversation

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

## 2. Compose the bundle

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
skill bundles" → `directory:`. Use it as the on-disk path. The
exact bash / python tool name depends on operator config — read
your tool catalogue before composing the call. Generic shape:

```text
<bash-provider>:<run-tool> python ${SKILL_DIR}/scripts/query_movement.py --material {material_code}
```

Common defaults: `bash:run`, `bash-mcp:bash_shell`, `python:run_script`.
Never hard-code one provider name in the saved body — let the
future model substitute its actual tool name from the catalogue
at the call site.

**Ground `allowed-tools` against the deployment.** Call
`skill:tools_catalog` to confirm tool names actually exist. Don't
guess `python-mcp:run_code` if the actual provider is
`python:run_code`.

**Do NOT request `autoload: true`.** Autoload is reserved for
system / admin skills. Local skills load on demand — the user
explicitly `/skill load`s them in future sessions. The
`skill:save` tool rejects autoload manifests with
`ErrAutoloadReserved`.

## 3. Save itself

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

## 4. Mandatory validation after save

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

## 5. Naming collisions

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

The `tool_result` envelope from a failed skill:save carries
`{is_error: true, code: <code>, message: <text>}`. Branch on
`code` — each one points at a specific recovery path:

| `code`                | Meaning | Action |
|-----------------------|---------|--------|
| `skill_bad_manifest`  | SKILL.md doesn't parse or lacks required fields | Fix manifest, re-save. |
| `skill_autoload`      | Manifest has `metadata.hugen.autoload: true` | Remove the autoload field, re-save. Autoload is reserved for system / admin skills. |
| `skill_exists`        | Name collision and `overwrite: false` | **ASK THE USER explicitly** before retrying with `overwrite: true`, OR pick a different name. Do NOT silently retry — this is the load-bearing collision protocol. |
| `skill_bad_path`      | A key in references / scripts / assets is unsafe | Use simple `name.ext` keys (sub/dir.ext is fine). No `..`, no leading `/`, no hidden segments (`.env`). |
| (anything else)       | Unexpected error | Surface it to the user and ask how to proceed. |

**Important**: a failed skill:save is NOT a no-op. The
returned envelope has `is_error: true` — treat it as an error
that needs recovery, never as "the operation completed
silently". If the message text mentions "already exists",
"autoload", "manifest", or "path", branch as above.
