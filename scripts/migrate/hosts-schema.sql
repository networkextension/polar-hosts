-- ============================================================
-- polar_hosts schema — end-state.
--
-- Apply:
--   CREATE DATABASE polar_hosts OWNER ideamesh;
--   psql -d polar_hosts -f scripts/migrate/hosts-schema.sql
--
-- The hosts plugin owns agent_tokens (the polar-agent WS auth), hosts
-- (named machines), host_skills (operator-enabled per-host skills),
-- host_skill_runs (one row per start/stop attempt), host_skill_credentials
-- (env-injected secrets), and console_layouts (per-user pane layouts
-- on /console.html).
--
-- Cross-DB references (TEXT, resolved via dock SDK):
--   - user_id, owner_user_id, created_by  → /internal/v1/users/:id
--   - workspace_id                        → /internal/v1/teams/:id
--
-- Reverse refs from dock (handled in cutover PR — dock drops the FK):
--   - bot_users.host_skill_id  → was FK to host_skills(id), becomes TEXT pointer
--   - bot_users.auto_registered_by_token → was FK to agent_tokens(id), becomes TEXT
--
-- See doc/arch/database-ownership.md.
-- ============================================================

-- Polar-agent WS auth tokens. token_hash = sha256 of plaintext;
-- plaintext shown once at issue time. host_* columns populated on
-- first WS attach (uname + hostname + remote-ip).
CREATE TABLE IF NOT EXISTS agent_tokens (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    token_hash TEXT NOT NULL UNIQUE,
    coder_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    host_os TEXT NOT NULL DEFAULT '',
    host_arch TEXT NOT NULL DEFAULT '',
    host_name TEXT NOT NULL DEFAULT '',
    host_ip TEXT NOT NULL DEFAULT '',
    last_used_at TIMESTAMPTZ,
    last_attached_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_tokens_user ON agent_tokens(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS hosts (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    slug TEXT NOT NULL,
    name TEXT NOT NULL,
    agent_token_id TEXT REFERENCES agent_tokens(id) ON DELETE SET NULL,
    os TEXT NOT NULL DEFAULT '',
    arch TEXT NOT NULL DEFAULT '',
    last_seen_ip TEXT NOT NULL DEFAULT '',
    advertised_skills_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_seen_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (workspace_id, slug)
);
CREATE INDEX IF NOT EXISTS idx_hosts_workspace ON hosts(workspace_id);
CREATE INDEX IF NOT EXISTS idx_hosts_last_seen ON hosts(last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_hosts_agent_token
    ON hosts(agent_token_id) WHERE agent_token_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS host_skills (
    id BIGSERIAL PRIMARY KEY,
    host_id TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    config_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    auto_start BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_host_skills_host ON host_skills(host_id);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_host_skills_host_kind
    ON host_skills(host_id, kind);

CREATE TABLE IF NOT EXISTS host_skill_runs (
    id BIGSERIAL PRIMARY KEY,
    skill_id BIGINT NOT NULL REFERENCES host_skills(id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'starting',
    pid INTEGER,
    listen_addr TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    log_path TEXT NOT NULL DEFAULT '',
    meta_json JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_skill_runs_skill ON host_skill_runs(skill_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_skill_runs_active
    ON host_skill_runs(status) WHERE status IN ('starting', 'running');

-- Encrypted credentials injected as env at skill.start time.
-- AES-GCM via POLAR_CREDENTIAL_KEY env (same key used by dock today).
CREATE TABLE IF NOT EXISTS host_skill_credentials (
    id BIGSERIAL PRIMARY KEY,
    host_skill_id BIGINT NOT NULL REFERENCES host_skills(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    value_cipher TEXT NOT NULL DEFAULT '',
    value_plaintext TEXT NOT NULL DEFAULT '',
    encrypted BOOLEAN NOT NULL DEFAULT FALSE,
    last_used_at TIMESTAMPTZ,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (host_skill_id, key)
);
CREATE INDEX IF NOT EXISTS idx_host_skill_credentials_skill
    ON host_skill_credentials(host_skill_id);

-- console_layouts: per-user named layouts for /console.html. panes_config
-- is a JSONB array of {host_id, host_skill_id}.
CREATE TABLE IF NOT EXISTS console_layouts (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    owner_user_id TEXT NOT NULL,
    name TEXT NOT NULL,
    panes_config JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (workspace_id, owner_user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_console_layouts_owner
    ON console_layouts(workspace_id, owner_user_id, updated_at DESC);
