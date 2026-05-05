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
  - provider: session
    tools:
      - notepad_append
      - skill_load
      - skill_unload
      - skill_ref
      - skill_files
  - provider: policy
    tools:
      - save
      - revoke
  - provider: tool
    tools:
      - provider_add
      - provider_remove
  - provider: system
    tools:
      - runtime_reload
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
agent to its baseline tool surface and tells you how to use it.

## bash-mcp — files & shell

bash-mcp lets you use real shell commands and filesystem
operations against the host. There is no virtual path layer —
shell tools and file tools see exactly the same paths.

1. **Your session scratch directory** — the cwd every tool call
   starts in. Available as `$SESSION_DIR` in shell commands so
   you can construct absolute paths reliably. It is private to
   this session and is wiped on close (unless the operator
   disabled cleanup). Use it for temporary files, scripts you
   write, downloaded data, intermediate artifacts.

2. **`$SHARED_DIR`** (env var, optional) — a real host path the
   operator designated as the user-visible exchange folder.
   Read it in shell commands as `$SHARED_DIR`. Empty or unset
   means there is no shared area in this deployment.
   - Files placed here are visible to the user outside the
     agent and persist across sessions.
   - Use it for the durable outputs the user explicitly asks
     for (reports, cleaned datasets, generated documents).

3. **The rest of the host filesystem** — anything else you can
   reach by absolute path. In container deployments the kernel
   confines you to the bind-mounted paths. In local / dev
   deployments you are running under the user's own filesystem
   permissions; behave accordingly.

### Behaviour rules

- **Stay in your scratch directory by default.** Every tool call
  starts there. Don't write to peer sessions' workspaces (sibling
  directories) — they belong to other conversations.
- `cd` inside a single `bash.shell` invocation works for that
  call only. The next tool call starts back at scratch. Pass
  `cwd: "subdir"` to start a tool call in a sub-directory.
- Writes that need to outlive the session go to `$SHARED_DIR`
  (when configured). Everything else stays in scratch.
- All shell binaries on the host PATH are available — `du`,
  `find`, `grep`, `sed`, `awk`, `python`, `git`, etc. Use them
  freely; bash-mcp does not restrict them.
- When the user asks "what files do you see", check both your
  scratch dir and `$SHARED_DIR` before reporting "empty".

## system — meta tools

- `notepad_append` — append to the per-session scratchpad. Use it
  to log intermediate findings the user may ask about later.
- `skill_load` / `skill_unload` — load or release a skill mid-
  session. Inspect the available skills index in your system
  prompt before loading.
- `skill_ref` — read a reference document that ships with a
  loaded skill (`references/<name>.md`).
- `skill_files` — list the on-disk files of a loaded skill with
  relative + absolute paths so other tools (`bash.read_file`,
  `python-mcp:run_script`, `duckdb-mcp:execute_query`) can address
  them directly. Optional `subdir` / `glob` filters narrow the
  listing.
- `policy:save` / `policy:revoke` — persist or remove a personal
  Tier-3 tool policy ("always allow" / "always deny") for the
  caller. Args: `tool_name` (`<provider>:<field>`, glob `*`
  suffix accepted), `decision` (`allow|deny|ask`), optional
  `scope` (default `global`) and `note`. Tier 3 NEVER overrides
  the operator floor or the user's role rules — when the user
  asks "always allow X", call this; if X is later denied by a
  higher tier the call still blocks (that's correct behaviour).
- `runtime_reload` — re-read live runtime state. `target` ∈
  `permissions` (re-fetch Hugr role rules), `skills` (rescan
  skill stores), `mcp` (re-spawn per-agent MCP providers), or
  `all`. Use only when the user explicitly asks to refresh.
- `tool:provider_add` / `tool:provider_remove` — admin path to
  register or drop a tool provider at runtime. Operator-only; the
  call may be denied by policy. Use `runtime_reload(target=mcp)`
  to restart already-registered MCP providers.

## Discovering skill contents

A loaded skill's body (this SKILL.md) is the entry point but rarely
the whole story. Skills ship references, sample data, scripts, and
templates under their own filesystem root. The standard discovery
flow:

1. **`skill_files(name="<skill>")`** — list every file the skill
   ships, with relative + absolute paths, size, and mode. Optional
   `subdir` (e.g. `"references"`) and `glob` (e.g. `"*.md"`) narrow
   the listing.
2. **`skill_ref(skill="<skill>", ref="<base>")`** — convenience for
   reference docs: reads `references/<base>.md` directly without
   needing the absolute path.
3. **`bash.read_file <abs path>`** — for any other bundled file
   (sample data, scripts, templates) once `skill_files` gave you
   the path.

Skill bodies SHOULD point at the references they consider important
in narrative form, but you do NOT have to wait for an explicit
mention. If a workflow looks underspecified or a reference may
exist, run `skill_files` and read what is there.

## Operator policy

Operators can refine this surface via Tier-1 (config) or Tier-2
(Hugr role) rules — for example, denying `bash.shell` while
keeping `bash.run`. The `_system` skill itself is never unloaded.
If a tool you expect is missing, it has been denied by policy;
do not retry, surface the constraint to the user.
