# Host Module — Development Plan

**Status**: planning. P0 starts after this doc lands.

**Companion**: [host-module-design.md](./host-module-design.md) — the
"why" and "what". This doc is the "how" and "in what order".

## Phase plan

Six phases. Phase 0 is the load-bearing one (schema + protocol +
advertise+enroll); Phases 1–5 are one-skill-each and ship
independently. Phase 6 is wrap-up (docs + retiring legacy paths).

| Phase | Scope                                              | Est LOC | Depends on | Risk |
|---|---|---|---|---|
| P0    | Schema + agent advertise + dock state + host list UI | 600 Go + 250 TS | — | high (foundation) |
| P1    | Coder skill — migrate existing `attach --bot --tool` to host_skill | 400 Go + 120 TS | P0 | medium (legacy compat) |
| P2    | Shell skill — pty + bidi WS                          | 500 Go + 250 TS | P0 | medium (terminal UX) |
| P3    | Proxy skill — sing-box-managed HTTP/SOCKS5           | 300 Go + 80 TS | P0 | low |
| P4    | WireGuard skill — wg-quick                           | 400 Go + 150 TS | P0 | medium (peer config UX) |
| P5    | KDP skill — interactive serial console with `?` routing | 450 Go + 180 TS | P0 + P2 (shell reused) | low |
| P6    | Polish: retire bare `polar-agent attach`, deprecate `bot_users.agent_token` direct path | 100 Go + audit | P1–P5 | low |

P0 + P1 are the gates. Once those merge, downstream phases can be
parallelized.

## Phase 0 — Foundation

### Tasks

| # | Task | File |
|---|---|---|
| 0.1 | Schema bootstrap: 3 new tables + indexes + FK guards | `internal/app/dock/store.go::applySchema` |
| 0.2 | `hosts` CRUD store helpers | `internal/app/dock/hosts_store.go` (new) |
| 0.3 | `host_skills` CRUD + enable/disable | `internal/app/dock/hosts_store.go` |
| 0.4 | `host_skill_runs` create/update/list | `internal/app/dock/hosts_store.go` |
| 0.5 | `Skill` interface + skill registry on agent side | `cmd/polar-agent/skills/skill.go` (new pkg) |
| 0.6 | Agent advertise on WS handshake | `cmd/polar-agent/attach.go` (extend) |
| 0.7 | Dock side: receive advertise, persist `advertised_skills_json` | `internal/app/dock/agent_hub.go` (extend) |
| 0.8 | `skill.start` / `skill.stop` / `skill.event` message handlers (server side) | `internal/app/dock/host_skill_dispatch.go` (new) |
| 0.9 | Same on agent side: dispatch incoming `skill.start` to the right Skill instance | `cmd/polar-agent/skills/runner.go` (new) |
| 0.10 | Host enrollment endpoint: `POST /api/hosts/enroll` + token gen | `internal/app/dock/host_handlers.go` (new) |
| 0.11 | Host list + detail handlers: `/api/hosts`, `/api/hosts/:id`, `/api/hosts/:id/skills` | same |
| 0.12 | UI page: `/hosts.html` + `/hosts/<id>.html` + `ui/src/hosts.ts` | `ui/public/hosts.html` (new), `ui/src/hosts.ts` (new) |
| 0.13 | UI: "Add Host" modal + token paste-back | embedded in `hosts.ts` |
| 0.14 | Unit tests: Skill registry, advertise/dispatch protocol, host store CRUD | `*_test.go` |

### Out of scope for P0

- No actual skills run yet — only the framework exists.
- "Add host" only registers; with zero advertised skills, the host
  detail page shows "No skills available from this agent — install
  P1+ to enable".

### Acceptance

- `dropdb && createdb && go run ./cmd/dock` boots cleanly with the
  3 new tables visible in `psql -d ideamesh -c '\dt'`.
- A polar-agent built from this branch attaches and the host appears
  on `/hosts.html` within 1s.
- Operator can hit "+ Add Host", get a token, paste-run on a second
  machine, and see two hosts on the list.
- All existing flows (`bot_users` with `agent_token`, chat passthrough)
  unchanged.

## Phase 1 — Coder skill

### Tasks

