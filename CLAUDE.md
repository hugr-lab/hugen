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
- `design/002-runtime-canonical/backlog.md` — consolidated free
  agenda (cross-phase items that don't yet warrant a phase).
- Per-phase specs: `phase-4.2-*.md`, `phase-4.2.2-*.md`,
  `phase-4.2.3-*.md`, `phase-5.1-hitl-followups-async.md`,
  `phase-5.1b-spec.md`.

When 002 and a 001 phase doc disagree, 002 wins.

Phase plan:

| Phase | Status |
| ----- | ------ |
| 1. Native core + ModelRouter + console UI | shipped |
| 2. HTTP/SSE + webui + ADK eviction | shipped |
| 3. Action layer (skills + tools + 3-tier permissions + bash/hugr-mcp) | shipped |
| 3.5. Analyst toolkit (duckdb-mcp + python-mcp + analyst skills) | shipped |
| 4. Sub-agents + plan + whiteboard + event-driven session loop | shipped |
| 4.1a. Extract `pkg/runtime` + dissolve `SystemProvider` | shipped (`33a0bc3`) |
| 4.1b. Observational scenario harness | shipped (`109f6b9`) |
| 4.1c. Subagent-as-adapter pump | shipped (`109f6b9`) |
| 4.2. Skill creation infrastructure (tri-state `AllowedTools` + `skill:save` + `_skill_builder`) | shipped (`1fc4e59`, 2026-05-10) |
| 4.2.2. Three-tier mandatory delegation (root → mission → worker) | shipped (`53ab909`, 2026-05-11) |
| 4.2.3. Session-scoped working notepad + sync close-turn + `overview` role | shipped (`066c7b1`, 2026-05-12) |
| ~~4.3~~ | **cancelled 2026-05-08** — superseded by 4.1a + per-domain ToolProvider pattern. |
| 5.1. HITL primitives (`session:inquire` approval/clarification with bubble-up + cascade-down), follow-up cascade through parent chain, async `spawn_mission`, `session:notify_subagent` parent-note, `requires_approval` skill gate, system/hub skill split (`assets/system/` embed-only + `assets/skills/` hub-installed), embed-only constitution + prompts (`pkg/prompts`), 9 HITL scenarios scaffolded | shipped (`d0611e3`, 2026-05-13) — 7 of 9 validated on Gemma 4-26B; multi_root_independence + adapter-visibility deferred → 5.1b |
| ~~5 (combined HITL + compactor)~~ | **split 2026-05-12** — HITL was production blocker; compactor independent. Became 5.1 (shipped) + 5.2 (open). |
| 5.1b. Adapter visibility surface — capability-bag `liveview` extension (FrameObserver / ChildFrameObserver / StatusReporter), enriched `SessionStatusPayload`, multi-root harness primitive + `multi_root_independence` scenario, `wait_subagents` optional ids + fast-fail | shipped (`b243b77`, 2026-05-14) |
| 5.1c. Bubble Tea TUI adapter — reference rich-client on top of 5.1b liveview surface; multi-root tabs, inquiry modal, sidebar driven by `ExtensionFrame{liveview/status}`, on-attach replay, dogfood polish | shipped (`410df3d`, 2026-05-15) |
| 5.1c.async-root. Root defaults to `wait="async"` + three-bucket follow-up/new-mission/conversational classifier + mandatory announce-on-spawn user reply + TUI `✓ mission … completed` marker. Skill-prompt-driven; no new runtime primitives. Bundles two dogfood doc fixes (hugr-data `schema-type_fields` semantic-search params, `_skill_builder` slim + de-autoload). 5/5 PASS on Gemma 26B | shipped (`dc077ab`, 2026-05-15) |
| 5.1c.cancel-ux. User-initiated mission cancel: `/mission` modal listing in-flight children with `j/k/c/Shift+C` keys; Esc-Esc double-press as panic-cancel-all. Fast-cancel discipline (Cancel{Cascade:true} → SessionClose with `user_cancel:` skip-close-turn prefix). Async result delivery (idle-fold + auto-summary turn on `SubagentResult{AsyncNotify}`). | shipped (`355ad48`, 2026-05-15) |
| 5.x.skill-polish-1. `_root` Bucket D (clarify before route) + verify-mission-alive pre-check; analyst category enumeration via `discovery-search_module_data_objects` so workers don't bail on first matched table; TUI cancel-ux R-followups (modal live rebuild on liveview status, async-summary flag gated by route). | in-progress (branch `019-mini-5.x.skill-polish-1`) |
| 5.2. Compactor — content-aware history summarisation; replaces phase-3 `defaultHistoryWindow=50` stop-gap; composes with 4.2.3 notepad (notepad = cross-mission, compactor = within-session) | open — not yet specced |
| 6. Cron + scheduler | open |
| 7. Memory pipeline + LLM Wiki — cross-conversation distilled knowledge. Reads 4.2.3 notepad as input; session-scoped working memory is owned by 4.2.3. | open |
| 8. Artifacts | open |
| 9. A2A adapter | open (defer until needed) |
| 10. Multi-party Workspaces (human + agent) | open — lands after A2A |
| Backlog | see `design/002-runtime-canonical/backlog.md` |

Goal: finish design-001 cleanly, then move to **hub integration**
(container packaging, deployment story, hub-spawned mode). Hub work is
explicitly deferred until design-001 is complete — `phase-3.5-spec.md
§Out of scope` and `design.md §16.8`.

## Active focus — 5.x.skill-polish-1 in flight; 5.x.subagent-lifetime + 5.3.policy-ux queued

Phase 5.1c.cancel-ux shipped to `main` at `355ad48` (PR #18,
2026-05-15) — `/mission` modal + Esc-Esc panic-cancel + fast-
cancel discipline + async-result idle-fold + auto-summary turn.
Independent code review surfaced one race (Cancel-before-
SessionClose ordering) and seven R-items; race fixed in-PR, 4
of 7 R-items rolled into 5.x.skill-polish-1.

- **5.x.skill-polish-1** *(in flight)* — skill prose + cancel-ux
  R-followups. `_root` gets Bucket D (clarify before route) +
  verify-mission-alive pre-check on Bucket A. `analyst` gets a
  CATEGORY classifier shape that enumerates ALL matching tables
  via `discovery-search_module_data_objects` before drilling in
  (stops the "bail on first found table" pattern). TUI modal
  rebuilds on each liveview status frame so cancelled rows
  disappear naturally; async-summary flag now armed in
  RouteBuffered (not first switch) so wait_subagents-consumed
  AsyncNotify results don't fire redundant summary turns.
- **5.x.subagent-lifetime** *(queued)* — don't auto-close
  subagents on `SubagentResult`; let model dismiss or follow-up
  the still-alive child. Phase-sized, needs deliberate design.
- **5.3.policy-ux** *(queued)* — HITL "always allow / always
  deny" keybinds + remote admin policies.
- **5.4.workspace-tree** *(queued, escalation only)* — if Path 1
  (SESSION_DIR ephemeral manifest warning, shipped 2026-05-15)
  doesn't hold, restructure to root-rooted workspace tree with
  per-subagent subdirs.

One longer-horizon option remains parallel-mergeable:

- **5.2** — compactor: content-aware history summarisation at
  turn boundaries; preserves recent turns verbatim, summarises
  older into a system-prompt digest. Composes with 4.2.3
  notepad (cross-mission) vs compactor (within-session). Not
  yet specced.

5.2 unblocks long-running sessions on context-window-tight
models.

Free-agenda items (cross-phase, no scheduled phase): live in
`backlog.md`. Includes B1–B7 (4.2.3 fast-follows), content-
based `requires_approval`, task-complexity routing, PeerGroup
mesh.

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
├── adapter/{console,http,webui}  # transport adapters (console has inline HITL renderer)
├── auth/{perm,sources,template}  # 3-tier permission stack + auth.Service loopback
├── config/                       # YAML schema + StaticService views (incl. Hitl, Subagents)
├── extension/{plan,whiteboard,notepad,skill,mcp,workspace}  # capability-bag extensions
├── identity/{local,hub}          # who-am-I providers
├── model/, models/, protocol/    # LLM routing + Frame protocol (incl. InquiryRequest/Response)
├── prompts/                      # embed-only template renderer (phase 5.1)
├── session/                      # Session, Manager, inquiry routing state, turn loop
│   └── manager/                  # multi-root supervisor + RestoreActive
├── skill/                        # Manifest parser + SkillManager + Store (system/hub/local/inline)
├── store/local/                  # embedded DuckDB persistence
└── tool/                         # ToolManager + providers + Policies
assets/
├── constitution/                 # tier-{root,mission,worker}.md + agent.md (embed-only)
├── prompts/                      # bundled template tree (embed-only, phase 5.1)
├── python/requirements.txt       # bundled analyst venv package list
├── system/                       # agent-core skills (`_root`, `_mission`, …; embed-only)
└── skills/                       # hub-tier bundled skills (analyst, hugr-data, duckdb-*, python-runner)
vendor/
└── mcp-server-motherduck/        # vendored MotherDuck DuckDB MCP (git submodule, MIT)
design/001-agent-runtime/         # historical: per-phase specs, original design, original architecture (gitignored)
design/002-runtime-canonical/     # canonical: design.md, architecture.md, backlog.md, phase-N-spec.md (gitignored)
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
