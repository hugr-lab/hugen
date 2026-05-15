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
    autoload: false
    autoload_for: []
    tier_compatibility: [root, mission, worker]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _skill_builder

Two protocols this skill teaches. **The skill is NOT autoloaded** —
the constitution mentions it by name in the discovery / save
paragraphs; load it via `skill:load(name: "_skill_builder")` when
either trigger fires, then follow the procedure below.

1. **Discovery** — before composing a procedure from scratch for any
   non-trivial task, check whether a saved local skill already covers
   it. Local skills do **not** autoload; you find them through
   `skill:tools_catalog`.
2. **Save** — when the user explicitly requests crystallising the
   current session's work into a reusable skill, follow the save +
   validate + collision-handling protocol. Full detail in
   `references/save-protocol.md` — call `skill:ref(skill:
   "_skill_builder", ref: "save-protocol")` the first time you need
   it in a session, then keep the body in working context.

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
they already crystallised. See `references/discovery-flow.md` for
worked examples.

## Save — trigger and entry point

Do **not** propose this yourself — the user owns this decision. The
save action is triggered by phrases like:

- "сохрани это как скилл / save this as a skill"
- "давай сделаем скилл что бы ... / let's make a skill that ..."
- "запомни этот процесс / remember this procedure"

When you see one of these triggers, read
`references/save-protocol.md` via `skill:ref` and follow the steps
end-to-end (clarify name + description + parameters → compose bundle
with generalised placeholders → `skill:save` → mandatory post-save
validation with synthetic params → report ready to user).

Also see `references/manifest-cheatsheet.md`,
`references/bundle-layout.md`, `references/validation-protocol.md`
for cross-references during the save flow.

## What this skill does NOT do

- Does NOT publish to a remote / shared catalogue. `skill:save` is
  local-store only. Hub-mode publish lands with hub integration.
- Does NOT propose saves. The user initiates; you follow.
- Does NOT replace the future memory layer. Memory is for implicit
  pattern reuse; this skill is for explicit user-curated procedures.
