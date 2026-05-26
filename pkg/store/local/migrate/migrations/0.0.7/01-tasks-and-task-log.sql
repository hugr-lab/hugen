-- ==========================================================================
-- Migration 0.0.7 — Phase 6 tasks + task_log
--
-- Adds the two storage tables that back the TaskManager extension:
--
--   • tasks     — one row per user-defined schedule entry. Lifecycle
--                 UPDATEs only (pause / resume / cancel / spec edit).
--                 Zero per-fire UPDATEs.
--   • task_log  — pure append-only event log: every fire emits >= 2
--                 rows (planned then terminal completed/failed), plus
--                 optional intermediate started/log rows. NO UPDATEs.
--                 Hypertable on Timescale.
--
-- The schedule anchor (next planned fire) lives in task_log as a
-- `planned` row inserted at task-create time (fire_seq=1) and at the
-- end of every fire (fire_seq=N+1). No `next_fire_at` column on
-- `tasks` — derived via the (task_id, event_type, fire_seq DESC,
-- created_at DESC) index. See phase-6-spec.md §2.7 for the
-- update-minimisation invariants.
--
-- Phase 6 also allows the new `cron` value on `sessions.session_type`
-- alongside the existing `root` / `subagent` / `fork`. The column
-- itself is unchanged (no CHECK constraint to widen); the migration
-- is a no-op for that — Go-side, pkg/protocol gains the
-- SessionKindCron constant.
-- ==========================================================================

-- Phase 6 / 0.0.7 deliberately stores task timestamps in TIMESTAMPTZ
-- on BOTH backends (DuckDB included). Existing tables use a naive
-- TIMESTAMP on DuckDB for legacy reasons; tasks need correct TZ
-- handling because Runner reads `planned_at` and compares it
-- against the wall clock for the due check.

CREATE TABLE IF NOT EXISTS tasks (
    id                VARCHAR PRIMARY KEY,
    agent_id          VARCHAR NOT NULL,
    kind              VARCHAR NOT NULL,
    status            VARCHAR NOT NULL DEFAULT 'active',
    schedule_kind     VARCHAR NOT NULL,
    owner_session_id  VARCHAR NOT NULL,
    skill_ref         VARCHAR,
    spec              {{ if isPostgres }}JSONB{{ else }}JSON{{ end }} NOT NULL,
    pause_reason      VARCHAR,
    created_at        TIMESTAMPTZ DEFAULT {{ if isPostgres }}NOW(){{ else }}CURRENT_TIMESTAMP{{ end }} NOT NULL,
    updated_at        TIMESTAMPTZ DEFAULT {{ if isPostgres }}NOW(){{ else }}CURRENT_TIMESTAMP{{ end }} NOT NULL
);

CREATE TABLE IF NOT EXISTS task_log (
    id          VARCHAR   {{ if not .IsTimescale }}PRIMARY KEY{{ end }},
    task_id     VARCHAR   NOT NULL,
    agent_id    VARCHAR   NOT NULL,
    fire_seq    INTEGER   NOT NULL,
    event_type  VARCHAR   NOT NULL,
    planned_at  TIMESTAMPTZ NOT NULL,
    session_id  VARCHAR,
    outcome     {{ if isPostgres }}JSONB{{ else }}JSON{{ end }},
    content     VARCHAR,
    created_at  TIMESTAMPTZ DEFAULT {{ if isPostgres }}NOW(){{ else }}CURRENT_TIMESTAMP{{ end }} NOT NULL
    {{ if .IsTimescale }}, PRIMARY KEY (created_at, id){{ end }}
);

{{ if .IsTimescale }}
SELECT create_hypertable('task_log', 'created_at', if_not_exists => TRUE);
{{ end }}

{{ if isPostgres }}
CREATE INDEX IF NOT EXISTS idx_tasks_active
    ON tasks (agent_id, status) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_tasks_owner
    ON tasks (agent_id, owner_session_id);

CREATE INDEX IF NOT EXISTS idx_task_log_latest
    ON task_log (task_id, event_type, fire_seq DESC, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_task_log_started
    ON task_log (agent_id, created_at)
    WHERE event_type = 'started';
{{ end }}
