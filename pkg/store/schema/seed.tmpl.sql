-- Memory Storage — Seed Data (Go template)
--
-- Initial data for standalone mode. Executed at first startup
-- AFTER schema.tmpl.sql creates tables.
-- Idempotent: INSERT OR IGNORE (DuckDB) / ON CONFLICT DO NOTHING (PostgreSQL).
--
-- Template params:
--   .AgentType.ID          string
--   .AgentType.Name        string
--   .AgentType.Description string
--   .AgentType.Config      string — pre-escaped JSON literal
--   .Agent.ID              string
--   .Agent.ShortID         string
--   .Agent.Name            string

-- ============================================================
-- Seed agent type
-- ============================================================

{{ if isDuckDB }}
INSERT OR IGNORE INTO agent_types (id, name, description, config)
VALUES (
    '{{ .AgentType.ID }}',
    '{{ .AgentType.Name }}',
    '{{ .AgentType.Description }}',
    '{{ .AgentType.Config }}'::JSON
);
{{ end }}

{{ if isPostgres }}
INSERT INTO agent_types (id, name, description, config)
VALUES (
    '{{ .AgentType.ID }}',
    '{{ .AgentType.Name }}',
    '{{ .AgentType.Description }}',
    '{{ .AgentType.Config }}'::JSONB
)
ON CONFLICT (id) DO NOTHING;
{{ end }}

-- ============================================================
-- Seed agent instance
-- ============================================================

{{ if isDuckDB }}
INSERT OR IGNORE INTO agents (id, agent_type_id, short_id, name, status)
VALUES (
    '{{ .Agent.ID }}',
    '{{ .AgentType.ID }}',
    '{{ .Agent.ShortID }}',
    '{{ .Agent.Name }}',
    'active'
);
{{ end }}

{{ if isPostgres }}
INSERT INTO agents (id, agent_type_id, short_id, name, status)
VALUES (
    '{{ .Agent.ID }}',
    '{{ .AgentType.ID }}',
    '{{ .Agent.ShortID }}',
    '{{ .Agent.Name }}',
    'active'
)
ON CONFLICT (id) DO NOTHING;
{{ end }}
