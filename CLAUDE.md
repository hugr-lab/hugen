# hugen — Claude Operating Notes

## What hugen is

Native AI-agent runtime for the Hugr Data Mesh platform. One Go binary
that hosts the agent loop, tool dispatch, prompt assembly, skill
loading, and per-session lifecycle. Two run modes:

- **local** — embeds Hugr in-process via DuckDB; runs as a
  single-tenant analyst on the user's host.
- **hub** — connects to a remote Hugr deployment for catalog,
  memory, and data queries.

The agent treats Hugr as the LLM backend, the data exploration layer,
and the persistent memory store — all through GraphQL + MCP. We build
inside a Hugr ecosystem; constraints from that environment are
authoritative (see `.specify/memory/constitution.md` and
`assets/constitution/agent.md`).

## Where we are

We are mid-execution of **design 001 — Hugr Agent Runtime**.
The canonical, up-to-date design lives in
`design/002-runtime-canonical/` (rewrite as of 2026-05-10);
`design/001-agent-runtime/` is preserved as the historical
record of *why* each phase ended up the way it did.

**Read first** when touching session / extension / tool / skill
internals:

- `design/002-runtime-canonical/architecture.md` — state-of-the-tree
  map; §11 is the extension recipe book.
- `design/002-runtime-canonical/design.md` — vision + phase plan.
- `design/002-runtime-canonical/phase-4.2-spec.md` — active phase.

When 002 and a 001 phase doc disagree, 002 wins.

Phase plan:

| Phase | Status |
|-------|--------|
| 1. Native core + ModelRouter + console UI | shipped |
| 2. HTTP/SSE + webui + ADK eviction | shipped |
| 3. Action layer (skills + tools + 3-tier permissions + bash/hugr-mcp) | shipped |
| 3.5. Analyst toolkit (duckdb-mcp + python-mcp + analyst skills) | shipped |
| 4. Sub-agents + plan + whiteboard + event-driven session loop | shipped |
| 4.1a. Extract `pkg/runtime` + dissolve `SystemProvider` (tools onto domain `ToolProvider`s; absorbs ex-4.3) | shipped (`33a0bc3`) |
| 4.1b. Observational scenario harness (port `../agent/tests/scenarios/` pattern; live LLM + real Hugr; ~8 scenarios v1) | shipped (`109f6b9`); 7 of 9 scenarios validated on gemini-pro + gemma4-26b, claude-sonnet canary green; `full_analyst_workflow` deferred → 4.2 |
| 4.1c. Subagent-as-adapter: parent observes child's outbox; eliminates child→parent.Submit cross-session shortcut. Surfaced by 4.1b harness (every sub-agent hung mid-flight). | shipped (`109f6b9`) — pump + retry + per-skill intent + plan envelope migration all in same merge |
| 4.2. **Skill creation infrastructure** — closes the save → discover → reuse loop. Tri-state `AllowedTools` (nil≠empty) + union resolution (also wires existing `tools_catalog.available_in_skills` → discovery channel for unloaded local skills); `skill:save` (structured bundle, manifest validation, autoload-rejection, collision-handling, path-safety); skill `Advertiser` exports `directory` + bundled-files listing; `_skill_builder` system skill (autoload root) holds discovery + save protocols with mandatory validation. `skill:tools_catalog` already exists, no code change. | spec v3 (`design/002-runtime-canonical/phase-4.2-spec.md`); ~640 LOC code + ~440 LOC tests + content, ~1 week. Routing-as-structural-mechanism cancelled 2026-05-10 (was over-engineered); analyst content moved out to 4.2.2. |
| 4.2.2. **Analyst mega-skill** — bundled `_analyst` skill with 4 roles in `sub_agents:` (`data-explorer`, `sql-analyst`, `python-postprocessor`, `report-builder`); re-enable `full_analyst_workflow` harness scenario; tune preamble until passes consistently across Claude / Gemini / Gemma. Authored inline via 4.2's `skill:save` first, then promoted. | spec v1 (`design/002-runtime-canonical/phase-4.2.2-spec.md`); ~700 LOC content, 1.5-2 weeks of harness iteration. Pure content + scenario tuning; no `pkg/*` changes. Depends on 4.2. |
| ~~4.3~~ | **cancelled 2026-05-08** — historical scope was Manager-as-ToolProvider generalisation; superseded by 4.1a (`SystemProvider` already dissolved) and the per-domain ToolProvider pattern that landed with it. |
| 5. Compactor + HITL: approvals + clarifications (compactor first within the phase; replaces the phase-3 `defaultHistoryWindow=50` stop-gap) | open |
| 6. Cron + scheduler | open |
| 7. Memory pipeline + LLM Wiki (short + long-term) | open |
| 8. Artifacts | open |
| 9. A2A adapter | open (defer until needed) |
| 10. Multi-party Workspaces (human + agent) | open — external interaction surface; lands after A2A so participant model is shaken out |
| Backlog. PeerGroup mesh / pipeline | deferred — whiteboard (phase 4) covers broadcast; mesh / pipeline re-introduce when a real workload demands it |

