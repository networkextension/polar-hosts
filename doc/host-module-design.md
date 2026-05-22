# Host Module — Design Note

**Status**: design recorded, not implemented yet. See [host-module-dev.md](./host-module-dev.md) for the phase plan.

**Date**: 2026-05-13

## Goal

Generalize `polar-agent` from "an AI tool runner" into a **named remote
host** that can serve a dynamic catalog of capabilities (proxy server,
WireGuard tunnel, code execution, serial console, interactive shell,
…). One operator UI manages many hosts. Each host advertises what it
can do; the operator picks and configures.

Today an operator who has a Mac on a LAN can only use it to run
`kimi`/`claude`/`codex` for chat-bound coding tasks. After this work,
the same Mac can also:

- Expose an HTTP/SOCKS5 proxy out of its network zone
- Host a WireGuard endpoint for remote access
- Stream a serial console (e.g. our dpaa2 board) into chat
- Drop the user into an interactive shell from chat
- Run any future skill compiled into polar-agent

…all selected and configured through Polar's UI, without re-deploying
or hand-editing config files on the host.

## Why this matters

- One host = one Polar Workspace member that can do meaningful work.
  Multi-host fleets become legible (status, capability, current load).
- New capabilities are a single Go file (`cmd/polar-agent/skills/foo.go`)
  + a small UI card, not a fork of `polar-agent attach`.
- Existing `polar-agent attach --bot=… --tool=…` flow becomes one
  special case (the `coder` skill) under the new umbrella; the legacy
  args path keeps working via a compatibility shim during transition.

## Concept Model

```
Workspace
 └─ Host                              (one entry per registered machine)
     ├─ AdvertisedSkill[]             (what the agent compiled in)
     └─ EnabledSkill[]                (operator-chosen + configured)
         └─ SkillRun[]                (each start/stop attempt)
```

| Entity            | Lifetime                              | Source of truth                      |
|---|---|---|
| `Host`            | Registered → forever (or revoked)     | dock DB                              |
| `AdvertisedSkill` | Refreshed each agent attach           | dock DB (snapshot from agent advertise) |
| `EnabledSkill`    | Operator-managed                      | dock DB                              |
| `SkillRun`        | One per start; ends on stop/crash     | dock DB; live state mirrored over WS |

**Why three concepts and not one**: an operator may want a `coder`
skill on the host that isn't currently running (e.g. between jobs).
"Enabled" persists across agent reconnects; "Run" is the in-flight
instance.

## Data Model

Three new tables. Reuses the existing `agent_tokens` table for agent
authentication and `teams` for workspace ownership.

```sql
CREATE TABLE hosts (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    slug TEXT NOT NULL,                                  -- url-friendly handle
    name TEXT NOT NULL,
    agent_token_id TEXT REFERENCES agent_tokens(id),     -- enrollment token
    os TEXT,                                             -- darwin / linux / freebsd
    arch TEXT,                                           -- arm64 / amd64
    last_seen_ip INET,
    advertised_skills_json JSONB NOT NULL DEFAULT '[]',  -- snapshot from last attach
    last_seen_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (workspace_id, slug)
);
CREATE INDEX idx_hosts_workspace ON hosts(workspace_id);
CREATE INDEX idx_hosts_last_seen ON hosts(last_seen_at DESC);

CREATE TABLE host_skills (
    id BIGSERIAL PRIMARY KEY,
    host_id TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,                                  -- coder / shell / proxy / wireguard / kdp / ...
    name TEXT NOT NULL,                                  -- operator-given label
    config_json JSONB NOT NULL DEFAULT '{}',             -- per-skill schema
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    auto_start BOOLEAN NOT NULL DEFAULT TRUE,            -- start on agent attach
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_host_skills_host ON host_skills(host_id);

CREATE TABLE host_skill_runs (
    id BIGSERIAL PRIMARY KEY,
    skill_id BIGINT NOT NULL REFERENCES host_skills(id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'starting',             -- starting / running / stopping / stopped / failed
    pid INTEGER,
    listen_addr TEXT,                                    -- e.g. "127.0.0.1:1080" for proxy
    error_message TEXT,
    log_path TEXT                                        -- /tmp/polar.skill.<id>.log on agent host
);
CREATE INDEX idx_skill_runs_skill ON host_skill_runs(skill_id, started_at DESC);
CREATE INDEX idx_skill_runs_status ON host_skill_runs(status) WHERE status IN ('starting', 'running');
```

