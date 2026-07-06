-- 0.0.9: agents.role — the Hugr auth role the hub token issuer stamps into
-- the agent's JWT (spec-hub-side §1.3, D10: admin-assigned at agent creation).
-- Nullable with default to stay portable across DuckDB/Postgres ALTERs; empty
-- or NULL reads as 'agent'.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS role VARCHAR DEFAULT 'agent';
UPDATE agents SET role = 'agent' WHERE role IS NULL;
