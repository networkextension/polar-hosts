-- ============================================================
-- polar_hosts: skill_catalog (P0b)
--
-- Catalog of installable .skill bundles. One row per (publisher,
-- skill_kind, version). Used by:
--   - GET  /api/skill-catalog        — list installable
--   - POST /api/skill-catalog        — register (admin)
--   - GET  /api/skill-catalog/:id    — detail
--   - POST /api/skill-catalog/:id/retire
--   - GET  /api/skill-catalog/:id/download  — proxy (or 302 to download_url)
--
-- workspace_id semantics (P3 will enforce, P0b just stores):
--   - is_platform=TRUE, workspace_id=''  → visible to every workspace
--   - is_platform=FALSE, workspace_id=<wid>  → only visible to that workspace
--
-- Apply:
--   psql -d polar_hosts -f scripts/migrate/skill-catalog.sql
-- ============================================================

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS skill_catalog (
    id              TEXT PRIMARY KEY,           -- "scat_<random>"
    publisher       TEXT NOT NULL,              -- e.g. com.networkextension.kdp
    skill_kind      TEXT NOT NULL,              -- e.g. kdp, mcp.lldb
    version         TEXT NOT NULL,              -- e.g. 0.3.0
    sha256          TEXT NOT NULL,              -- hex; 64 chars
    download_url    TEXT NOT NULL,              -- absolute URL to .skill ZIP
    size_bytes      BIGINT NOT NULL,
    manifest_json   JSONB NOT NULL,             -- parsed manifest.yaml; nullable fields preserved

    display_name    TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    license         TEXT NOT NULL DEFAULT '',
    homepage        TEXT NOT NULL DEFAULT '',

    -- P2 hookpoint; left empty in P0b
    publisher_pubkey TEXT NOT NULL DEFAULT '',
    signed_at        TIMESTAMPTZ,

    -- P3 hookpoints; defaults make it platform-wide in P0b
    is_platform     BOOLEAN NOT NULL DEFAULT TRUE,
    workspace_id    TEXT NOT NULL DEFAULT '',   -- empty = platform; non-empty = workspace-scoped

    created_by      TEXT NOT NULL,              -- user_id
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at      TIMESTAMPTZ,

    UNIQUE (publisher, skill_kind, version)
);

CREATE INDEX IF NOT EXISTS idx_skill_catalog_lookup
    ON skill_catalog (publisher, skill_kind, version)
    WHERE retired_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_skill_catalog_workspace
    ON skill_catalog (workspace_id, retired_at);

CREATE INDEX IF NOT EXISTS idx_skill_catalog_platform
    ON skill_catalog (is_platform, retired_at);

-- Schema version bookkeeping (so future migrations can branch on it)
INSERT INTO host_schema_version (version)
SELECT 2
WHERE NOT EXISTS (SELECT 1 FROM host_schema_version WHERE version = 2);