Schema bootstrap follows the [FK ordering rule](#fk-ordering-rule)
documented in `store.go::applySchema` (post PR #178): cross-table
references go through late-bound `DO $$ ... ADD CONSTRAINT $$` blocks.

## Protocol

The agent already maintains a long-lived WS connection to dock
(`/ws/agent`). Add four message types under that channel:

### agent → dock

```json
{
  "type": "skill.advertise",
  "skills": [
    {"kind": "coder",     "version": "1.0", "capabilities": {"tools": ["kimi", "claude", "codex"]}},
    {"kind": "shell",     "version": "1.0", "capabilities": {}},
    {"kind": "proxy",     "version": "1.0", "capabilities": {"protocols": ["http", "socks5"]}},
    {"kind": "wireguard", "version": "1.0", "capabilities": {}},
    {"kind": "kdp",   "version": "1.0", "capabilities": {}}
  ],
  "host": {"os": "darwin", "arch": "arm64", "hostname": "locals-Mac"}
}
```

Sent once immediately after WS handshake. Dock writes
`hosts.advertised_skills_json` + auto-resolves the host_id from the
agent_token.

```json
{
  "type": "skill.event",
  "skill_id": 42,
  "run_id": 1337,
  "event": "state",                     // state | log | metric | exit
  "data": {"status": "running", "listen_addr": "127.0.0.1:1080"}
}
```

State changes, log lines, periodic metrics, and the final exit code
all flow as `skill.event`. Dock persists state changes into
`host_skill_runs`; log/metric events fan out to subscribed UI clients
without DB roundtrips.

### dock → agent

```json
{
  "type": "skill.start",
  "skill_id": 42,
  "run_id": 1337,                       // dock pre-allocates this
  "kind": "proxy",
  "config": {"protocol": "socks5", "listen_addr": "127.0.0.1:1080", "upstream": "direct"}
}
```

```json
{
  "type": "skill.stop",
  "run_id": 1337,
  "reason": "operator"                  // operator | restart | host_disabled
}
```

### Reconciliation

On WS reconnect, agent compares its in-memory skill_run state with
dock's record:

1. Agent sends `skill.advertise` again (capabilities may have changed
   if binary was upgraded).
2. Dock replies with a `skill.reconcile` carrying the expected state
   for every `enabled=true, auto_start=true` skill on this host.
3. Agent starts any missing run, sends `skill.event` for state changes.
4. If dock has an active `run_id` that agent doesn't know about, agent
   reports `state=failed reason=lost_on_restart` so dock can finalize
   the row.

This mirrors the [orphan-stream recovery pattern](#related-pieces) we
discussed for chat streaming — restart-safe state convergence via Redis
or DB scan.

## UI

Two new pages under the workspace nav.

### `/hosts` — list

```
┌────────────────────────────────────────────────────────────┐
│ locals-Mac.local              🟢 online · seen 2s ago      │
│ darwin/arm64 · ssh local@127.0.0.1:5722                    │
│ 4 skills running · 1 idle                                  │
│ [coder×2] [shell] [proxy 127.0.0.1:1080]                   │
└────────────────────────────────────────────────────────────┘
┌────────────────────────────────────────────────────────────┐
│ dpaa2-board-01                🟢 online · seen 15s ago     │
│ freebsd/arm64 · LAN-only                                   │
│ 1 skill running                                            │
│ [kdp /dev/cu.usbserial-DT03V19V]                       │
└────────────────────────────────────────────────────────────┘
[+ Add host]
```

Card list sorted by `last_seen_at DESC`. Click → detail.

### `/hosts/<slug>` — detail

Tabs: **Skills** · **Logs** · **Settings** · **Danger**

```
Skills tab:

Advertised by agent              Enabled on this host
────────────────────────────     ──────────────────────────────────────
✓ coder  v1.0                    ✓ coder("kimi", workdir=/research)        [▶ running] [⏸] [⚙]
✓ shell  v1.0                    ✓ coder("claude", workdir=/Polar-)        [▶ running] [⏸] [⚙]
✓ proxy  v1.0                    + Enable shell
✓ wireguard v1.0                 + Enable proxy
✓ kdp v1.0                   + Enable wireguard
                                 + Enable kdp
```

Per-skill row shows: state pill, last 3 log lines (collapsible), pause/
resume/edit buttons. Edit opens a per-skill config form (different per
kind).

### Add-Host flow

UI shows a one-time enrollment payload (token + bootstrap command):

```
On the host you want to enroll, run:

  curl -fsSL https://polar.example.com/install/agent | \
    POLAR_TOKEN=polar_eyJ... bash

Or if you already have polar-agent installed:

  polar-agent register --token=polar_eyJ... --name="my-mac"
```

Token is single-use, 1-hour TTL. After enroll, agent gets a permanent
`agent_token` row and the host appears on the list page.

## Skill Catalog

Each skill is a single Go file under `cmd/polar-agent/skills/` plus a
matching UI form spec under `ui/src/hosts/skill-forms/<kind>.ts`.

| Kind         | Purpose                          | Process model               | Critical config           |
|---|---|---|---|
| `coder`      | Run kimi-cli/claude/codex (existing path) | fork-per-message | tool, workdir, env       |
| `shell`      | Interactive bash to chat user    | pty + bidi WS bytes         | initial cwd, env, timeout |
| `proxy`      | HTTP/SOCKS5 outbound proxy       | sing-box/shadowsocks subproc | protocol, listen, upstream |
| `wireguard`  | WG server or client endpoint     | wg-quick + iface             | peer config, endpoint     |
| `kdp`    | Serial console to embedded board | pty + bidi (routed: `?…` to AI) | device, baud, KDP UDP port |
| `mcp-server` | Run MCP servers for chat tool-use | spawn mcp executable        | server type, args         |

Each skill implements:

```go
type Skill interface {
    Kind() string
    Validate(config json.RawMessage) error
    Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error)
}

type Run interface {
    ID() int64
    Status() RunStatus           // periodic poll, debounced events to dock
    Events() <-chan SkillEvent   // log/state/metric stream
    Stop(reason string) error    // graceful, then SIGTERM after 10s
}
```

## Lifecycle / State Machine

```
              register
  (nothing) ─────────────> Host(no skills)
                              │
            enable + auto_start │
                              ▼
                          host_skills row created
                              │  (agent reconciles on next event)
                              ▼
                          skill.start sent
                              │
                              ▼  starting
                          host_skill_runs.status=starting
                              │
                              ▼  state=running
                          host_skill_runs.status=running
                              │
                ┌──────────┼──────────┐
                ▼          ▼          ▼
            stopped   crashed    stop (operator)
            (status=     (status=    (status=
             stopped)    failed)     stopping → stopped)
```

State transitions are dock-side, driven by `skill.event`. Failed runs
are NOT auto-restarted by default — operator decides. A future
`auto_restart` flag on `host_skills` can flip this behavior per skill.

## Security

1. **Agent token scope**: each `agent_token` is workspace-scoped; the
   dock validates that the host's `workspace_id` matches the token's
   on every WS handshake.
2. **Skill-level access**: enabling/configuring/starting/stopping a
   skill requires `TeamRoleAdmin` on the host's workspace. Plain members
   can only view status.
3. **Skill subprocess identity**: skills run as the OS user that owns
   the polar-agent process. No additional sandboxing — operator is
   expected to give polar-agent the principle-of-least-privilege user
   they're comfortable with (and not root, ever).
4. **Config secrets**: WireGuard private keys, proxy upstream creds,
   etc. live in `host_skills.config_json` encrypted at rest using the
   existing `IOSDIST_RESOURCE_KEY` AES-GCM pattern (or a new key, TBD;
   see [open question 1](#open-questions)).
5. **Log content**: skill logs may contain secrets; dock fans them out
   only to subscribed authenticated UI clients on the host's workspace.
   Logs are also NOT persisted to DB long-term — `log_path` points to a
   bounded local file on the agent host.

## Bot ↔ Host Coupling

Existing `bot_users` keep their `agent_token` link, but the resolution
changes:

```
bot received message
  ↓
look up bot_users.bot_user_id → bot row
  ↓
bot.host_skill_id IS NULL?    ↓                   bot.host_skill_id IS NOT NULL?
  ↓ (legacy)                                       ↓ (host-bound)
attach-style passthrough via                       look up host_skills row
the agent for the bot's
agent_token (current path)                         dispatch to that host's
                                                   coder skill via WS
                                                   (run_id may be active or
                                                   spun up on demand)
```

This means multiple bots can **share** a host's coder skill (one agent
process, multiple bot personas routed to it) — a real saving once you
have 6 bots all pointed at the same Mac.

## Why static skill set, not plugins

We considered Go shared-library plugins (build-time `-buildmode=plugin`).
Rejected:

1. macOS Go plugins are notoriously fragile across Go minor versions —
   plugin built with Go 1.23 won't load in a host running 1.24.
2. The dynamic surface (operator drops a .so file) is also the attack
   surface — we'd need to sign + verify plugins, which is a
   non-trivial security pipeline.
3. Compile-time skills are zero-config, zero-failure-mode, and the
   agent binary is already small (~10 MB) so doubling the skill count
   is cheap.

Trade-off accepted: adding a new skill kind requires a polar-agent
release (cross-compile + rsync). The release cadence is operator-paced
(roughly weekly in current usage) which we consider acceptable.

## Open Questions

1. **Secret encryption**: reuse `IOSDIST_RESOURCE_KEY` for the
   `host_skills.config_json` blob, or introduce a separate
   `HOST_SKILL_SECRET_KEY` to isolate the trust domain? Leaning
   toward separate, since the iOS dist team and the host ops team are
   not always the same humans. Cost: one more env var to deploy.
2. **Log retention**: agent-side files are bounded but un-rotated. Add
   a `--log-retention-days` flag with logrotate-style behavior, or
   delegate to operator? Leaning delegate; surface log_path in UI but
   don't manage rotation.
3. **Single-skill-instance enforcement**: a `wireguard` skill probably
   can't have two concurrent runs on the same host (port collision +
   iface name collision). Should the data model encode "max 1 active
   run per (host, kind)", or let the skill's Validate fail at start?
   Leaning skill-side validation — keeps the schema generic.
4. **Skill cross-references**: e.g. a `coder` skill should be able to
   "use the proxy on this same host for outbound LLM API calls". How
   does that get expressed in config? Probably symbolic refs
   (`{"http_proxy": "$skill:proxy-1"}`) resolved at start time by the
   agent. Defer to a Phase 7 once we have ≥2 skills that need it.

## Related pieces

- [orphan stream recovery](#) (chat streaming) — the
  `skill.reconcile` mechanism we're proposing here is a generalization
  of the streaming recovery pattern. May share infrastructure.
- [polar-agent container](./agent-container-design.md) — when the
  container design lands, "Add Host" UI can offer a one-line
  `docker run` instead of `curl | bash`.
- [system-agent capability D — thread summary](./system-agent-design.md)
  — analogous "agent advertises capabilities, server consumes them"
  pattern; this design borrows that vocabulary intentionally.

## FK ordering rule

> Cross-table FK constraints added inline in an `ALTER TABLE ADD COLUMN`
> can break fresh-install bootstrap if the target table doesn't exist
> yet in the same `db.Exec(schema)` batch. Always: declare the column
> first as a plain type, then add the FK via a `pg_constraint`-guarded
> `DO $$` block AFTER the referenced table's `CREATE`. See `store.go`
> `markdown_entries_workspace_id_fkey` for the canonical example.

This is a project-wide rule landed in PR #178; the host module's three
new tables must follow it for any cross-references introduced.
