# polar-hosts — extraction WIP notes

Status at first commit (2026-05-22): **build green**, tests pass, but the WebSocket dispatch path is stubbed. See `internal/hosts/stubs.go` for the precise shape.

## Deferred to follow-up PRs

1. **WS skill dispatch** (`agentHub.dispatchSkill{Start,Stop,Stdin,Resize}`)
   - polar-agent attaches to dock's `/api/agent/attach` (lives in `dock/agent_handlers.go` + `dock/agent_hub.go`). Those files stayed in dock.
   - Extracted shell/VNC handlers call `p.agentHub.lookupByHostID(hostID)` to find the live `*agentConn` then `dispatchSkillStart` to send the frame. Stub returns nil/ErrNotWired.
   - Real fix: add SDK surface `/internal/v1/agent/skill-dispatch?host_id=...` that dock proxies onto the WS for that host, OR proxy raw frames via a long-poll bridge.
   - Search marker: `TODO(extract)` in `internal/hosts/stubs.go`.

2. **`createAgentToken`** — used by `/api/hosts/local-bootstrap` to issue an agent token to a brand-new machine. `agent_tokens` table stays in dock; SDK has no AgentTokenCreate surface yet.
   - Stub returns ErrNotWired so the bootstrap endpoint 500s cleanly.

3. **Tests that touched the WS path** — all WS-path tests stayed in dock (e.g. `agent_hub_test.go`); none of the 18 moved files referenced agentConn at test time, so the test suite here is green without rework.

## Other notes

- Backscroll test (`host_shell_backscroll_test.go`) is pure stdlib, passes.
- `host_skill_credentials_store_test.go` was rewritten to use `&Plugin{}` instead of `&Server{}`; behavior identical.
- `iosdistResourceKeyBytes = 32` was copied into `stubs.go` (dock has it in `iosdist_crypto.go` which stays in dock).
- Receiver rewrite was done with a scope-aware Python pass (`/tmp/rewrite_hosts.py`) so loop variables named `s` were preserved (`for i, s := range ...` etc).
- `agent_handlers.go`, `agent_hub.go`, `agent_install_script.go`, `agent_store.go`, `agent_tokens.go` STAY in `dock/` — the cutover PR keeps those + adds the SDK surfaces above.
