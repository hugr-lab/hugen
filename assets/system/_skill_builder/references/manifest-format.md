# SKILL.md manifest format

A skill's `SKILL.md` is YAML frontmatter (between `---` fences)
followed by a markdown body. The body is the skill's prose — what the
model reads after `skill:load`. The frontmatter is the typed manifest.

## Minimum frontmatter (any skill)

```yaml
---
name: roads-by-region
description: >
  One or two sentences — what the skill does and when to use it.
  This is what the catalogue and semantic recall match against.
license: Apache-2.0
metadata:
  hugen:
    requires_skills: []          # other skills loaded transitively
    autoload: false              # MUST stay false — autoload is reserved
    tier_compatibility: [root, mission, worker]
---
```

- `name` — kebab-case, unique in the store. The save-time tool name
  becomes `task:<name>` for task skills.
- `description` — the discovery surface. Write it for matching, not
  for prose flavour.
- `autoload` — leave `false`. Setting it true is rejected
  (autoload is reserved for system skills compiled into the binary).
- `tier_compatibility` — which tiers may load the skill (`root`,
  `mission`, `worker`). A worker-run task is usually `[worker]` or
  `[root, mission, worker]`.

## allowed-tools (granting tools to the skill)

`allowed-tools` lists the tools the skill makes available when
loaded. Flat `provider:tool` entries or grouped form both work:

```yaml
allowed-tools:
  - bash-mcp:bash.shell
  - python-mcp:run_script
# or grouped:
allowed-tools:
  - provider: bash-mcp
    tools: [bash.shell, bash.read_file]
```

Every entry is an EXACT `provider:tool` name from the live registry.
Look them up with `tool:providers` / `tool:tools` — never invent.
See `tool-discovery.md`.

## The task block (task-eligible skills only)

A skill that `schedule:create` or a `task:<name>` call can run is a
**task skill**. Its task config lives under `metadata.hugen.task` —
NOT at the top level, NOT flattened:

```yaml
metadata:
  hugen:
    requires_skills: []
    autoload: false
    tier_compatibility: [worker]
    task:
      eligible: true             # the master flag — INSIDE the task block
      kind: worker               # worker (default) — mission is reserved
      goal_summary: >
        Build the roads-by-region report.
      inputs_schema:             # JSON Schema for the inputs blob
        type: object
        required: [region]
        properties:
          region:      { type: string, description: region filter }
          output_path: { type: string, description: where to write the report }
      allowed_tools_default:     # pre-filled tool allow-list for the run
        - bash-mcp:bash.shell
        - python-mcp:run_script
      body_is_template: false    # true only if the body uses {{ }} actions
```

### The dead-skill mistakes (both rejected by skill:save)

1. **Top-level `task:`** — the block at the document root instead of
   under `metadata.hugen.task`. The decoder silently drops it; the
   skill parses but is not task-eligible.

   ```yaml
   # WRONG — dropped on parse
   task:
     eligible: true
   ```

2. **Flat `task_eligible`** — `metadata.hugen.task_eligible: true`
   instead of `metadata.hugen.task.eligible: true`. Same silent dud.

   ```yaml
   # WRONG — unknown key, dropped
   metadata:
     hugen:
       task_eligible: true
   ```

Both surface as a "task block misplaced" error from `skill:save`
naming exactly what it found. The fix is always: move the whole task
config under `metadata.hugen.task` with `eligible: true` nested in it.

## Body referencing bundled files

The body addresses bundled scripts / assets through `${SKILL_DIR}`,
the on-disk root of the loaded skill:

```markdown
Run the report generator:
`python-mcp:run_script` on `${SKILL_DIR}/scripts/report.py`
```

`skill:files(name: "<skill>")` lists the absolute paths after load, so
a worker can read or execute them.
