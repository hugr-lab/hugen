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
    requires_approval:
      - bash.run
      - bash.shell
      - bash.write_file
  - provider: skill
    tools:
      - load
      - unload
      - ref
      - files
  - provider: notepad
    tools:
      - append
  # Phase 6.1d — admin-tier tools (policy:* / tool:provider_* /
  # runtime:reload) moved out of the baseline autoload into the
  # `_admin` lazy-load skill. Most sessions never use them; keeping
  # them off the autoload baseline shrinks the tool catalogue every
  # session carries by ~5 entries. Load `_admin` explicitly when an
  # admin action is needed.
metadata:
  hugen:
    requires: []
    sub_agents: []
    memory: {}
    autoload: true
    autoload_for: [root, mission, worker]
    tier_compatibility: [root, mission, worker]
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
   you can construct absolute paths reliably. Use it for
   temporary files, scripts you write, downloaded data, and
   intermediate artifacts. Its lifetime depends on your role in
   the spawn tree:
   - **Standalone session** (no children spawned): `$SESSION_DIR`
     is private to this session. Treat it as ephemeral — the
     runtime reclaims it on its own schedule after close. Save
     anything the user must keep to `$SHARED_DIR`.
   - **Coordinated task** (you were spawned by a dispatcher, or
     workers spawned by the same dispatcher are running
     alongside you): every sibling sees the same `$SESSION_DIR`
     and the directory survives past your own session's close
     so the next worker in the same task can read what you
     wrote. Hand off files between steps by writing to
     `$SESSION_DIR`; the next worker reads them from the same
     path.

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

## meta tools

- `notepad:append` — append to the per-session scratchpad. Use it
  to log intermediate findings the user may ask about later.
- `skill:load` / `skill:unload` — load or release a skill mid-
  session. Inspect the available skills index in your system
  prompt before loading.
- `skill:ref` — read a reference document that ships with a
  loaded skill (`references/<name>.md`).
- `skill:files` — list the on-disk files of a loaded skill with
  relative + absolute paths so other tools (file-readers, script
  runners, query engines) can address them directly. Optional
  `subdir` / `glob` filters narrow the listing.

Admin actions (`policy:save` / `policy:revoke`,
`tool:provider_add` / `tool:provider_remove`, `runtime:reload`)
live in the `_admin` skill — load it via `skill:load("_admin")`
when the user explicitly asks for a personal-policy edit, runtime
provider registration, or live-state refresh. Most sessions never
need them, so they're kept off the baseline to shrink the tool
catalogue every session carries.

## Discovering skill contents

A loaded skill's body (this SKILL.md) is the entry point but rarely
the whole story. Skills ship references, sample data, scripts, and
templates under their own filesystem root. The standard discovery
flow:

1. **`skill:files(name="<skill>")`** — list every file the skill
   ships, with relative + absolute paths, size, and mode. Optional
   `subdir` (e.g. `"references"`) and `glob` (e.g. `"*.md"`) narrow
   the listing.
2. **`skill:ref(skill="<skill>", ref="<base>")`** — convenience for
   reference docs: reads `references/<base>.md` directly without
   needing the absolute path.
3. **`bash.read_file <abs path>`** — for any other bundled file
   (sample data, scripts, templates) once `skill:files` gave you
   the path.

Skill bodies SHOULD point at the references they consider important
in narrative form, but you do NOT have to wait for an explicit
mention. If a workflow looks underspecified or a reference may
exist, run `skill:files` and read what is there.

## Operator policy

Operators can refine this surface via Tier-1 (config) or Tier-2
(Hugr role) rules — for example, denying `bash.shell` while
keeping `bash.run`. The `_system` skill itself is never unloaded.
If a tool you expect is missing, it has been denied by policy;
do not retry, surface the constraint to the user.

## HITL approval (phase 5.1)

`bash.run`, `bash.shell`, and `bash.write_file` carry
`requires_approval: true` in the manifest. The runtime intercepts
calls to those tools and routes them through `session:inquire`
(type=approval); the call returns `{"error": "denied_by_user"}`
unless the operator approves. Treat a denial as a hard stop —
do not retry with a different argument hoping to slip past the
gate; report the denial back to whoever spawned you.
