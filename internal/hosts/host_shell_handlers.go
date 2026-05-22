package hosts

// Host module Phase 2 — Shell skill dock-side surface.
//
//   POST /api/hosts/:id/skills/shell/open
//     Body: {host_skill_id, rows?, cols?, cwd?, env?, idle_timeout_sec?}
//     Auth: workspace admin
//     Returns: {run_id, ws_url, host_id, opened_at}
//     Side effects: reserve a slot in shellSessions registry, INSERT
//     a host_skill_runs row with status='starting' + meta_json with
//     operator_user_id, dispatch skill.start to the agent.
//     The browser then opens the WS at ws_url to claim the slot.
//     If the browser never connects within 10s the registry's reserve
//     is implicit-released by the run timing out on the agent side
//     (state→exit reason=eof when no stdin happens) — sufficient for v1.
//
//   GET  /ws/host/:id/shell/:run_id
//     Auth: session cookie + workspace admin (verified inline because
//     gin middleware can't run cleanly across an upgrade).
//     Behavior: byte bridge — see handleHostShellWS.
//
//   GET  /api/hosts/:id/skills/shell/sessions
//     Returns: {sessions: [{run_id, operator_user_id, opened_at, ...}]}
//
//   DELETE /api/hosts/:id/skills/shell/sessions/:run_id
//     Forcibly closes a session (kicks). Admin-only.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var hostShellUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 << 10,
	WriteBufferSize: 64 << 10,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

const (
	hostShellPingInterval = 20 * time.Second
	hostShellWriteTimeout = 10 * time.Second
)

