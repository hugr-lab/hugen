# Discovery flow — finding unloaded local skills

Local skills do NOT autoload. The discovery channel is
`skill:tools_catalog` → `available_in_skills`. This document walks
through the mechanics with worked examples.

## How `available_in_skills` works

`skill:tools_catalog` returns every registered tool with two
discovery-relevant fields per entry:

```json
{
  "name": "hugr-main:discovery-list_collections",
  "granted_to_session": false,
  "available_in_skills": ["material-movement-report", "hugr-data"]
}
```

- **`granted_to_session`** — true if the calling session's loaded
  skills' `allowed-tools` admit this tool RIGHT NOW.
- **`available_in_skills`** — every skill in the store (loaded or
  not, system / local / community) whose `allowed-tools` admits
  this tool. **This is the load-bearing field for discovery** —
  it surfaces unloaded skills you might want to load.

The list includes:

- Skills with explicit grants matching this tool (e.g. `hugr-data`
  grants `hugr-main:*`).
- Skills with absent `allowed-tools` (the agentskills.io "do not
  restrict" form). These appear for EVERY tool — the model
  evaluates them by `description`, not by tool match.

## When to use the discovery flow

Run discovery on the FIRST turn of any non-trivial user request.
Concretely:

- Analytical / reporting requests.
- Data-fetch / summarisation patterns.
- Anything multi-step that the user might have done before.

Skip discovery for:

- Pure conversational answers ("what time is it", "explain X").
- Simple single-tool requests handled by an autoloaded skill.

## Worked example — material movement report

Session N: user asked for movement of material AB-1234, agent ran
queries + rendered PDF, user said "сохрани". Saved as
`material-movement-report` with `description: "PDF-отчёт о
движении материала в модуле логистики"`.

Session N+1 (fresh): user asks "хочу movement по материалу
XYZ-9999, как в прошлый раз".

Your flow:

1. Recognise this as analytical/data-fetch shape → run discovery.
2. `skill:tools_catalog(pattern: "movement")` — distinctive
   keyword from the request.
3. Inspect result. Likely a `hugr-main:*` tool entry has
   `available_in_skills: ["material-movement-report", "hugr-data"]`.
4. Inspect each candidate. `material-movement-report` description
   matches the request shape directly.
5. `skill:load material-movement-report`.
6. The "Loaded skill bundles" section appears in your prompt with
   `directory:` and the bundled `scripts/`, `references/`,
   `assets/` listing.
7. Read the loaded skill body. Follow its procedure with
   `material_code=XYZ-9999`.

## Discovery vs autoload

| Mechanism | Where used | Triggered |
|-----------|-----------|-----------|
| **Autoload** | A tight set of tier-base skills (`_root`, `_mission`, `_worker`, `_planner`, `_whiteboard`) — kept minimal so steady-state context cost stays low | Every session at boot, per tier |
| **Discovery + skill:load** | Local user skills (created via `skill:save`), on-demand community skills, AND `_skill_builder` itself (load it when the save / explicit-discovery trigger fires — full protocol in its body + `references/save-protocol.md`) | Model decides per turn |

There is no per-session "autoload my local skills" knob — by
design. Local stores grow over sessions and an autoload-everything
policy would bloat the system prompt unboundedly.

## Pattern matching tips

Choose `pattern` keywords that:

- Are present in the request (so retrieval is grounded).
- Are likely to appear in skill names or descriptions (skills are
  named after their domain, not their implementation).

Bad: `pattern: "tool"` — matches everything.
Bad: `pattern: "discovery-list"` — too narrow, may miss
broader skills.
Good: `pattern: "movement"` — domain-specific; matches both
the tool and skill names referencing logistics.

If your first pattern returns nothing, try a broader keyword OR
call `skill:tools_catalog` with no filters and scan the
`available_in_skills` field across all tools.

## Don't reload an already-loaded skill

`skill:tools_catalog` shows ALL skills in the store. Some are
already loaded into your session (system skills like `_root`).
Check the "Loaded skill bundles" block in your prompt before
calling `skill:load`. Loading an already-loaded skill is a
no-op, but it's noise.

## When to NOT load a discovered skill

- Description doesn't match the user's request shape — even if
  the name looks plausible.
- Skill grants tools you don't trust for the current session
  (e.g. a community skill granting destructive operations
  beyond the user's request).
- Multiple candidates and you can't decide — ask the user
  rather than guessing.

It's better to compose from scratch than load the wrong skill
and produce wrong output following its procedure.
