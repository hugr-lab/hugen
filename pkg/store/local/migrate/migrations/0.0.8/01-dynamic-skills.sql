-- ==========================================================================
-- Migration 0.0.8 — Phase 6.2.db dynamic skills (skills + skill_log +
-- skill_links)
--
-- DB-backed skill source: an on-disk bundle stays the content; these
-- tables are the discovery/index/usage layer.
--
--   • skills      — one index row per dynamic-skill bundle. `metadata`
--                   JSON carries the full metadata.hugen projection
--                   (mission roles / task inputs_schema served without
--                   a SKILL.md read); the filter columns are
--                   denormalised from it for cheap WHERE. `source` is
--                   the reapability/provenance anchor; `pin` bypasses
--                   the discovery bandit. `description_vec` mirrors the
--                   memory_items embedding column (gated by VectorSize).
--   • skill_log   — append-only usage telemetry {shown,loaded,used}.
--   • skill_links — m2m edges (catalog_member / autoload / requires /
--                   related). Plain junction with two FKs to skills.id
--                   (mirrors memory_links — NOT a tagged is_m2m table,
--                   so the junction stays addressable for visibility
--                   control); no parent_id self-FK on skills.
--
-- `shared` is a forward-compat column (cross-agent visibility, db-3).
-- Indexes are POSTGRES-ONLY; DuckDB stays index-free per the existing
-- rule.
-- ==========================================================================

CREATE TABLE IF NOT EXISTS skills (
    id                VARCHAR PRIMARY KEY,
    agent_id          VARCHAR NOT NULL,
    shared            BOOLEAN NOT NULL DEFAULT FALSE,
    name              VARCHAR NOT NULL,
    type              VARCHAR NOT NULL DEFAULT 'skill',
    description       VARCHAR,
    task_eligible     BOOLEAN NOT NULL DEFAULT FALSE,
    task_kind         VARCHAR,
    keywords          VARCHAR[],
    tier_compat       VARCHAR[],
    has_inputs_schema BOOLEAN NOT NULL DEFAULT FALSE,
    metadata          {{ if isPostgres }}JSONB{{ else }}JSON{{ end }} NOT NULL,
    pin               BOOLEAN NOT NULL DEFAULT FALSE,
    source            VARCHAR NOT NULL,
    version           VARCHAR,
    content_hash      VARCHAR,
    bundle_path       VARCHAR,
    owner             VARCHAR,
    created_at        {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }} NOT NULL,
    updated_at        {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }} NOT NULL,
    installed_at      {{ if isPostgres }}TIMESTAMPTZ{{ else }}TIMESTAMP{{ end }}
    {{ if gt .VectorSize 0 }},description_vec {{ if isPostgres }}vector({{ .VectorSize }}){{ else }}FLOAT[{{ .VectorSize }}]{{ end }}{{ end }}
);

CREATE TABLE IF NOT EXISTS skill_log (
    id          VARCHAR PRIMARY KEY,
    skill_id    VARCHAR NOT NULL,
    agent_id    VARCHAR NOT NULL,
    event       VARCHAR NOT NULL,
    session_id  VARCHAR,
    details     {{ if isPostgres }}JSONB{{ else }}JSON{{ end }},
    created_at  {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }} NOT NULL
);

CREATE TABLE IF NOT EXISTS skill_links (
    agent_id    VARCHAR NOT NULL,
    source_id   VARCHAR NOT NULL,
    target_id   VARCHAR NOT NULL,
    relation    VARCHAR NOT NULL,
    created_at  {{ if isPostgres }}TIMESTAMPTZ DEFAULT NOW(){{ else }}TIMESTAMP DEFAULT CURRENT_TIMESTAMP{{ end }} NOT NULL
);

{{ if gt .VectorSize 0 }}
{{ if isPostgres }}CREATE INDEX IF NOT EXISTS skills_desc_vss ON skills USING hnsw (description_vec vector_cosine_ops);{{ end }}
{{ end }}

{{ if isPostgres }}
CREATE INDEX IF NOT EXISTS idx_skills_agent       ON skills (agent_id, type);
-- (agent_id, source, name) is the upsert identity tuple — UNIQUE so a
-- TOCTOU double-insert fails loud instead of silently duplicating.
CREATE UNIQUE INDEX IF NOT EXISTS uq_skills_agent_source_name ON skills (agent_id, source, name);
CREATE INDEX IF NOT EXISTS idx_skills_task        ON skills (agent_id, task_eligible);
CREATE INDEX IF NOT EXISTS idx_skill_log_skill    ON skill_log (skill_id, event);
CREATE INDEX IF NOT EXISTS idx_skill_log_agent    ON skill_log (agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_skill_links_source ON skill_links (agent_id, source_id, relation);
CREATE INDEX IF NOT EXISTS idx_skill_links_target ON skill_links (agent_id, target_id, relation);
{{ end }}
