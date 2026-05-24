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
`design/004-runtime-post-phase-i/` (consolidated 2026-05-20
after Phase I shipped). `design/002-runtime-canonical/` and
`design/003-mission-pdca/` remain readable as historical
baselines; `design/001-agent-runtime/` is preserved as the
record of *why* each early phase ended up the way it did.

**Read first** when touching session / extension / tool / skill /
mission internals:

- `design/004-runtime-post-phase-i/architecture.md` — state-of-
  the-tree map. §11 is the extension recipe book. §15 is the
  mission-PDCA runtime.
- `design/004-runtime-post-phase-i/design.md` — vision + phase
  plan + current shipped state.
- `design/004-runtime-post-phase-i/backlog.md` — consolidated
  free agenda (B1–B14 + queued mini-phases).
- Per-phase specs: `phase-4.2-*.md`, `phase-4.2.2-*.md`,
  `phase-4.2.3-*.md`, `phase-5.1-hitl-followups-async.md`,
  `phase-5.1b-spec.md`, `phase-5.1c-tui-bubbletea.md`,
  `phase-5.1c.async-root.md`, `phase-5.1c.cancel-ux.md`,
  `phase-5.2-root-as-chat.md` (all under 002 until promoted to
  004), and `003-mission-pdca/{design,spec}.md` for the
  pre-implementation mission-PDCA canon.

When 004 and an earlier directory disagree, **004 wins**.

Phase plan:

