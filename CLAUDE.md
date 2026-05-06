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

We are mid-execution of **design 001 — Hugr Agent Runtime**
(`design/001-agent-runtime/design.md`). Phase plan:

| Phase | Status |
|-------|--------|
| 1. Native core + ModelRouter + console UI | shipped |
| 2. HTTP/SSE + webui + ADK eviction | shipped |
| 3. Action layer (skills + tools + 3-tier permissions + bash/hugr-mcp) | shipped |
| 3.5. Analyst toolkit (duckdb-mcp + python-mcp + analyst skills) | shipped |
| **4. Sub-agents + plan + whiteboard + event-driven session loop** | **spec drafted v2** (`phase-4-spec.md`, ready for `/speckit.specify`); architecture decisions in `phase-4-architecture.md` |
| 4.1a. Extract `pkg/runtime` + dissolve `SystemProvider` (tools onto domain `ToolProvider`s; absorbs ex-4.3) | spec drafted v1 (`phase-4.1a-spec.md`); follows 4, gates 4.1b |
| 4.1b. Observational scenario harness (port `../agent/tests/scenarios/` pattern; live LLM + real Hugr; ~8 scenarios v1) | spec drafted v1 (`phase-4.1b-spec.md`); follows 4.1a, gates 4.2 |
| 4.2. Analyst mega-skill + role sub-agents + `skill_builder` + community-skill enablement (tri-state `allowed-tools`, `system:tool_catalog`) | spec drafted v0 (`phase-4.2-spec.md`); follows 4.1b |
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

## Active focus — phase 4.1a

Phase 4 shipped to `main` as `ba003c0` (PR #6). Active focus
moves to **phase 4.1a — extract `pkg/runtime` + dissolve
`SystemProvider`** before the observational harness (4.1b)
and the analyst mega-skill (4.2) can land on a clean wiring
surface.

**Document set for phase 4.1a work** (in order of authority for
an implementer):

1. **`design/001-agent-runtime/phase-4.1a-spec.md`** — the
   contract. §1 Why bundle two refactors, §2 Goal, §3 Boundary
   contract, §4 Two config types (BootstrapConfig vs
   runtime.Config — env-pure), §5 Nine phases of `Build`, §6
   System-tools refactor (absorbed from ex-4.3), §7 File
   layout, §8 Migration / risk, §9 Implementation order (19
   commits, single PR), §10 What 4.1a does NOT include.
2. **`design/001-agent-runtime/phase-4.3-spec.md`** —
   superseded by 4.1a §6, kept for historical context. Tool
   ownership map and rename rationale carry over verbatim.
3. **`design/001-agent-runtime/design.md §19`** —
   architectural foundations to honour. 4.1a is a refactor —
   no foundation lands here; existing decisions stay.

**Key locked decisions for 4.1a**:

- `pkg/runtime.Build(ctx, runtime.Config) (*Core, error)` is
  the single boot-path entry point. Both `cmd/hugen` and the
  4.1b harness call it.
- `runtime.Config` is **env-pure** — no `os.Getenv` inside
  Build. Caller projects from `BootstrapConfig` (cmd/hugen) or
  scenario-bootstrap (harness).
- Build runs **9 named phases** sequentially; each phase reads
  fields populated by prior ones. No cross-cutting helpers.
- `pkg/tool/system.go` is **deleted**. Every system tool lives
  next to its state owner via `tool.ToolProvider`:
  `*skill.SkillManager`, `*tool.ToolManager` (self-hosting),
  `*tool.Policies`, `*session.Manager` (`notepad_append`),
  `cmd/hugen.reloadProvider` (`runtime:reload`).
- **Strict rename** for tool names (`system:foo` →
  `<owner>:foo`). Bundled skill manifests under
  `assets/skills/` update in the same PR; manifest validator
  emits a helpful migration error for stale `system:*`
  references in `allowed-tools`.
- 4.1a is a **pure refactor** — no behaviour change visible to
  the model, no new flags / env vars / tools.

Phase 4.1a is **one PR** on branch `007-runtime-extract`.
Internal commit order in spec §9 (19 commits) is prescriptive
for the author / reviewer; not enforced as separate PRs.

Phase 4.1b (harness; `phase-4.1b-spec.md`) and phase 4.2
(`phase-4.2-spec.md`) follow sequentially; both consume the
clean `pkg/runtime` surface 4.1a establishes.

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
design/001-agent-runtime/         # design + per-phase specs (gitignored after promotion)
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
   `design/001-agent-runtime/*.md` (or a new ADR-style note alongside
   it) with the *why*, the alternative considered, and what it
   supersedes. Implementation memos go into the per-phase spec's
   "Implementation update" footer (the pattern phase 3.5 used after
   `2026-05-01`).
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
