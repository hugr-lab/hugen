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
      - bash.grep
      - bash.edit_file
    requires_approval:
      - bash.run
      - bash.shell
  - provider: skill
    tools:
      - load
      - unload
      - ref
      - files
      - catalog_list
  - provider: notepad
    tools:
      - append
  # Artifacts (Phase 8) — the durable, user-facing store of the
  # conversation (the user's uploads + everything a session published).
  # Granted on every tier: the capability IS the access (a session sees
  # its own conversation's artifacts, scoped by root). publish a
  # deliverable the user asked for; copy one in to read / process it.
  - provider: artifact
    tools:
      - list
      - copy
      - publish
      - delete
  # Stage 2 (L3) in-turn context checkpoints. Granted on every tier so
  # any spawned worker can always shed context to recover; the triggers
  # only ARM on subagents (root-off tier gate in the compactor), and the
  # tools are advertised in prose only when context is actually filling,
  # so root carries the four tiny-schema tools without noise. Ungated
  # (no approval) — they only reshape what the model sees, never the host.
  - provider: context
    tools:
      - checkpoint
      - hide
      - expand
      - rollback
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
  runtime: hugen
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
   - Write to it with `bash.shell` (the file tools stay inside
     your workspace). For most deliverables prefer
     `artifact:publish` — `$SHARED_DIR` is for when the operator
     designated a specific host exchange folder.

3. **The rest of the host filesystem** — anything else you can
   reach by absolute path. In container deployments the kernel
   confines you to the bind-mounted paths. In local / dev
   deployments you are running under the user's own filesystem
   permissions; behave accordingly.

### Behaviour rules

- **The file tools write only inside your workspace.** Every tool
  call starts in your scratch dir, and `bash.write_file` /
  `bash.edit_file` / `bash.sed` are confined to it — a host path or
  a peer session's dir is rejected. That confinement is why they
  need no approval: a workspace write can't clobber anything that
  matters.
- `cd` inside a single `bash.shell` invocation works for that
  call only. The next tool call starts back at scratch. Pass
  `cwd: "subdir"` to start a tool call in a sub-directory.
- For a file that must outlive the session, publish it
  (`artifact:publish`) or, for a `$SHARED_DIR` / user-named host
  path, write it via `bash.shell`. Everything else stays in scratch.
- All shell binaries on the host PATH are available — `du`,
  `find`, `grep`, `sed`, `awk`, `python`, `git`, etc. Use them
  freely; bash-mcp does not restrict them.
- To edit a few values in an existing file, prefer not to read it
  whole: `bash.grep(path, pattern)` to locate the anchor you
  already know, then `bash.edit_file(path, old, new)` on the
  minimal changed text. Reading a large file just to edit it
  wastes context. (`bash.read_file` truncates large files anyway;
  page with `start`, or load the file in python for data.)
- When the user asks "what files do you see", check both your
  scratch dir and `$SHARED_DIR` before reporting "empty".

## artifacts — the user's deliverables

Artifacts are the durable, user-facing store of THIS conversation:
the files the **user uploaded** plus the files a session
**published**. They are the user's one place to pick up results —
distinct from the ephemeral scratch dir above.

- **To use an artifact** (an upload, or a prior result): copy it into
  your workspace and read it — `artifact:copy(id)` gives a normal
  local file. `artifact:list` shows what exists (id · name · type ·
  size).
- **To deliver a result**: publish a workspace file —
  `artifact:publish(path)`. Use this for any deliverable the user
  asked for (a report, a cleaned dataset, a generated document) when
  they did NOT name a specific host path.
- **Disambiguation.** "Save the report" / "give me a file" with NO
  path → **publish an artifact**. A CONCRETE host path the user named
  → write it there with `bash.shell` (the file tools are
  workspace-only). Scratch stays in the workspace; deliverables
  become artifacts.
- **Reference a published artifact in your reply to the user as
  `artifacts://<name>`** (e.g. "saved to `artifacts://road-report.md`")
  so their client renders an open / download element.
- Publishing is non-overwriting by default; to replace one, read
  `artifact:list` first, then publish with `overwrite:true`.

## meta tools

- `notepad:append` — append to the per-session scratchpad. Use it
  to log intermediate findings the user may ask about later.
- `skill:load` / `skill:unload` — load or release a skill mid-
  session. Inspect the available skills index in your system
  prompt before loading.
- `skill:catalog_list` — search the FULL skill catalogue by
  free-text `keyword` (relevance-ranked), or list everything when
  called without one. The `## Available skills` block is a capped
  shortlist; use this tool when nothing there fits or the user
  asks what skills exist. It lists loadable SKILLS only — to find a
  runnable built task, use `task:search` (where granted).
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

## HITL approval

`bash.run` and `bash.shell` carry `requires_approval: true` in the
manifest — they can reach the whole host. The file tools
(`bash.write_file`, `bash.edit_file`, `bash.sed`) are NOT gated:
they are confined to your session workspace, so a write is
inherently safe and runs without a prompt (a host path is rejected
— deliver host files with `artifact:publish`, or use the gated
`bash.shell`). The runtime intercepts calls to the gated tools and
routes them through `session:inquire` (type=approval); the call
returns `{"error": "denied_by_user"}` unless the operator approves.
Treat a denial as a hard stop —
do not retry with a different argument hoping to slip past the
gate; report the denial back to whoever spawned you.