// handleHostShellOpen mints a host_skill_run + dispatches skill.start
// to the agent, returning the WS URL the browser should connect to.
// Reserves the concurrency slot before any DB work so a saturated
// host fails fast.
func (p *Plugin) handleHostShellOpen(c *gin.Context) {
	if p.polarDisableShellSkill {
		c.JSON(http.StatusForbidden, gin.H{"error": "shell skill disabled on this dock"})
		return
	}
	skillID, ok := p.resolveHostSkillAdmin(c)
	if !ok {
		return
	}
	operatorUserID, ok := requireUserID(c)
	if !ok {
		return
	}
	hostID := strings.TrimSpace(c.Param("id"))

	// Each /shell/open mints a fresh PTY. Multi-pane = multi-terminal:
	// pane A runs `top`, B runs `xcodebuild`, C runs `df`, D runs
	// `macmon` — independent processes, independent buffers, no
	// shared keystrokes. tmux WINDOW semantics, not tmux-attach.
	//
	// Refresh / saved-layout reattach is handled UI-side: the React
	// console queries listHostShellSessions on bootstrap, gets a list
	// of live (run_id, host_skill_id), creates panes pre-bound to
	// those run_ids, and the Pane component skips /shell/open and
	// connects WS directly to /ws/host/:id/shell/:run_id. No dedup
	// needed at this endpoint.

	var req struct {
		Rows           uint16            `json:"rows"`
		Cols           uint16            `json:"cols"`
		Cwd            string            `json:"cwd"`
		Env            map[string]string `json:"env"`
		IdleTimeoutSec int               `json:"idle_timeout_sec"`
		// Shell picker: "bash" / "zsh" / "sh". Empty / unknown → agent
		// auto-picks. Whitelisted server-side too so an attacker
		// can't smuggle an arbitrary binary path into the env.
		Shell string `json:"shell"`
		// P5 (KDP): when the bound skill kind is kdp, these
		// supersede the shell-specific fields above.
		DevicePath string `json:"device_path"`
		Baud       int    `json:"baud"`
	}
	_ = c.ShouldBindJSON(&req) // body is optional; agent fills defaults

	// Fail-fast concurrency check before DB.
	if err := p.shellSessions.reserve(hostID); err != nil {
		switch {
		case errors.Is(err, errShellPerHostCapReached):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "host shell limit reached (4/4)"})
		case errors.Is(err, errShellGlobalCapReached):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "global shell limit reached"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	// Find the live agentConn for the host so we can dispatch.
	conn := p.agentHub.lookupByHostID(hostID)
	if conn == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "no polar-agent currently connected for this host",
		})
		return
	}

	now := time.Now().UTC()
	meta := map[string]any{
		"operator_user_id": operatorUserID,
		"kind":             "shell",
	}
	if strings.TrimSpace(req.Cwd) != "" {
		meta["initial_cwd"] = req.Cwd
	}
	runID, err := p.createHostSkillRunWithMeta(skillID, meta, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create run: " + err.Error()})
		return
	}

	// Build skill.start config blob. Agent's shell.go / kdp.go
	// fill sane defaults for any omitted field. The dispatch kind
	// comes from the host_skills row (not hardcoded) so KDP +
	// future byte-streaming skills route through this same endpoint.
	startCfg := map[string]any{}
	if req.Rows > 0 {
		startCfg["rows"] = req.Rows
	}
	if req.Cols > 0 {
		startCfg["cols"] = req.Cols
	}
	if req.Cwd != "" {
		startCfg["cwd"] = req.Cwd
	}
	if len(req.Env) > 0 {
		startCfg["env"] = req.Env
	}
	if req.IdleTimeoutSec > 0 {
		startCfg["idle_timeout_sec"] = req.IdleTimeoutSec
	}
	if pref := strings.ToLower(strings.TrimSpace(req.Shell)); pref == "bash" || pref == "zsh" || pref == "sh" {
		startCfg["shell"] = pref
	}
	// P5: pass through device_path + baud when the bound skill is
	// kdp. The req struct above doesn't yet have these fields;
	// see the next handler revision.
	if strings.TrimSpace(req.DevicePath) != "" {
		startCfg["device_path"] = req.DevicePath
	}
	if req.Baud > 0 {
		startCfg["baud"] = req.Baud
	}

	// Resolve the host_skill row's kind so we dispatch as whatever
	// the operator bound (shell / kdp / future byte-streaming).
	info, err := p.getHostSkillForDispatch(skillID)
	if err != nil || info == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "resolve host_skill kind"})
		return
	}

	env := skillStartEnvelope{
		Kind:      "skill.start",
		RunID:     runID,
		SkillKind: info.Kind,
		Config:    startCfg,
	}

	// CRITICAL: track the event + stdout channels BEFORE dispatching
	// skill.start. The agent starts bash and writes the prompt to PTY
	// within microseconds of receiving the dispatch; if the channels
	// aren't tracked yet, deliverSkillStdout silently drops those
	// bytes and the browser sees an empty WS forever.
	eventCh, stdoutCh := p.trackShellChannels(conn, runID)

	if err := p.agentHub.dispatchSkillStart(conn, env); err != nil {
		conn.untrackSkillPending(runID)
		conn.untrackSkillStdout(runID)
		_ = p.finalizeHostSkillRun(runID, "failed", "dispatch: "+err.Error(), time.Now().UTC())
		c.JSON(http.StatusBadGateway, gin.H{"error": "dispatch skill.start: " + err.Error()})
		return
	}

	// Insert the session record + spawn the supervisor BEFORE returning.
	// The supervisor owns the agent event channel for the run's lifetime
	// and survives any browser disconnect (refresh, tab close), so the
	// PTY stays alive until the agent reports exit / the operator kicks
	// the run / the agent disconnects.
	sess := &shellSession{
		RunID:          runID,
		HostID:         hostID,
		HostSkillID:    skillID,
		OperatorUserID: operatorUserID,
		OpenedAt:       now,
		backscroll:     newByteRing(shellBackscrollBytes),
	}
	if err := p.shellSessions.insert(sess); err != nil {
		_ = p.agentHub.dispatchSkillStop(conn, runID, "registry_full")
		conn.untrackSkillPending(runID)
		conn.untrackSkillStdout(runID)
		_ = p.finalizeHostSkillRun(runID, "failed", "registry: "+err.Error(), time.Now().UTC())
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		return
	}
	first := p.startShellSupervisor(conn, runID, eventCh, stdoutCh)
	// Wait briefly for the agent to confirm the run started (or fail
	// fast). Avoids the race where the browser's WS upgrade arrives
	// AFTER the supervisor has already removed the session because the
	// agent rejected the dispatch (e.g. kdp without device_path).
	// 1500ms covers loopback + pty.Start + first byte; longer pty
	// startups would race regardless and fall back to a 404 on WS.
	select {
	case sig := <-first:
		if sig.failed {
			c.JSON(http.StatusBadGateway, gin.H{"error": "skill start failed: " + sig.reason})
			return
		}
	case <-time.After(1500 * time.Millisecond):
		// Best effort — assume success; if the agent never started,
		// the WS connect will surface the failure.
	}

	scheme := "ws"
	if strings.HasPrefix(strings.ToLower(c.GetHeader("X-Forwarded-Proto")), "https") || c.Request.TLS != nil {
		scheme = "wss"
	}
	// Prefer X-Forwarded-Host when present — it preserves the port the
	// browser sees verbatim. nginx's $host variable strips the port (so
	// a non-default-port deploy like :8888 would otherwise round-trip
	// as just the bare hostname, sending the browser to port 80 and
	// closing with WS code 1006).
	wsHost := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if wsHost == "" {
		wsHost = c.Request.Host
	}
	wsURL := scheme + "://" + wsHost + "/ws/host/" + hostID + "/shell/" + strconv.FormatInt(runID, 10)

	c.JSON(http.StatusCreated, gin.H{
		"run_id":     runID,
		"ws_url":     wsURL,
		"host_id":    hostID,
		"opened_at":  now,
	})
}

