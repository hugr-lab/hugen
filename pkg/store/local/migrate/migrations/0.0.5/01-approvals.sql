-- ==========================================================================
-- Migration 0.0.5 — HITL Approvals (spec 009 / phase 4)
--
-- Additive-only. Adds:
--   • approvals — persistent HITL approval rows (gate-pause + ask_coordinator)
--
-- The `args` column carries the original tool arguments verbatim.
-- The `approval_requested` event metadata carries an args_digest
-- (preview), NOT the full args — bytes never duplicate into the
-- events stream.
--
-- Indexes are POSTGRES-ONLY. DuckDB stays index-free for this table
-- (per project rule + sparse cardinality assumption: most rows
-- terminate within the 30m default timeout or seconds via auto-approve).
-- ==========================================================================

CREATE TABLE IF NOT EXISTS approvals (
    id                  VARCHAR PRIMARY KEY,
    agent_id            VARCHAR NOT NULL,
    mission_session_id  VARCHAR NOT NULL,    -- sub-agent whose tool call is gated
    coord_session_id    VARCHAR NOT NULL,    -- coordinator surfacing the approval
    tool_name           VARCHAR NOT NULL,    -- "ask_coordinator" for ask-variant
    args                {{ if isPostgres }}JSONB{{ else }}JSON{{ end }} NOT NULL,
    risk                VARCHAR NOT NULL,    -- low | medium | high
    status              VARCHAR NOT NULL,    -- pending | approved | rejected | modified | expired
    response            {{ if isPostgres }}JSONB{{ else }}JSON{{ end }},
    created_at          {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }} NOT NULL,
    responded_at        {{ if isPostgres }}TIMESTAMPTZ{{ else }}TIMESTAMP{{ end }}
);

{{ if isPostgres }}
-- Hot-path lookup: "all pending approvals on this coord session for
-- this agent". Used by the mission-tick resume path and policy_list
-- on the coordinator side.
CREATE INDEX IF NOT EXISTS idx_approvals_coord
    ON approvals (agent_id, coord_session_id, status);
{{ end }}
