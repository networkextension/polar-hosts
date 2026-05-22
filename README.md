# polar-hosts

Host inventory + shell + VNC plugin for the [Polar](https://github.com/networkextension/Polar) platform.

Owns: `hosts`, `host_skills`, `host_skill_runs`, `host_skill_credentials`, `console_layouts`. AES-GCM vault for skill credential env vars.

`agent_tokens` + the polar-agent WebSocket hub stay in dock — this plugin reaches into them via the SDK and via stubs in `internal/hosts/stubs.go`. See WIP-NOTES.md (if present) for what's still wired through dock.

## Status

W3 handler migration combined with extraction at 2026-05-22. Hosts data is workspace-scoped (each team's machines); cross-domain user lookups go through dock SDK.

**WS proxy is stubbed**: shell + VNC dispatch (`agentHub.dispatchSkillStart/Stop/Stdin/Resize`) logs a TODO and returns ErrNotWired until dock exposes a `/internal/v1/agent/skill-dispatch` SDK surface. The supervisor + handler scaffolding compiles + runs; open / kick / sessions REST work, but the live PTY/VNC channels short-circuit. Follow-up PR wires the WS path.

`createAgentToken` (used by `/api/hosts/local-bootstrap`) is also stubbed — same reason (agent_tokens lives in dock).

## Install

```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/hosts-svc ./cmd/hosts-svc
rsync -avz /tmp/hosts-svc local@<deploy-box>:/Users/local/.local/bin/
```

## Environment

- `POLAR_DOCK_BASE` — http://127.0.0.1:8080
- `POLAR_PLUGIN_TOKEN` — plaintext from /admin-plugins.html
- `POLAR_HOSTS_DB_DSN` — Postgres for `polar_hosts`
- `POLAR_HOSTS_LISTEN` — default `127.0.0.1:8095`
- `POLAR_HOSTS_BLOB_DIR` — local-bootstrap install scripts + skill logs
- `POLAR_HOSTS_METRICS_TOKEN` — bearer for `/metrics`
- `HOSTS_SKILL_KEY` — 64 hex chars (32 bytes) AES-256-GCM key for `host_skill_credentials`. Unset = plaintext storage (UI badge: "encryption inactive").
- `POLAR_ALLOW_LOCAL_BOOTSTRAP` — `1` to enable `POST /api/hosts/local-bootstrap` (anon).
- `POLAR_DISABLE_SHELL_SKILL` / `POLAR_DISABLE_VNC_SKILL` — `1` to 503 the open endpoints.

## Endpoints

- `POST /api/hosts/enroll`, `POST /api/hosts/register`, `POST /api/hosts/local-bootstrap`
- `GET|DELETE /api/hosts[/:id]`
- `GET /api/host-skills`, `GET /api/hosts/:id/skills`, `PATCH /api/hosts/:id/skills/:skillId`
- `GET|PUT|DELETE /api/hosts/:id/skills/:skillId/credentials[/:key]`
- `POST /api/hosts/:id/skills/:skillId/shell/open`, sessions list/kick (`/api/hosts/:id/skills/shell/sessions`)
- `POST /api/hosts/:id/skills/:skillId/vnc/open`, sessions list/kick
- `GET|POST|PATCH|DELETE /api/console/layouts[/:layoutId]`

## Related

- [Polar dock](https://github.com/networkextension/Polar)
- [polar-sdk](https://github.com/networkextension/polar-sdk)
- [polar-packtunnel](https://github.com/networkextension/polar-packtunnel) — reference pattern

## License

MIT