Goal: finish design-001 cleanly, then move to **hub integration**
(container packaging, deployment story, hub-spawned mode). Hub work is
explicitly deferred until design-001 is complete — `phase-3.5-spec.md
§Out of scope` and `design.md §16.8`.

## Active focus — phase 4.2

Phases 4 / 4.1a / 4.1b-pre / 4.1b / 4.1c all shipped to `main`
(latest merge `109f6b9`). The scenario harness lives in
`tests/scenarios/`; subagent pump + retry + per-skill intent +
plan→ExtensionFrame migration all landed with phase 4.1c. 4.3 is
**cancelled** (its scope was Manager-as-ToolProvider, which 4.1a
already absorbed).

Next phase is **4.2 — skill creation infrastructure**. Spec
v3 at `design/002-runtime-canonical/phase-4.2-spec.md`. Closes
the **save → discover → reuse** loop with minimal new code by
leaning on what already exists. ~640 LOC code + ~440 LOC tests
+ ~200 lines content, ~1 week PR:

1. **Tri-state `AllowedTools` (nil≠empty)** — `nil` (absent → inherit
   via union), non-nil empty (explicit empty), populated.
   Unblocks community-skill onboarding AND propagates into
   the existing `tools_catalog`'s `granted_to_session` /
   `available_in_skills` projections — wiring the discovery
   channel correctly.
2. **`skill:save`** tool — structured bundle (`skill_md` +
   `references` + `scripts` + `assets`), manifest
   validation, rejects `autoload: true` (reserved for
   system / admin), `overwrite: false` default with explicit
   collision error, path-safety on relative keys. Auto-loads
   in current session.
3. **Skill `Advertiser`** — exports loaded skill's
   `directory` + bundled-files listing into system prompt;
   model invokes bundled scripts/templates via existing
   `bash:run` / `python:run_script` with `${SKILL_DIR}/...`.
4. **`_skill_builder` system skill** (autoload root) — two
   protocols. **Discovery**: before composing a procedure
   from scratch on a non-trivial request, call
   `skill:tools_catalog(pattern=...)`, scan
   `available_in_skills` for unloaded local skills,
   `skill:load` if a fit. **Save**: on user-initiated save —
   clarify, generalise, ground, mandatory post-save
   validation loop (test with synthetic params → if fail
   unload+fix+resave with `overwrite=true`), naming-collision
   handling.

**`skill:tools_catalog` is NOT new** — it already exists at
`pkg/extension/skill/tools_catalog.go` with `granted_to_session`
+ `available_in_skills`. Phase 4.2 only verifies tri-state
union is correctly reflected (mainly the `available_in_skills`
indexer needs to handle absent-allow skills per spec §3.3.2).

**Cancelled mid-discussion** (was in earlier drafts):
`task_classify` tool, `pkg/extension/router/`, ToolFilter
routing gate, 4-class taxonomy, `skill_builder` mega-skill
walkthrough, strict validation hardening, brand-new
`skill:tools_catalog` (already exists). Rationale: lean on
what's there; routing stays as constitution-level guidance
per skill, not a structural mechanism.

After 4.2 → **4.2.2 (analyst mega-skill)**: spec at
`design/002-runtime-canonical/phase-4.2.2-spec.md`. One
bundled `_analyst` skill with 4 roles in `sub_agents:`,
authored inline via 4.2's `skill:save` first (eat our own
dogfood), then promoted to `assets/skills/_analyst/` once
harness scenarios pass consistently across Claude / Gemini /
Gemma. Pure content + scenario tuning; no `pkg/*` changes.

## Project structure

