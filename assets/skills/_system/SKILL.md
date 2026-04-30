---
name: _system
description: Built-in system skill granting the agent its baseline file-and-shell capabilities and the slash-command tools every session needs.
license: Apache-2.0
allowed-tools:
  - provider: bash-mcp
    tools:
      - bash.run
      - bash.shell
      - bash.read_file
      - bash.write_file
      - bash.list_dir
      - bash.sed
  - provider: system
    tools:
      - notepad_append
      - skill_load
      - skill_unload
      - skill_ref
metadata:
  hugen:
    requires: []
    sub_agents: []
    memory: {}
    autoload: true
    autoload_for: [root, subagent]
compatibility:
  model: any
  runtime: hugen-phase-3
---

# _system skill

The system skill is loaded into every session at boot. It binds the
agent to its baseline tool surface:

- **bash-mcp** — file-and-shell ops scoped to the session workspace
  (`/workspace/<sid>/`), the agent's shared area (`/shared/<aid>/`),
  and any operator-mounted `/readonly/<name>/` directories. Path
  resolution is symlink-canonicalised; writes under `/readonly/`
  are rejected.
- **system** — `notepad_append` for the per-session scratchpad and
  `skill_load` / `skill_unload` / `skill_ref` for in-loop skill
  management.

Operators can refine this surface via Tier-1 (config) or Tier-2
(Hugr role) rules — for example, denying `bash.shell` while
keeping `bash.run`. The system skill itself is never unloaded.