// handleHostShellWS is the byte bridge — browser ↔ dock ↔ agent.
//
//   browser BINARY frame   → skill.stdin to agent (base64 wrap)
//   browser TEXT  frame    → JSON control: {kind:"resize",rows,cols}
//   agent  skill.event stdout → BINARY frame to browser (already raw bytes)
//   either side closes     → cancel ctx, the other side observes, defer
//                            removes registry entry + sends skill.stop
//                            to agent for explicit pty teardown
func (p *Plugin) handleHostShellWS(c *gin.Context) {
	if p.polarDisableShellSkill {
		c.JSON(http.StatusForbidden, gin.H{"error": "shell skill disabled on this dock"})
		return
	}
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	if !teamRoleAllows(workspaceRole(c), TeamRoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要管理员权限"})
		return
	}
	hostID := strings.TrimSpace(c.Param("id"))
	runIDRaw := strings.TrimSpace(c.Param("run_id"))
	runID, err := strconv.ParseInt(runIDRaw, 10, 64)
	if err != nil || runID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run_id"})
		return
	}
	host, err := p.getHostByID(hostID)
	if err != nil || host == nil || host.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	conn := p.agentHub.lookupByHostID(hostID)
	if conn == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent not connected"})
		return
	}
	operatorUserID, ok := requireUserID(c)
	if !ok {
		return
	}

	// Verify the session was registered by /shell/open. Direct WS
	// without prior /open is rejected — /open is the only place that
	// dispatches skill.start to the agent.
	if _, ok := p.shellSessions.get(runID); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "shell session not found (call /shell/open first)"})
		return
	}
	_ = operatorUserID

	wsConn, err := hostShellUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer wsConn.Close()

	bridgeCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	// Bridge slot: the supervisor owns the agent channels and forwards
	// stdout + events to our local sinks. If a previous browser is
	// attached, evict it (last writer wins — refresh-then-reconnect
	// kicks the stale tab gracefully).
	bridge := &shellBridge{
		cancel:     cancel,
		eventSink:  make(chan skillEventFrame, 16),
		stdoutSink: make(chan []byte, 256),
	}
	if prev := p.shellSessions.attachBridge(runID, bridge); prev != nil && prev.cancel != nil {
		prev.cancel()
	}
	defer p.shellSessions.detachBridge(runID, bridge)

	// Replay backscroll FIRST so the operator sees the recent screen
	// state before live stdout starts streaming. Without this, refresh
	// lands on a blank xterm until the agent's next byte. Lookup is
	// fresh so we get the up-to-date snapshot post-attach.
	if sess, _ := p.shellSessions.get(runID); sess != nil && sess.backscroll != nil {
		if snap := sess.backscroll.snapshot(); len(snap) > 0 {
			select {
			case bridge.stdoutSink <- snap:
			default:
				// stdoutSink is buffered 256 chunks; this case is
				// theoretically unreachable for an unread sink but
				// the select is cheap insurance.
			}
		}
	}

	// Bridge close does NOT send skill.stop and does NOT finalize the
	// run — both are the supervisor's job (on terminal event) or
	// handleHostShellKick's (explicit kick). This is what makes the
	// session survive browser refresh.

	// Write mutex — multiple goroutines (bytes pump + ping ticker)
	// share the WS conn; gorilla forbids concurrent writes.
	var writeMu sync.Mutex
	writeBinary := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = wsConn.SetWriteDeadline(time.Now().Add(hostShellWriteTimeout))
		return wsConn.WriteMessage(websocket.BinaryMessage, b)
	}
	writePing := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = wsConn.SetWriteDeadline(time.Now().Add(hostShellWriteTimeout))
		return wsConn.WriteMessage(websocket.PingMessage, nil)
	}

	// Browser → agent goroutine.
	go func() {
		defer cancel()
		for {
			if bridgeCtx.Err() != nil {
				return
			}
			msgType, payload, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			switch msgType {
			case websocket.BinaryMessage:
				if dispatchErr := p.agentHub.dispatchSkillStdin(conn, runID, payload); dispatchErr != nil {
					return
				}
			case websocket.TextMessage:
				var ctl struct {
					Kind string `json:"kind"`
					Rows uint16 `json:"rows"`
					Cols uint16 `json:"cols"`
				}
				if json.Unmarshal(payload, &ctl) != nil {
					continue
				}
				switch ctl.Kind {
				case "resize":
					_ = p.agentHub.dispatchSkillResize(conn, runID, ctl.Rows, ctl.Cols)
				case "ping":
					// Browser-side keepalive; nothing to do.
				}
			}
		}
	}()

	// Agent → browser loop (main goroutine).
	pingTicker := time.NewTicker(hostShellPingInterval)
	defer pingTicker.Stop()
	for {
		select {
		case chunk, ok := <-bridge.stdoutSink:
			if !ok {
				return
			}
			if err := writeBinary(chunk); err != nil {
				return
			}
		case ev, ok := <-bridge.eventSink:
			if !ok {
				return
			}
			if ev.EventKind == "exit" {
				// Supervisor will finalize + remove; just close the WS
				// politely with a hint for the operator.
				reason, _ := ev.Data["reason"].(string)
				code := websocket.CloseNormalClosure
				if reason == "agent_disconnect" {
					code = websocket.CloseGoingAway
				}
				writeMu.Lock()
				_ = wsConn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(code, "session ended: "+reason),
					time.Now().Add(time.Second))
				writeMu.Unlock()
				return
			}
		case <-pingTicker.C:
			if err := writePing(); err != nil {
				return
			}
		case <-bridgeCtx.Done():
			return
		case <-conn.close:
			writeMu.Lock()
			_ = wsConn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "agent disconnected"),
				time.Now().Add(time.Second))
			writeMu.Unlock()
			return
		}
	}
}

