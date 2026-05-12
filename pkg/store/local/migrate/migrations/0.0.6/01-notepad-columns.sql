-- ==========================================================================
-- Migration 0.0.6 — Phase 4.2.3 notepad columns
--
-- Additive-only. Adds four columns to session_notes so the post-4.2.3
-- notepad surface (notepad:append/read/search/show) can persist its
-- richer write payload:
--
--   • category      — model-supplied open-string filtering tag.
--   • author_role   — runtime-set tier of writing session
--                     (agent | coordinator | worker).
--   • mission       — model-supplied short context phrase.
--   • embedding     — Vector column (gated on .VectorSize). Hugr
--                     populates server-side via the `summary:` mutation
--                     parameter under the @embeddings directive on
--                     session_notes.
--
-- Append-only — no UPDATEs are issued anywhere in the runtime for
-- session_notes; the read surface uses a 48h window for eviction
-- rather than soft-delete or archival columns. Existing rows get
-- NULL in the new text columns (and NULL embeddings) — they remain
-- readable via notepad:read but won't surface from notepad:search
-- until they're re-written, which is fine for the working-memory
-- framing.
-- ==========================================================================

-- Match the pre-existing pattern (migration 0.0.2): plain
-- ALTER TABLE ADD COLUMN. DuckDB does NOT support the `IF NOT
-- EXISTS` clause on ALTER TABLE ADD COLUMN, and the migration
-- runner records the applied version so each migration script
-- runs at most once per DB. Idempotency is unnecessary.

ALTER TABLE session_notes ADD COLUMN category    VARCHAR;
ALTER TABLE session_notes ADD COLUMN author_role VARCHAR;
ALTER TABLE session_notes ADD COLUMN mission     VARCHAR;

{{ if gt .VectorSize 0 }}
ALTER TABLE session_notes ADD COLUMN embedding
    {{ if isPostgres }}vector({{ .VectorSize }}){{ else }}FLOAT[{{ .VectorSize }}]{{ end }};
{{ end }}
