---
name: _admin
description: >
  Operator-tier admin surface — personal-policy editing (`policy:*`),
  runtime tool-provider registration (`tool:provider_*`), and live
  runtime reload (`runtime:reload`). Lazy-load via
  `skill:load("_admin")` when the user explicitly asks to edit a
  policy, add an MCP provider, or refresh state; otherwise leave
  unloaded — the baseline `_system` skill no longer carries these
  tools because most chat sessions never use them and catalogue
  noise scales linearly with grants.
license: Apache-2.0
allowed-tools:
  - provider: policy
    tools:
      - save
      - revoke
  - provider: tool
    tools:
      - provider_add
      - provider_remove
  - provider: runtime
    tools:
      - reload
metadata:
  hugen:
    requires_skills: []
    autoload: false
    autoload_for: []
    # Restricted to root tier: admin actions originate from the
    # operator's chat session, never from a mission's wave-worker or
    # an ad-hoc recipe child. The skill:load gate (allowed_skills
    # whitelist) blocks task-spawned children from loading this
    # surface even if the LLM tries.
    tier_compatibility: [root]
compatibility:
  model: any
  runtime: hugen-phase-6
---

# `_admin` skill

Operator-tier admin tools, kept off the baseline autoload so most
sessions don't carry their tool descriptions in the catalogue.
Load only when the user explicitly asks for an admin action.

## When to load

- User asks to "always allow / always deny" a tool → `policy:save`
- User asks to revoke a personal policy entry → `policy:revoke`
- User asks to register a new MCP provider at runtime →
  `tool:provider_add`
- User asks to drop a running MCP provider → `tool:provider_remove`
- User asks to refresh permissions / skills / MCP state without a
  restart → `runtime:reload`

## Tool surface

- **`policy:save(tool_name, decision, scope, note)`** — persist a
  Tier-3 personal "always allow" / "always deny" rule for the
  caller. `tool_name` accepts a `<provider>:<field>` exact match or
  a `*` suffix glob. `decision` ∈ `allow|deny|ask`. Tier 3 NEVER
  overrides the operator floor or the user's role rules — a higher
  tier can still block.
- **`policy:revoke(tool_name, scope)`** — remove a previously-saved
  Tier-3 entry.
- **`tool:provider_add(name, config)`** / **`provider_remove(name)`**
  — register or drop a tool provider at runtime. Operator-only;
  may be denied by policy. Use `runtime:reload(target=mcp)` to
  restart an already-registered MCP provider in place.
- **`runtime:reload(target)`** — `target` ∈ `permissions`
  (re-fetch role rules) | `skills` (rescan skill stores) | `mcp`
  (re-spawn per-agent MCP providers) | `all`. Use only when the
  user explicitly asks to refresh.

## Notes

- These tools are unloaded by default — the recipe-child path
  (Phase 6.1d sealed session) cannot reach them even via
  `skill:load` unless the recipe manifest explicitly lists
  `_admin` in `allowed_skills` (which no recipe should).
- `_admin` itself is not autoloaded for any tier; loading is an
  explicit user-initiated step.
