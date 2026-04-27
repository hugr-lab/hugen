-- ==========================================================================
-- Migration 0.0.5 — Persistent Tool Policies (spec 009 / phase 4)
--
-- Additive-only. Adds:
--   • tool_policies — persistent overrides for default approval behavior
--
-- Hot-cached by *PolicyStore (atomic.Pointer[PolicySnapshot]).
-- Reads are lock-free; writes serialize on a writer mutex and
-- atomic-swap the snapshot.
--
-- Indexes are POSTGRES-ONLY. DuckDB stays index-free (per project
-- rule + the table is small and entirely cached in memory).
-- ==========================================================================

CREATE TABLE IF NOT EXISTS tool_policies (
    agent_id    VARCHAR NOT NULL,
    tool_name   VARCHAR NOT NULL,                 -- exact name OR prefix glob ("data-*")
    scope       VARCHAR NOT NULL,                 -- "global" | "skill:<name>" | "role:<skill>:<role>"
    policy      VARCHAR NOT NULL,                 -- "always_allowed" | "manual_required" | "denied"
    note        VARCHAR,
    created_by  VARCHAR NOT NULL,                 -- "user" | "llm" | "system"
    created_at  {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }} NOT NULL,
    updated_at  {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }} NOT NULL,
    PRIMARY KEY (agent_id, tool_name, scope)
);

{{ if isPostgres }}
-- Per-scope filter index for snapshot rebuilds and policy_list.
CREATE INDEX IF NOT EXISTS idx_policies_agent_scope
    ON tool_policies (agent_id, scope);
{{ end }}
