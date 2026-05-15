# Manifest cheatsheet — SKILL.md frontmatter

The frontmatter sits between two `---` lines at the top of
SKILL.md, in YAML. Below: every recognised field, with whether
it's required for `skill:save` to accept the manifest.

## Required fields

```yaml
---
name: my-skill
description: |
  Short distinctive description used by discovery.
license: Apache-2.0
---
```

| Field         | Constraint |
|---------------|-----------|
| `name`        | `[A-Za-z0-9_-]{1,64}`. Match the directory name. |
| `description` | non-empty, max 1024 chars. Make it specific. |
| `license`     | SPDX identifier. `Apache-2.0` is the standard for hugen-bundled. |

## `allowed-tools` — tri-state

```yaml
allowed-tools:
  - bash:run
  - python:run_script
  - hugr-main:discovery-list_collections
```

Three states matter for tools_catalog discovery:

| YAML form                  | Meaning |
|----------------------------|---------|
| `allowed-tools:` absent    | Inherits union of other loaded skills' grants. Appears in `available_in_skills` for every tool — useful for community "do not restrict" skills. |
| `allowed-tools: []`        | Reference-only skill. Contributes no tools. Appears in catalogue but not in `available_in_skills`. |
| `allowed-tools: [grants]`  | Explicit grants. Skill admits exactly these tools when loaded. |

For `skill:save` always be explicit — list the tools the
procedure actually invokes. Discovery for your saved skill
works best when grants are specific; absent-grant local skills
get noisy in `available_in_skills`.

### Two notations supported

```yaml
# Flat form — preferred for hugen-authored skills:
allowed-tools:
  - bash:run
  - hugr-main:discovery-list_collections

# Grouped form — agentskills.io standard:
allowed-tools:
  - provider: bash
    tools: [run]
  - provider: hugr-main
    tools: [discovery-list_collections]
```

Mix freely; the parser merges flat entries by provider.

### Wildcard

`provider:*` admits every tool from the provider:

```yaml
allowed-tools:
  - hugr-main:*
```

Use for skills that need broad access to a domain. Specific
patterns are preferred when feasible.

## `metadata.hugen.*` — hugen-specific extensions

```yaml
metadata:
  hugen:
    requires_skills: [_planner]      # transitive deps
    intents: [default, cheap]        # model router intents this skill works on
    sub_agents:                      # role declarations for spawn_subagent
      - name: explorer
        description: ...
        intent: cheap
        can_spawn: false
        tools: [hugr-main:*]
        max_tool_turns: 8            # per-role per-invocation cap (worker tier)
        max_tool_turns_hard: 16      # per-role hard ceiling
        stuck_detection:
          enabled: true
    mission:
      enabled: true
      summary: ...
      max_tool_turns: 12             # per-mission cap (mission tier)
      max_tool_turns_hard: 24        # per-mission hard ceiling
    autoload: false                  # NEVER true for skill:save manifests
    autoload_for: [root]             # ignored when autoload:false
```

| Field | Purpose |
|-------|---------|
| `requires_skills` | List of skill names to auto-load alongside this one. Resolved transitively at load time. |
| `intents` | Model router intents this skill is compatible with. Empty = any. |
| `sub_agents` | Declared roles for `session:spawn_subagent`. Per-role `max_tool_turns` / `max_tool_turns_hard` / `stuck_detection` override the worker-tier defaults. |
| `mission.max_tool_turns` / `mission.max_tool_turns_hard` | Per-mission turn-loop caps for the mission-tier session itself. Operator-wide defaults live in `config.subagents.tier_defaults.<tier>`. |
| `autoload` | **Forbidden in `skill:save` — set to false or omit.** Reserved for system / admin skills. |
| `autoload_for` | Which session types autoload triggers in. Ignored when `autoload:false`. |

## Common skill:save manifest

A typical `skill:save` manifest looks like:

```yaml
---
name: material-movement-report
description: PDF-отчёт о движении материала в модуле логистики (parameter material_code).
license: Apache-2.0
allowed-tools:
  - hugr-main:discovery-list_collections
  - hugr-main:data-validate_graphql_query
  - bash:run
  - python:run_script
metadata:
  hugen:
    requires_skills: []
---
```

Note: no `autoload`, no `autoload_for`. `requires_skills: []` is
explicit (vs absent) — both work. Per-tier turn-loop budgets come
from `config.subagents.tier_defaults`; per-role / per-mission
overrides go on `sub_agents[*].max_tool_turns` and
`mission.max_tool_turns`.

## Validation

`skill:save` runs `pkg/skill.Manifest.Parse`. Failures wrap
`ErrManifestInvalid`. Common rejections:

- `name does not match [A-Za-z0-9_-]{1,64}` — fix name format.
- `description is required` — add description.
- `description length N exceeds 1024 chars` — shorten.
- `allowed-tools[i].provider is required` — grouped-form entry
  missing provider.

Plus the `skill:save`-specific rejection:

- `ErrAutoloadReserved` — manifest sets `autoload: true`. Remove it.

## What NOT to put in manifest

- Specific user data (`material_code: "AB-1234"`) — parameterise
  via body instructions, not manifest fields.
- Secrets / credentials.
- Tool grants the procedure doesn't actually use.
- `autoload: true` — rejected.
