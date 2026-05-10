# Bundle layout — agentskills.io structure

A skill bundle is a directory with a known shape. `skill:save`
accepts a typed bundle that maps to this layout on disk.

## Directory shape

```text
<skill-name>/
├── SKILL.md              — manifest (frontmatter) + body markdown
├── references/           — deep-dive notes (markdown)
│   ├── howto.md
│   └── deep/dive.md      — nested allowed
├── scripts/              — executable artefacts
│   ├── query.py
│   └── render.sh
└── assets/               — text data (templates, configs)
    └── template.html.tmpl
```

`SKILL.md` is required. Every other directory is optional.
Subdirectories within `references/`, `scripts/`, `assets/` are
allowed (e.g. `references/deep/dive.md`).

## `skill:save` argument shape

```json
{
  "skill_md":   "---\nname: ...\n---\n\nbody",
  "references": {"howto.md": "...", "deep/dive.md": "..."},
  "scripts":    {"query.py": "..."},
  "assets":     {"template.html.tmpl": "..."},
  "overwrite":  false
}
```

Map keys are RELATIVE paths within the category directory. The
tool prepends the category prefix (`references/`, `scripts/`,
`assets/`) automatically — do NOT include it in the key.

| Key       | Becomes on disk                    |
|-----------|------------------------------------|
| `howto.md`              | `<skill-dir>/references/howto.md` |
| `deep/dive.md`          | `<skill-dir>/references/deep/dive.md` |
| `query.py` (in scripts) | `<skill-dir>/scripts/query.py` |

## Path safety rules

`skill:save` rejects unsafe keys with `ErrInvalidPath`. The rules:

- Empty path — no.
- Absolute path (leading `/`) — no.
- Parent-dir reference (`..` segment) — no.
- Hidden segment (starts with `.` like `.env`) — no.
- NUL byte or backslash — no (cross-OS safety).
- Non-normalised path (`foo//bar`, trailing `/`, `./foo`) — no.

OK examples: `foo.md`, `sub/foo.py`, `a-b_c.md`, `templates/r1.tmpl`.

## What lives where

- **`SKILL.md` body** — procedure description: when to use the
  skill, what parameters to ask the user for, what scripts to
  run, what tools to call. The body is auto-rendered into the
  system prompt when the skill loads. Keep it readable but
  complete — the model uses it as a recipe.

- **`references/`** — auxiliary documentation the model reads
  on-demand via `skill:ref`. Use for:
  - Long-form explanations that would bloat the body.
  - Schema cheatsheets.
  - Worked examples.
  - Edge-case handling notes.
  These are NOT auto-injected into the prompt; they sit on disk
  until the model decides to read one.

- **`scripts/`** — executable artefacts the procedure invokes.
  Python, SQL, shell — whatever fits. Reference them from the
  body as `${SKILL_DIR}/scripts/foo.py`. The runtime exposes
  `${SKILL_DIR}` via the loaded-skills meta block in the system
  prompt.

- **`assets/`** — text data files: templates, sample configs,
  reference data the scripts consume. NOT for binaries (v1).

## `${SKILL_DIR}` substitution

The runtime does NOT auto-substitute `${SKILL_DIR}` in tool args.
Read the actual `directory:` value from the "Loaded skill bundles"
block in your system prompt and inline it when you emit
`bash:run` / `python:run_script` calls. Example:

System prompt shows:

```text
Loaded skill: `material-movement-report`
  directory: /home/user/.local/state/hugen/skills/local/material-movement-report
  scripts:
    - scripts/query_movement.py
```

You emit:

```json
bash:run {
  "command": "python /home/user/.local/state/hugen/skills/local/material-movement-report/scripts/query_movement.py --material XYZ-9999"
}
```

Body markdown can use the `${SKILL_DIR}` placeholder for clarity
(humans reading the saved skill see the intent), but you, the
model, substitute the actual path at call time.

## Manifest essentials

Required frontmatter fields:

```yaml
name: kebab-case-name           # [A-Za-z0-9_-]{1,64}
description: |                  # one-line distinctive description
  short, specific, used for discovery
license: Apache-2.0             # SPDX identifier
allowed-tools:                  # explicit grants
  - bash:run
  - python:run_script
  - hugr-main:discovery-list_collections
```

For full field list see `manifest-cheatsheet.md`.

## What NOT to bundle

- The user's actual data values (parameterise instead).
- Secrets or credentials.
- Binary assets (v1 doesn't support them; binaries get rejected).
- Tools that only worked because of a one-off session state
  (e.g. a python venv path that won't exist next session).