| # | Task | File |
|---|---|---|
| 1.1 | `Coder` skill implementation: tool resolution, fork-per-message | `cmd/polar-agent/skills/coder.go` (new) — wraps existing `tools.go` logic |
| 1.2 | Legacy `polar-agent attach --bot --tool` becomes "implicit host_skill" — on attach, auto-create a `host_skills{kind:coder, config:{tool, workdir}}` row if not already present | `cmd/polar-agent/attach.go` + `internal/app/dock/agent_hub.go` |
| 1.3 | `bot_users.host_skill_id` nullable column + migration | `store.go::applySchema` |
| 1.4 | Bot reply routing: when `bot.host_skill_id` is set, dispatch to that host_skill instead of legacy passthrough | `internal/app/dock/ai_agent.go` |
| 1.5 | UI: skill config form for `coder` (tool dropdown, workdir input, env editor) | `ui/src/hosts/skill-forms/coder.ts` |
| 1.6 | UI on bot edit page: optional "bind to host coder skill" picker | `ui/src/bots.ts` (extend) |
| 1.7 | Tests: legacy `attach` still works; new bind-to-host flow; multiple bots sharing one coder | `*_test.go` |

### Migration story

```
Before P1                         After P1
─────────────────────────────     ────────────────────────────────
bot A → agent token X             bot A → host_skill 42 (coder, kimi)
        attach --tool=kimi              host H1 ┐
bot B → agent token X             bot B → host_skill 42  (same)
        attach --tool=kimi
                                  bot C → host_skill 43 (coder, claude)
                                          host H1 (same physical host, ┘
                                                   different skill row)
```

Existing operators don't change anything; the auto-shim in 1.2 keeps
their `attach --tool=` invocations producing the right host_skill rows
on demand.

### Acceptance

- Bot with no `host_skill_id` set: routes to legacy attach path
  (current behavior unchanged).
- Bot with `host_skill_id` set: routes to that host's coder skill;
  skill_run row created per LLM call; latency / token counters work.
- Two bots both bound to the same host_skill: serialized to one
  agent's subprocess fork — no double-spawn.
- Killing the agent process: subsequent bot reply marks the skill_run
  failed cleanly (same recovery pattern as streaming).

### Auth model — hybrid (P1c.4 + manual on-host)

The coder CLIs (claude / codex / kimi-cli) have two auth modes:

| Tier | How | Polar's role |
|---|---|---|
| **API key** (pay-per-token) | `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `MOONSHOT_API_KEY` env var | **Polar manages**: paste once into `/hosts/:id` skill config form; dock encrypts via `POLAR_CREDENTIAL_KEY` (AES-256-GCM) into `host_skill_credentials`; agent injects as env at `skill.start` time |
| **OAuth / login** (Claude Pro, ChatGPT Plus subscription) | `claude login` / `codex login` opens a browser; credentials persist in `~/.claude/credentials.json` etc. | **Operator manages**: ssh to the host once, run `claude login` per CLI. polar-agent inherits the on-disk creds when it spawns the CLI subprocess |

The vault endpoints (P1c.4) are `GET/PUT/DELETE /api/hosts/:id/skills/:skillId/credentials[/:key]`, admin-only, never surface raw plaintext on read. When `POLAR_CREDENTIAL_KEY` is not set on the dock box, credentials store plaintext + the UI badges them "unencrypted" so operators fix the deployment before going to production. Decryption happens server-side only at dispatch time (P1c.3); the raw value never crosses an API boundary back to a browser.

Generate the credential key:

```bash
openssl rand -hex 32   # 64-char hex output → POLAR_CREDENTIAL_KEY
```

Then set it in the polar-dock launch env (parallel to `IOSDIST_RESOURCE_KEY`).

## Phase 2 — Shell skill

A "live shell to chat" capability. Operator at chat keyboard, host's
bash on the other end.

### Tasks

| # | Task | File |
|---|---|---|
| 2.1 | `Shell` skill: spawn `bash -i` in a pty, bidi byte multiplexing to dock | `cmd/polar-agent/skills/shell.go` |
| 2.2 | New WS channel: `/ws/host/<id>/shell/<run_id>` — bytes both ways | `internal/app/dock/host_shell_handlers.go` |
| 2.3 | xterm.js integration on UI side | `ui/src/hosts/skill-views/shell.ts` |
| 2.4 | Skill config form: initial cwd + env + max session minutes | `ui/src/hosts/skill-forms/shell.ts` |
| 2.5 | Resize protocol: UI resize → dock → agent → `TIOCSWINSZ` | shell.go + UI |
| 2.6 | Audit log: every shell session start/end → workspace audit log | `host_shell_handlers.go` |
| 2.7 | Tests: pty echo, ctrl-c, resize, idle-timeout | `*_test.go` |

### Security posture

- Only `TeamRoleAdmin` of the host's workspace can open a shell.
- Audit log every session start with timestamp, operator user_id,
  workspace_id, initial cwd. Session content NOT logged.
- Idle timeout default 30 min — config knob raises ceiling, can't go
  unbounded (hard cap 6h).

## Phase 3 — Proxy skill

### Tasks

| # | Task | File |
|---|---|---|
| 3.1 | `Proxy` skill: spawn sing-box subprocess with operator-supplied config | `cmd/polar-agent/skills/proxy.go` |
| 3.2 | Sing-box binary lookup at start (`exec.LookPath`; agent doesn't bundle it) | same |
| 3.3 | Skill config: protocol (http/socks5), listen_addr (local-only by default), upstream chain | `ui/src/hosts/skill-forms/proxy.ts` |
| 3.4 | Status panel: bytes/s, active connections (parse sing-box clash API or just `lsof -i :port`) | shell on detail page |
| 3.5 | Tests: spawn, port collision detection, sing-box missing binary | `*_test.go` |

### Why sing-box specifically

Multi-protocol (http, socks5, vmess, vless, hysteria2, …) and a single
binary. If operator wants shadowsocks-rust or v2ray-rust, a separate
skill kind can be added later — keep proxy.go specifically sing-box.

## Phase 4 — WireGuard skill

### Tasks

| # | Task | File |
|---|---|---|
| 4.1 | `WireGuard` skill: write `/tmp/polar.wg.<run_id>.conf`, `sudo wg-quick up <iface>` | `cmd/polar-agent/skills/wireguard.go` |
| 4.2 | Iface naming convention: `polar-wg-<run_id>` to avoid collision | same |
| 4.3 | Skill config: peer config blob, endpoint, listen_port (server mode) | `ui/src/hosts/skill-forms/wireguard.ts` |
| 4.4 | Sudoers note in docs: agent user needs passwordless `sudo wg-quick` | dev doc + `cmd/polar-agent/install-script` |
| 4.5 | Status: `wg show <iface>` parsed into UI (peer endpoint, last handshake, transfer) | proxy.go pattern reused |
| 4.6 | Tests: config generation, iface name collision, peer add/remove | `*_test.go` |

### Security note

WireGuard requires root for `wg-quick up`. Either:

(a) polar-agent runs as root (current convention on the test box,
   actually false — agent runs as `local`)

(b) operator adds passwordless sudo for `wg-quick` to the agent's user

Option (b) is the documented path. We'll generate a snippet in the
install-script output:

```
echo "%polar-agent ALL=(root) NOPASSWD: /opt/homebrew/bin/wg-quick" | sudo tee /etc/sudoers.d/polar-agent-wg
```

## Phase 5 — KDP skill

See [kdp interactive chat design — separate brief, TBD]. Shell
(P2) infrastructure is reused; only the routing rule (`?` prefix → AI
context, else → pty stdin) is skill-specific.

### Tasks

| # | Task | File |
|---|---|---|
| 5.1 | `KDP` skill: spawn kdp in pty + ring buffer of last 100 lines | `cmd/polar-agent/skills/kdp.go` |
| 5.2 | Routing rule: input starting with `?` or `/ai` → AI request with buffer as context; else → pty stdin | same |
| 5.3 | Skill config: device path, baud (default 115200,N,8,1), KDP UDP port | `ui/src/hosts/skill-forms/kdp.ts` |
| 5.4 | Reuses chat WS for output streaming (same as shell, different render: typed `kdp_output` instead of pure bytes) | UI |
| 5.5 | Tests: route correctness, buffer overflow, exit-on-disconnect | `*_test.go` |

## Phase 6 — Polish / retire legacy

### Tasks

| # | Task |
|---|---|
| 6.1 | Mark `polar-agent attach --bot --tool` as deprecated in `--help`; print stderr warning recommending `polar-agent register` |
| 6.2 | Drop legacy `attach` code path in a separate PR once telemetry shows < 1% usage |
| 6.3 | Update README.md + relevant doc pages to point at host module first |
| 6.4 | Backfill `host_skills` rows for any active `bot_users.agent_token` that's been hit in the last 30 days, so operators see their existing setup in the new UI |

## File / package layout (planned)

```
cmd/polar-agent/
├── skills/                    # NEW
│   ├── skill.go               # Skill interface + registry
│   ├── runner.go              # dispatcher; receives skill.start, manages Run instances
│   ├── coder.go               # P1
│   ├── shell.go               # P2
│   ├── proxy.go               # P3
│   ├── wireguard.go           # P4
│   ├── kdp.go             # P5
│   └── *_test.go              # unit tests for each
├── attach.go                  # extended in P0 (advertise) + P1 (legacy shim)
└── ...

