-- ==========================================================================
-- Migration 0.0.3 — Persistent Artifact Registry (spec 008)
--
-- Additive-only. Adds:
--   • artifacts        — metadata for files published by sessions
--   • artifact_grants  — explicit access overlay (widen-only)
--
-- Bytes live OUTSIDE hub.db in the active Storage backend
-- (pkg/artifacts/storage). Manager resolves them via
-- Storage.Open(ObjectRef{Backend, Key}).
--
-- Indexes are POSTGRES-ONLY. DuckDB stays index-free for these tables.
-- ==========================================================================

CREATE TABLE IF NOT EXISTS artifacts (
    id              VARCHAR PRIMARY KEY,
    agent_id        VARCHAR NOT NULL,
    name            VARCHAR NOT NULL,
    type            VARCHAR NOT NULL,
    storage_key     VARCHAR NOT NULL,
    storage_backend VARCHAR NOT NULL,
    original_path   VARCHAR,
    description     VARCHAR NOT NULL,
    {{ if gt .VectorSize 0 }}description_embedding {{ if isPostgres }}vector({{ .VectorSize }}){{ else }}FLOAT[{{ .VectorSize }}]{{ end }},{{ end }}
    session_id          VARCHAR NOT NULL,
    mission_session_id  VARCHAR,
    derived_from        VARCHAR,
    visibility          VARCHAR NOT NULL DEFAULT 'self',
    created_at          {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }},
    size_bytes          BIGINT,
    row_count           BIGINT,
    col_count           INTEGER,
    file_schema         {{ if isPostgres }}JSONB{{ else }}JSON{{ end }},
    tags                VARCHAR[],
    ttl                 VARCHAR NOT NULL DEFAULT 'session'
);

CREATE TABLE IF NOT EXISTS artifact_grants (
    artifact_id  VARCHAR NOT NULL,
    agent_id     VARCHAR NOT NULL,
    session_id   VARCHAR NOT NULL,
    granted_by   VARCHAR NOT NULL,
    granted_at   {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }},
    PRIMARY KEY (artifact_id, agent_id, session_id)
);

{{ if isPostgres }}
CREATE INDEX IF NOT EXISTS idx_art_agent
    ON artifacts (agent_id);
CREATE INDEX IF NOT EXISTS idx_art_session
    ON artifacts (agent_id, session_id);
CREATE INDEX IF NOT EXISTS idx_art_mission
    ON artifacts (mission_session_id) WHERE mission_session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_art_ttl
    ON artifacts (ttl, created_at) WHERE ttl <> 'permanent';
CREATE INDEX IF NOT EXISTS idx_art_grants_target
    ON artifact_grants (agent_id, session_id);
{{ if gt .VectorSize 0 }}
CREATE INDEX IF NOT EXISTS artifacts_desc_vss
    ON artifacts USING hnsw (description_embedding vector_cosine_ops);
{{ end }}
{{ end }}