```text
cmd/
├── hugen/                # main binary — runtime bootstrap, console + webui adapters
└── hugen-skill-validate/ # CLI: validate a SKILL.md manifest
mcp/
├── bash-mcp/             # in-tree shell + filesystem MCP (per_session)
├── hugr-query/           # in-tree Hugr GraphQL → file output (per_agent)
└── python-mcp/           # in-tree Python execution + lazy per-session venv (per_agent)
pkg/
├── adapter/{console,http,webui}  # transport adapters
├── auth/{perm,sources,template}  # 3-tier permission stack + auth.Service loopback
├── config/                       # YAML schema + StaticService
├── identity/{local,hub}          # who-am-I providers
├── model/, models/, protocol/    # LLM routing + Frame protocol
├── session/                      # Session, Manager, Resources, Workspace, RuntimeStore
├── skill/                        # Manifest parser + SkillManager + stores (system/local/community/inline/hub)
├── store/local/                  # embedded DuckDB persistence
└── tool/                         # ToolManager + MCPProvider + SystemProvider + Policies
assets/
├── constitution/agent.md         # universal agent constitution (rendered into prompt)
├── python/requirements.txt       # bundled analyst venv package list
└── skills/                       # bundled skills: _system, hugr-data, duckdb-data, duckdb-docs, python-runner
vendor/
└── mcp-server-motherduck/        # vendored MotherDuck DuckDB MCP (git submodule, MIT)
design/001-agent-runtime/         # historical: per-phase specs, original design, original architecture (gitignored)
design/002-runtime-canonical/     # canonical: design.md, architecture.md, active phase-N-spec.md (gitignored)
specs/<NNN-feature-name>/         # per-feature speckit artefacts (gitignored)
.specify/memory/constitution.md   # Go code constitution (load-bearing rules)
```

## How I should behave (Claude operating mode)

I am logical, consistent, and grounded in the actual codebase. I do
not invent APIs, package layouts, or architectural decisions; I read
code first, propose second, edit third. When the user asks something
I do not know, I check (`grep`, `Read`, `gh`, build/test) before
answering. I am allowed to speculate — but every speculation must end
with "let me verify" or "this is a guess, please correct me", and I
evaluate every proposal against what already exists in the tree.

I am brief. End-of-turn summaries are one or two sentences. Updates
mid-task are one short line. I do not narrate internal deliberation.

Russian / English: I match the user's language.

## Flexibility with deliberation

We are **flexible on goals and architecture**: design-001 phases can
be re-ordered, scopes can be moved, providers can be added or
collapsed, package boundaries can shift. But every such change is a
deliberate decision, not a drift:

1. **Surface the question.** When I or the user notice the current
   plan no longer fits, I name it explicitly.
2. **Discuss before editing.** Architecture / goal pivots are
   discussed before code lands — even when the patch is small.
3. **Record and justify.** The decision goes into the relevant
   `design/002-runtime-canonical/*.md` (or a new ADR-style note
   alongside it) with the *why*, the alternative considered, and
   what it supersedes. Implementation memos for active phases go
   into the per-phase spec's "Implementation update" footer (the
   pattern phase 3.5 used after `2026-05-01`).
4. **No silent rewrites.** Refactoring code that touches a documented
   contract requires updating the contract in the same PR or in an
   immediately-visible follow-up.

Code-level constitution (Go conventions, package layering, ADK
quarantine, append-only persistence) lives in
`.specify/memory/constitution.md` and is **non-negotiable** at PR
review. Goals and architecture are negotiable but only via the flow
above.

## Build / test cheatsheet

```sh
make build                       # bin/{hugen,bash-mcp,hugr-query,python-mcp}
make test                        # go test -race -tags=duckdb_arrow ./...
make submodule-check             # gate against vendored MCP drift
make python-mcp-template         # one-time analyst venv build (after make build)
go vet -tags=duckdb_arrow ./...  # static checks
```

Constitution gates (every PR): clean build with `-tags=duckdb_arrow`,
green tests, `go vet` clean, no new ADK imports below `pkg/models`,
new public function/method comes with happy-path + edge-case test.

## Speckit flow

Per-feature work goes through `/speckit.specify` → `/speckit.plan` →
`/speckit.tasks` under `specs/<NNN-feature-name>/`. The active feature
slot is `.specify/feature.json`. Outputs are gitignored by design
(per-developer) and only get reflected in `design/` when promoted.

<!-- MANUAL ADDITIONS START -->
<!-- MANUAL ADDITIONS END -->

## Active Technologies
- Go 1.23.x (project uses generics, `slices`, `maps`, (005-phase-4-agent-runtime)
- DuckDB local store via `pkg/store/local`; append-only on (005-phase-4-agent-runtime)

## Recent Changes
- 005-phase-4-agent-runtime: Added Go 1.23.x (project uses generics, `slices`, `maps`,
