-- v4 backfill: one agents row per existing hosts row.
--
-- The dock + polar-hosts schemas both grow an `agents` table on the
-- v4 cutover (doc/arch/agent-identity-v4.md in main repo). The
-- application doesn't auto-backfill; this script generates a stable
-- ag_<random32hex> for every existing host that doesn't yet have an
-- agent.
--
-- Idempotent: re-running on a partially-backfilled DB skips rows that
-- already have a matching agents row (host_id match).
--
-- Run on .57 against polar_hosts (and against ideamesh with the same
-- shape — see scripts/migrate/v4-backfill-agents.sql in polar-dock).
--   psql -d polar_hosts -f scripts/migrate/v4-backfill-agents.sql
--
-- Pre-flight: confirm hosts has the v4 columns the schema sync should
-- have added (run scripts/migrate/hosts-schema.sql on the DB first if
-- it doesn't):
--   SELECT column_name FROM information_schema.columns
--    WHERE table_name='hosts'
--      AND column_name IN ('mem_peak_bytes','cpu_peak_pct','first_seen_at');
-- Expected: 3 rows.

INSERT INTO agents (id, workspace_id, host_id, name, agent_token_id,
                    bot_user_id, os, arch, created_at, last_hello_at)
SELECT 'ag_' || encode(gen_random_bytes(16), 'hex'),
       h.workspace_id, h.id, h.name, h.agent_token_id,
       NULL,
       NULLIF(h.os, ''),
       NULLIF(h.arch, ''),
       h.created_at,
       h.last_seen_at
  FROM hosts h
 WHERE h.agent_token_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.host_id = h.id);

-- Sanity-check the backfill result.
SELECT COUNT(*) AS agents_total,
       SUM(CASE WHEN last_hello_at IS NULL THEN 1 ELSE 0 END) AS never_helloed
  FROM agents;