internal/app/dock/
├── hosts_store.go             # NEW (P0) — CRUD for hosts, host_skills, host_skill_runs
├── host_handlers.go           # NEW (P0) — /api/hosts* HTTP handlers
├── host_skill_dispatch.go     # NEW (P0) — server-side skill.start/stop/event router
├── host_shell_handlers.go     # NEW (P2) — /ws/host/.../shell pty bridge
├── agent_hub.go               # extended (P0) — handle skill.advertise + skill.event
├── ai_agent.go                # extended (P1) — bot.host_skill_id routing
└── store.go::applySchema      # extended (P0) — 3 new tables + (P1) bot_users.host_skill_id column

ui/src/
├── hosts.ts                   # NEW (P0) — list page
├── host-detail.ts             # NEW (P0) — detail page
├── hosts/
│   ├── skill-forms/           # NEW — per-skill config form widgets
│   │   ├── coder.ts           # P1
│   │   ├── shell.ts           # P2
│   │   ├── proxy.ts           # P3
│   │   ├── wireguard.ts       # P4
│   │   └── kdp.ts         # P5
│   └── skill-views/           # NEW — per-skill live views (e.g. terminal for shell)
│       ├── shell.ts           # P2
│       └── kdp.ts         # P5
└── ...

ui/public/
├── hosts.html                 # NEW (P0)
└── host-detail.html           # NEW (P0)

doc/
├── host-module-design.md      # this PR
├── host-module-dev.md         # this PR
└── (per-phase) host-skill-<kind>.md   # added with each phase PR
```

## Registration topology (P0 → P1e)

Polar supports **two** registration shapes depending on where dock + agent run:

| Topology | Use when | Flow | Auth |
|---|---|---|---|
| **1:1 co-located** | dock + agent on the same machine (dev box, single-node deploy) | `polar-agent register --local` hits `http://127.0.0.1:8080/api/hosts/local-bootstrap` directly — no nginx | **Loopback bind + no proxy headers**. Server rejects requests whose `c.Request.RemoteAddr` isn't `127.0.0.1` / `::1`, or that carry `X-Forwarded-For` / `X-Real-IP` / `Forwarded`. Opt-in via `POLAR_ALLOW_LOCAL_BOOTSTRAP=true` on the dock launch env (`~/polar-dock.env`); a stock remote-only deploy doesn't register the route. |
| **1:N remote** | many polar-agents distributed; central dock | Admin opens `/hosts.html` → "Add Host" → copies one-time enroll token; operator runs `polar-agent register --token=<enroll> --server=https://dock-host` on the target machine | The one-time enroll token (TTL 1h, single-use) is the bearer on `POST /api/hosts/register`. Server consumes it + mints a permanent agent_token in the response. |

Both modes save `~/.polar/agent.toml` with the URL the agent should use for `attach`. **Local mode saves `http://127.0.0.1:8080` (direct dock)** so WS attach also bypasses nginx — fewer hops on the local round-trip. Remote mode saves whatever `--server` was passed (typically the public TLS URL).

### Picking a mode

- Single Mac (or single Linux box) running dock + your own agent + your own UI? → **local**.
- Internal cluster, dev machines, CI runners all connecting back to a central dock? → **remote** for each agent.
- Mixed (one local agent on the dock box + N remote)? → use both. The dock box uses `--local`; remotes use `--token`.

### How `--local` security works (in 3 lines)

```go
host, _, _ := net.SplitHostPort(c.Request.RemoteAddr)
if !isLoopbackAddr(host) { return 403 }            // 127.0.0.1 / ::1 only
for hdr in {X-Forwarded-For, X-Real-IP, Forwarded} { if hdr present { return 403 } }
```

We use `c.Request.RemoteAddr` (raw socket peer), **not** `c.ClientIP()` — the gin helper consults `X-Forwarded-For` and would defeat the loopback guard.

## API surface (planned, P0 first)

