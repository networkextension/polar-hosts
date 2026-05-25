-- ============================================================
-- polar_hosts: host_skills publisher/install_id/source (P1a)
--
-- Adds 3 columns so the table can distinguish:
--   - builtin skills (publisher='', install_id='', source='builtin')
--   - bundle skills installed via P1a's skill.install flow
--     (publisher set, install_id non-empty, source='bundle')
--
-- Old rows get defaults that match their current semantics — none
-- were ever installed via skill.install, so source='builtin' is the
-- safe backfill.
--
-- Apply:
--   psql -d polar_hosts -f scripts/migrate/host-skills-publisher.sql
-- ============================================================

ALTER TABLE host_skills
    ADD COLUMN IF NOT EXISTS publisher  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS install_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source     TEXT NOT NULL DEFAULT 'builtin';

-- Partial index so the marketplace UI can quickly answer "which hosts
-- have publisher X's skill installed?" without scanning the whole table.
CREATE INDEX IF NOT EXISTS idx_host_skills_publisher
    ON host_skills (publisher, kind)
    WHERE source = 'bundle';

INSERT INTO host_schema_version (version)
SELECT 3
WHERE NOT EXISTS (SELECT 1 FROM host_schema_version WHERE version = 3);
