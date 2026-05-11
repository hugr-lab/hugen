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
- `design/002-runtime-canonical/phase-4.2.3-cross-mission-notepad.md` — active phase (with 2026-05-11 implementation-notes footer).

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
| 4.2. Skill creation infrastructure (tri-state `AllowedTools` + `skill:save` + skill Advertiser + `_skill_builder`). | shipped (`1fc4e59`, 2026-05-10) |
| 4.2.2. Three-tier mandatory delegation (root → mission → worker); `session:spawn_mission` singular + `session:spawn_wave` atomic; per-tier constitutions (`agent.md` + `tier-{root,mission,worker}.md`); `analyst` + `_general` mission dispatchers; Gemma 26B 20/20 acceptance gate. | shipped (`53ab909`, 2026-05-11) |
| 4.2.3. **Session-scoped working notepad** — climb-to-root in extension; 4 new columns on `session_notes` (`category`, `author_role`, `mission`, `embedding`); `notepad:append/read/search/show` (no clear / no archive — append-only constitution); configurable read window (default 48h); semantic search via Hugr `@embeddings` + `semantic:` top-level arg + `summary:` mutation; auto-snapshot at mission spawn. Bounded to single root session — cross-conversation distillation is phase 7. | spec at `design/002-runtime-canonical/phase-4.2.3-cross-mission-notepad.md` with 2026-05-11 implementation-notes footer; ~530 LOC; depends on 4.2.2 (shipped). |
| ~~4.3~~ | **cancelled 2026-05-08** — historical scope was Manager-as-ToolProvider generalisation; superseded by 4.1a (`SystemProvider` already dissolved) and the per-domain ToolProvider pattern that landed with it. |
| 5. Compactor + HITL: approvals + clarifications (compactor first within the phase; replaces the phase-3 `defaultHistoryWindow=50` stop-gap) | open |
| 6. Cron + scheduler | open |
| 7. Memory pipeline + LLM Wiki — cross-conversation distilled knowledge (validated facts, advisor sub-agent). Reads phase-4.2.3 notepad as input; session-scoped working memory is owned by 4.2.3 (phase 7 does not duplicate that taxonomy). | open |
| 8. Artifacts | open |
| 9. A2A adapter | open (defer until needed) |
| 10. Multi-party Workspaces (human + agent) | open — external interaction surface; lands after A2A so participant model is shaken out |
| Backlog. PeerGroup mesh / pipeline | deferred — whiteboard (phase 4) covers broadcast; mesh / pipeline re-introduce when a real workload demands it |

Goal: finish design-001 cleanly, then move to **hub integration**
(container packaging, deployment story, hub-spawned mode). Hub work is
explicitly deferred until design-001 is complete — `phase-3.5-spec.md
§Out of scope` and `design.md §16.8`.

## Active focus — phase 4.2.3

Phases 4.2 (PR #11, `1fc4e59`, 2026-05-10) and 4.2.2 (PR #12,
`53ab909`, 2026-05-11) shipped to `main`. Three-tier mandatory
delegation (root → mission → worker) is live; `analyst` +
`_general` dispatchers green on the Gemma 26B 20/20 acceptance
gate.

Next phase is **4.2.3 — session-scoped working notepad**. Spec
at `design/002-runtime-canonical/phase-4.2.3-cross-mission-notepad.md`
with **2026-05-11 implementation-notes footer** (read the footer
first — it overrides several decisions in the body of the spec).
Closes the cross-mission-memory gap **within a single root
session**: knowledge mission A discovers is visible to mission B
without re-discovery. Strictly session-bounded; cross-conversation
distillation belongs to phase 7.

~530 LOC code + content, ~1.5 weeks:

1. **4 new columns on `session_notes`** (per footer §2):
   `category` (model-supplied open tag), `author_role` (runtime,
   from tier), `mission` (model-supplied short context phrase),
   `embedding` (Hugr server-side via `summary:` mutation arg
   under `@embeddings` directive). `author_skill`,
   `mission_goal-as-snapshot`, `last_accessed_at`, `archived_at`,
   `deleted_at` — all dropped per append-only constitution.
2. **Climb-to-root write path** — `notepad:append` walks
   `RootAncestor()` in the extension and stores
   `session_id = root.id`. `session_notes_chain` recursive view
   (`pkg/store/local/schema.tmpl.graphql:559-598`, already in
   place) returns the union — every session in the tree sees
   the full conversation's notepad.
3. **Configurable read window** (`config.notepad.window`,
   default **48h**) gates `read` / `search` / Block B snapshot
   via `filter: { created_at: { gte: $cutoff } }`. No archival
   sweep, no `notepad:clear` tool. Old notes physically remain
   (append-only) but fall out of model visibility.
4. **Pure semantic search** — `semantic: { query, limit }`
   top-level GraphQL arg on the chain view (same pattern
   `session_events` uses in `pkg/session/store/store.go:428-451`).
   No Go-side hybrid re-rank; window is narrow enough that pure
   similarity ordering suffices.
5. **Auto-snapshot at mission spawn** — third action in
   `applyMissionStartWrites` after plan + whiteboard renders a
   grouped-by-tag recent-notes block into the mission's first
   system prompt (≤2KB, ≤8 tags, ≤80 char snippets).
6. **`sessions.mission` column** (existing at
   `schema.tmpl.graphql:316` / `SessionRow.Mission`, currently
   unwritten): populated at `spawn_mission` from `SpawnSpec.Task`
   for Block B's current-mission-context header and for
   observability via `hub.db.agent.sessions`.

**Surface**: `notepad:append` (canonical write, name kept from
phase-3.5 stub) + new `notepad:read/search/show`. No `clear`.
Breaking change vs the existing append-only stub is ok per
pre-v1 (`feedback_pre_v1_breaking_changes` memory).

**Closes / supersedes** in spec body: open questions #1, #2, #3
(text-similarity API, hybrid weights, archival threshold) all
resolved by the footer. `_distance_to_query` projection,
oversample-and-re-rank, lifecycle timestamps — all dropped.

Milestones (per footer §11):
- α (~150 LOC) — schema + Hugr queries.
- β (~220 LOC) — extension surface (Append / Read / Search /
  Show; climb-to-root; window filter).
- γ (~100 LOC + content) — auto-snapshot + skill manifest
  `mission.on_start.notepad.tags`.
- δ — `cross_mission_notepad` scenario passes ≥ 7/10 on Gemma
  26B without regression on 4.2.2 scenarios.

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