func (p *Plugin) handleHostShellSessions(c *gin.Context) {
	if p.polarDisableShellSkill {
		c.JSON(http.StatusForbidden, gin.H{"error": "shell skill disabled on this dock"})
		return
	}
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	if !teamRoleAllows(workspaceRole(c), TeamRoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要管理员权限"})
		return
	}
	hostID := strings.TrimSpace(c.Param("id"))
	host, err := p.getHostByID(hostID)
	if err != nil || host == nil || host.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": p.shellSessions.listByHost(hostID)})
}

func (p *Plugin) handleHostShellKick(c *gin.Context) {
	if p.polarDisableShellSkill {
		c.JSON(http.StatusForbidden, gin.H{"error": "shell skill disabled on this dock"})
		return
	}
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	if !teamRoleAllows(workspaceRole(c), TeamRoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要管理员权限"})
		return
	}
	hostID := strings.TrimSpace(c.Param("id"))
	host, err := p.getHostByID(hostID)
	if err != nil || host == nil || host.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	runID, err := strconv.ParseInt(strings.TrimSpace(c.Param("run_id")), 10, 64)
	if err != nil || runID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run_id"})
		return
	}
	if _, ok := p.shellSessions.get(runID); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": errShellSessionNotFound.Error()})
		return
	}
	// Sever the dock-side bridge first so the browser closes promptly.
	_ = p.shellSessions.kick(runID)
	// Tell the agent to tear down the PTY. The supervisor will pick
	// up the resulting EventExit and finalize the DB run + remove
	// from the registry.
	if conn := p.agentHub.lookupByHostID(hostID); conn != nil {
		_ = p.agentHub.dispatchSkillStop(conn, runID, "operator_kick")
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