```
# Hosts
POST   /api/hosts/enroll                          # body: {workspace_id, name} → {token, expires_at}        (1:N: admin mints)
POST   /api/hosts/register                        # agent uses this with the enroll token → creates host    (1:N: agent consumes)
POST   /api/hosts/local-bootstrap                 # loopback-only, no auth → host + permanent agent_token   (1:1 co-located)
GET    /api/hosts                                 # list (workspace-scoped)
GET    /api/hosts/:id                             # detail (advertised + enabled skills)
PUT    /api/hosts/:id                             # rename, slug
DELETE /api/hosts/:id                             # revokes agent_token + cascades

# Skills (per-host)
GET    /api/hosts/:id/skills                      # list enabled skills + their runs
POST   /api/hosts/:id/skills                      # enable a skill (body: kind + name + config)
PUT    /api/hosts/:id/skills/:skill_id            # update config / enabled
DELETE /api/hosts/:id/skills/:skill_id

# Skill runs (read + control)
GET    /api/hosts/:id/skills/:skill_id/runs       # last N runs
POST   /api/hosts/:id/skills/:skill_id/start      # explicit start (skill must be enabled)
POST   /api/hosts/:id/skills/:skill_id/stop       # explicit stop

# WS (P0 reuse + P2 add)
/ws/agent                                         # existing — extended with skill.* message types
/ws/host/:id/skill/:run_id/stream                 # P2+ — for skill-specific bidi (shell/kdp)
```

All under `RequireWorkspace` middleware; mutations require
`TeamRoleAdmin`.

## How to add a skill (reference doc)

Once P0 lands, adding a new skill kind is:

1. New file `cmd/polar-agent/skills/<kind>.go` implementing `Skill`
   interface. Register it in `init()` via `skills.Register(...)`.
2. New form widget `ui/src/hosts/skill-forms/<kind>.ts` exporting a
   default object `{label, fields[], validate(config), summarize(config)}`.
3. (If skill has a live view, like shell or kdp:) new
   `skill-views/<kind>.ts` exporting a `mount(el, runID)` function.
4. Add an entry to `KNOWN_SKILL_KINDS` in `ui/src/hosts/types.ts`.
5. Phase docs under `doc/host-skill-<kind>.md`.
6. Tests: spawn / config-validate / Stop-cleanup.

No dock-side changes. No schema changes. No new HTTP routes.

## Local dev story

Two patterns, matching the registration table above.

### Pattern A — co-located bootstrap (recommended for single-box dev)

```bash
# Build dock + agent
go build -o bin/polar-dock ./cmd/dock
go build -o bin/polar-agent ./cmd/polar-agent

# Run dock with the bootstrap endpoint enabled
POSTGRES_DSN="postgres://ideamesh:test123456@localhost:5432/ideamesh?sslmode=disable" \
  METRICS_TOKEN=devtoken \
  POLAR_ALLOW_LOCAL_BOOTSTRAP=true \
  ./bin/polar-dock

# One command — no token paste:
./bin/polar-agent register --local --name=$(hostname)
./bin/polar-agent attach --workdir=.
```

`register --local` writes `agent.toml` with `server=http://127.0.0.1:8080` so the subsequent attach connects directly to dock loopback (skips nginx + TLS for the local round-trip).

### Pattern B — remote / multi-machine

```bash
# 1. Admin (on the dock box, via web UI):
#    open https://central-dock/hosts.html → "Add Host" → copy one-time token

# 2. Operator (on the target host machine):
./bin/polar-agent register --server=https://central-dock --token=<one-time-enroll-token>
./bin/polar-agent attach --workdir=.
```

Same `attach` step in both; the difference is purely the bootstrap.

### Smoke after register

```bash
./bin/polar-agent self-test        # whoami + WS handshake + skill.advertise round-trip
```

After P1c.3, the legacy `attach --bot --tool` still works for backward
compat. After P6, only the new flow remains.

## Risk register

| Risk | Mitigation |
|---|---|
| Skill registry interface churns during P1–P5 | Lock interface at end of P0 with formal review; new method additions only |
| Multi-tool subprocess management on a single host gets complex | Each skill_run is fully independent; no inter-skill IPC in P0; defer to Phase 7 |
| UI complexity explodes with N skill forms | Each form is small (≤ 100 LOC); routing dispatches by kind; no global state |
| WireGuard sudo requirement bites operators | Surface clearly in install-script output + UI tooltip on P4 enable button |
| Agent version skew (new dock, old agent that doesn't speak `skill.advertise`) | Dock treats missing advertise as "no skills available, you have one (default `coder`)". Operator nag-banner: "your polar-agent is X versions old". |

## Cadence estimate

- P0: 4–5 days incl tests
- P1: 2–3 days
- P2–P5: 2 days each
- P6: 1 day

Single-developer linear: ~3 weeks. With parallelization (P2+P3+P4
once P0 lands), shrinks to ~2 weeks.