| Phase | Status |
| ----- | ------ |
| 1. Native core + ModelRouter + console UI (console deprecated 2026-05-21, TUI is the only interactive adapter from PR #23 review-fixes) | shipped |
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
| 5.x.skill-polish-1. `_root` Bucket D (clarify before route) + verify-mission-alive pre-check; analyst category enumeration via `discovery-search_module_data_objects` so workers don't bail on first matched table; TUI cancel-ux R-followups (modal live rebuild on liveview status, async-summary flag gated by route). | shipped (`9be1fdd`, 2026-05-16, PR #19) |
| 5.2 root-as-chat. Bundled with 5.4.b workspace + 5.4.c.1-10 weak-model hardening + Stage 3 FromName + analyst SKILL review. 18 commits. | shipped (`1c0e0a7`, 2026-05-18, PR #21) |
| A + I. Mission-PDCA runtime (`pkg/extension/mission`) — planner / checker / synthesizer executor; `mission:validate_and_approve` atomic validate+inquire+marker stamp (frame-only); skill-agnostic approval state with `invalidates_plan_approval` worker signal; `mission_goal` + `mission_acceptance_criteria` in plan with runtime AC-gated `finish` (synthetic amend coercion); 28 sub-phases (I.1-I.28). | shipped (`e83b003`, 2026-05-20, PR #22) |
| 5.2. Compactor — content-aware history summarisation (`pkg/extension/compactor`). TurnBoundaryHook capability; FrameObserver-maintained boundary index; hybrid trigger (turn-count + abs token budget); per-Kind dispatch with incremental SummaryBlocks + cap-driven collapse; pure-chat short-circuit (no SummaryBlock emitted when range carries no tool calls / inquiries); inquiry Q/A preserved verbatim in KeptVerbatim; KeptVerbatim FIFO cap with first-user-message pin; three-layer config resolver (operator YAML view → tier overlay → skill manifest overrides); StatusReporter projection; TUI inline marker rendering with payload-driven `ui_marker_enabled` flag; `/compactor status|reset|compact` slash commands; lossless fanout (blocking subscriber send + ctx escape + 30s warn). Live Gemma 4-26B dogfood pass + PR #23 review-fixes (console adapter removed; S1-S6 + B2 closed; B19-B25 deferred to η). Known gap: `s.history` live truncation lands in η. | shipped on `025-phase-5.2-compactor` (PR #23 review fixes 2026-05-21) |
| B13 + B15 (mission-research-and-approval Step 1+2) — planner-signalled `requires_reapproval` flag replaces sha256 frame-hashing; mission research stage as runtime primitive (`mission.research:` block, `auto` predicate, ≤3 iter batched inquire); analyst SKILL migration; weak-model hardening (review fixes R1-R5 + S1-S10). | shipped (`f88d98a`, 2026-05-24, PR #25) |
| B11 (mission-research-and-approval Step 4) — Structured acceptance criteria with stable `ac-N` identity. Eight sub-phases: α data model + state helpers (Seed/Stage/Commit/Discard/ApplyStatusOnly/ApplyWorkerSatisfies); β planner `ac_add` / `ac_update` schema + diff merge + auto-promote modal on any contract change; γ manifest `acceptance_criteria` iter-0 seed (Go template against `.Inputs`); δ checker `ac_update[]` by id + finish gate reads `state.AC`; ε worker `satisfies: ["ac-N"]` shorthand; ζ approval modal structured diff renderer (✓ / ▸ / + / ✗ icons + [EDITED]/[NEW]/[DROPPED] tags); η synthesis evidence trail + analyst SKILL prose. | shipped on `027-mission-research-approval-pr2` (PR pending 2026-05-24) |
| 6. Cron + scheduler | open |
| 7. Memory pipeline + LLM Wiki — cross-conversation distilled knowledge. Reads 4.2.3 notepad as input; session-scoped working memory is owned by 4.2.3. | open |
| 8. Artifacts | open |
| 9. A2A adapter | open (defer until needed) |
| 10. Multi-party Workspaces (human + agent) | open — lands after A2A |
| Backlog | see `design/004-runtime-post-phase-i/backlog.md` |

Goal: finish design-001 cleanly, then move to **hub integration**
(container packaging, deployment story, hub-spawned mode). Hub work is
explicitly deferred until design-001 is complete — `phase-3.5-spec.md
§Out of scope` and `design.md §16.8`.

## Active focus — B11 shipped; §4.6 modal v2 + Phase 6 (cron) next

B11 (structured AC) shipped on `027-mission-research-approval-pr2`
(2026-05-24, PR pending). Eight sub-phases α-θ deliver the
identity-bearing acceptance-criteria model the mission-research-
and-approval spec §3 describes — replacing the I.26 string-list
AC with `state.AC []AcceptanceCriterion` carrying stable `ac-N`
ids + status + evidence trail. Planner emits diffs (`ac_add` /
`ac_update`), checker updates status-only by id, worker can
shorthand `satisfies: ["ac-N"]`, finish gate reads `state.AC`,
approval modal renders ✓/▸/+/✗ diff with [EDITED]/[NEW]/[DROPPED]
tags, synthesis sees per-AC evidence trail.

PR #25 (`f88d98a`, 2026-05-24) closed B13 + B15 in one bundle:
planner-signalled re-approval + research stage primitive + weak-
model hardening (R1-R5 + S1-S10).

Phase 5.2 (compactor) — `025-phase-5.2-compactor` lands α-ε in
one PR. Phase I (PR #22, `e83b003`, 2026-05-20) closed the
mission-PDCA work. Phase 5.2 root-as-chat (PR #21, `1c0e0a7`,
2026-05-18) bundled weak-model hardening + analyst SKILL review.
Phase 5.x.skill-polish-1 (PR #19, `9be1fdd`, 2026-05-16) closed
cancel-ux R-followups + Bucket-D clarify.

- **§4.6 Approval modal v2** *(queued next, ~450 LOC)* — 4
  options (`approve` / `approve with tools` / `reject` /
  `refine`) + tool auto-approve. Required before Phase 6 cron
  (scheduled missions cannot block on HITL).
- **B12 — mission visibility surface (TUI)** *(queued)* —
  approval-modal AC diff already in place via ζ; status bar
  tool count + persistent mission status panel still pending.
- **5.3.policy-ux** *(queued)* — HITL "always allow / always
  deny" keybinds + remote admin policies.
- **Phase 6 cron + scheduler** *(queued after §4.6)* — first
  periodic / wake-up primitive. Depends on modal-v2's
  auto-approve-tools.

Phase plan beyond 5.2: 6 cron → 7 memory pipeline → 8 artifacts
→ 9 A2A → 10 workspaces. See `design/004-runtime-post-phase-i/
design.md §4.3` for the table.

Закрытые 5.x: **5.4.workspace-tree** дропнут — workspace
закрылся на уровне 5.4.b в PR #21 (workspace-as-extension);
Path 2 (root-rooted tree restructure) не понадобился.
**5.x.subagent-lifetime** дропнут — остаток от заброшенного
`023-wave-lifecycle-hooks` эксперимента; в пост-Phase-A
архитектуре handoffs-by-ref + executor-driven flow закрыли use
case полностью.

Free-agenda items (cross-phase): see
`design/004-runtime-post-phase-i/backlog.md`. B1-B14 +
mini-phase tracker.

## Project structure

```text
cmd/
├── hugen/                # main binary — runtime bootstrap, tui + webui adapters
└── hugen-skill-validate/ # CLI: validate a SKILL.md manifest
mcp/
├── bash-mcp/             # in-tree shell + filesystem MCP (per_session)
├── hugr-query/           # in-tree Hugr GraphQL → file output (per_agent)
└── python-mcp/           # in-tree Python execution + lazy per-session venv (per_agent)
pkg/
├── adapter/{http,tui,webui}  # transport adapters (tui owns slash-parse + inquiry helpers)
├── auth/{perm,sources,template}  # 3-tier permission stack + auth.Service loopback
├── config/                       # YAML schema + StaticService views (incl. Hitl, Subagents)
├── extension/{plan,whiteboard,notepad,skill,mcp,workspace,liveview,mission}  # capability-bag extensions
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
design/001-agent-runtime/         # historical: per-phase specs, original design (gitignored)
design/002-runtime-canonical/     # historical: pre-mission-PDCA canon + phase specs (gitignored)
design/003-mission-pdca/          # historical: mission-PDCA canon (gitignored)
design/004-runtime-post-phase-i/  # canonical: design.md, architecture.md, backlog.md (gitignored)
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
