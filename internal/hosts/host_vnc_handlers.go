package hosts

// Host module — VNC skill dock-side surface (B.1 plain TCP relay).
//
//   POST /api/hosts/:id/skills/:skillId/vnc/open
//     Body: {target?, idle_timeout_sec?}
//     Auth: workspace admin
//     Returns: {run_id, ws_url, host_id, target, opened_at}
//     Side effects: reserve a slot in vncSessions registry, INSERT a
//     host_skill_runs row with status='starting' + meta_json, dispatch
//     skill.start to the agent. Browser opens ws_url next.
//
//   GET  /ws/host/:id/vnc/:run_id
//     Auth: session cookie + workspace admin (verified inline because
//     gin middleware can't run cleanly across an upgrade).
//     Behavior: binary byte bridge between browser noVNC and the
//     agent's TCP relay to the VNC server. See handleHostVNCWS.
//
//   GET  /api/hosts/:id/skills/vnc/sessions
//     Returns: {sessions: [{run_id, operator_user_id, opened_at, …}]}
//
//   DELETE /api/hosts/:id/skills/vnc/sessions/:run_id
//     Forcibly closes a session (kicks). Admin-only.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var hostVNCUpgrader = websocket.Upgrader{
	// VNC frames can be large (full framebuffer updates); bigger
	// buffers reduce write-syscall count on busy screens.
	ReadBufferSize:  128 << 10,
	WriteBufferSize: 128 << 10,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

const (
	hostVNCPingInterval = 20 * time.Second
	hostVNCWriteTimeout = 10 * time.Second
)

func (p *Plugin) handleHostVNCOpen(c *gin.Context) {
	if p.polarDisableVNCSkill {
		c.JSON(http.StatusForbidden, gin.H{"error": "vnc skill disabled on this dock"})
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

	// VNC differs from shell in its native UX: macOS Screen Sharing
	// supports multiple concurrent viewers of the same desktop, each
	// with its own RFB session. Apple's native client doesn't kick the
	// previous viewer when you open a second one — both see the same
	// desktop side by side. So /vnc/open does NOT dedup by (host,
	// skill, operator); every call mints a fresh run + fresh agent
	// dial to :5900. Per-host cap (2) bounds the parallelism.
	//
	// Refresh-reattach for THE SAME run still works: the UI tracks
	// each pane's run_id and connects WS directly via /ws/host/:id/
	// vnc/:run_id, bypassing /open. /open is only fresh-start clicks.

	var req struct {
		Target         string `json:"target"`
		IdleTimeoutSec int    `json:"idle_timeout_sec"`
	}
	_ = c.ShouldBindJSON(&req)

	if err := p.vncSessions.reserve(hostID); err != nil {
		switch {
		case errors.Is(err, errVNCPerHostCapReached):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "host vnc limit reached (2/2)"})
		case errors.Is(err, errVNCGlobalCapReached):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "global vnc limit reached"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	conn := p.agentHub.lookupByHostID(hostID)
	if conn == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "no polar-agent currently connected for this host",
		})
		return
	}

	now := time.Now().UTC()
	target := strings.TrimSpace(req.Target)
	meta := map[string]any{
		"operator_user_id": operatorUserID,
		"kind":             "vnc",
	}
	if target != "" {
		meta["target"] = target
	}
	runID, err := p.createHostSkillRunWithMeta(skillID, meta, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create run: " + err.Error()})
		return
	}

	startCfg := map[string]any{}
	if target != "" {
		startCfg["target"] = target
	}
	if req.IdleTimeoutSec > 0 {
		startCfg["idle_timeout_sec"] = req.IdleTimeoutSec
	}

	// Resolve the host_skill row's kind. Caller bound a vnc skill — the
	// info.Kind from the DB is the source of truth for the dispatch
	// kind sent to the agent.
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

	// Track channels BEFORE dispatch — same race fix as shell. Agent
	// dials VNC server + may emit RFB version banner bytes immediately.
	eventCh, stdoutCh := p.trackVNCChannels(conn, runID)
	log.Printf("[vnc-h] /vnc/open run=%d host=%s skill=%d target=%q — channels tracked + dispatching", runID, hostID, skillID, target)

	if err := p.agentHub.dispatchSkillStart(conn, env); err != nil {
		conn.untrackSkillPending(runID)
		conn.untrackSkillStdout(runID)
		_ = p.finalizeHostSkillRun(runID, "failed", "dispatch: "+err.Error(), time.Now().UTC())
		c.JSON(http.StatusBadGateway, gin.H{"error": "dispatch skill.start: " + err.Error()})
		return
	}

	sess := &vncSession{
		RunID:          runID,
		HostID:         hostID,
		HostSkillID:    skillID,
		OperatorUserID: operatorUserID,
		OpenedAt:       now,
		Target:         target,
		handshakeBuf:   newByteRing(vncHandshakeBufferBytes),
	}
	if err := p.vncSessions.insert(sess); err != nil {
		_ = p.agentHub.dispatchSkillStop(conn, runID, "registry_full")
		conn.untrackSkillPending(runID)
		conn.untrackSkillStdout(runID)
		_ = p.finalizeHostSkillRun(runID, "failed", "registry: "+err.Error(), time.Now().UTC())
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		return
	}
	first := p.startVNCSupervisor(conn, runID, eventCh, stdoutCh)
	select {
	case sig := <-first:
		if sig.failed {
			log.Printf("[vnc-h] /vnc/open run=%d FAILED early: %s", runID, sig.reason)
			c.JSON(http.StatusBadGateway, gin.H{"error": "vnc start failed: " + sig.reason})
			return
		}
		log.Printf("[vnc-h] /vnc/open run=%d agent confirmed start", runID)
	case <-time.After(3000 * time.Millisecond):
		log.Printf("[vnc-h] /vnc/open run=%d first-signal timeout (3s) — returning anyway, WS connect will surface any error", runID)
	}

	wsURL := p.buildHostWSURL(c, "vnc", hostID, runID)
	log.Printf("[vnc-h] /vnc/open run=%d returning 201 ws_url=%s", runID, wsURL)
	c.JSON(http.StatusCreated, gin.H{
		"run_id":    runID,
		"ws_url":    wsURL,
		"host_id":   hostID,
		"target":    target,
		"opened_at": now,
	})
}

// handleHostVNCWS is the byte bridge between the browser's noVNC and
// the agent's TCP relay to the VNC server. Identical shape to the
// shell bridge with one diff: VNC is pure binary in both directions
// (no resize/control text frames), so we ignore text-frame payloads
// other than logging.
func (p *Plugin) handleHostVNCWS(c *gin.Context) {
	if p.polarDisableVNCSkill {
		c.JSON(http.StatusForbidden, gin.H{"error": "vnc skill disabled on this dock"})
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
	agentConnPtr := p.agentHub.lookupByHostID(hostID)
	if agentConnPtr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent not connected"})
		return
	}
	if _, ok := p.vncSessions.get(runID); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "vnc session not found (call /vnc/open first)"})
		return
	}

	wsConn, err := hostVNCUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer wsConn.Close()

	bridgeCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	bridge := &vncBridge{
		cancel:     cancel,
		eventSink:  make(chan skillEventFrame, 16),
		stdoutSink: make(chan []byte, 512),
	}
	prev, handshakeSnap := p.vncSessions.attachBridge(runID, bridge)
	log.Printf("[vnc-ws] run=%d bridge attached (evicted_prev=%v, replay_snapshot=%d bytes)", runID, prev != nil, len(handshakeSnap))
	if prev != nil && prev.cancel != nil {
		prev.cancel()
	}
	// Byte/frame counters for diagnostic exit log. browser→agent and
	// agent→browser tracked separately so we can see which direction
	// is silent when a session looks "stuck".
	var bytesIn, framesIn, bytesOut, framesOut int64
	defer func() {
		log.Printf("[vnc-ws] run=%d bridge handler EXIT — browser→agent: %d frames / %d bytes, agent→browser: %d frames / %d bytes",
			runID, framesIn, bytesIn, framesOut, bytesOut)
	}()
	defer p.vncSessions.detachBridge(runID, bridge)

	// Replay the pre-bridge handshake snapshot BEFORE pumping live bytes.
	// Pre-attach the agent may have already received the RFB version banner
	// + security negotiation. Without this replay the browser noVNC sits
	// waiting forever for a server greeting it'll never see (because
	// macOS Screen Sharing already sent it into the void).
	if len(handshakeSnap) > 0 {
		select {
		case bridge.stdoutSink <- handshakeSnap:
		default:
			// stdoutSink buffer is 512 chunks; an unread sink fitting
			// a single snap is theoretically guaranteed.
		}
	}

	var writeMu sync.Mutex
	writeBinary := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = wsConn.SetWriteDeadline(time.Now().Add(hostVNCWriteTimeout))
		return wsConn.WriteMessage(websocket.BinaryMessage, b)
	}
	writePing := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = wsConn.SetWriteDeadline(time.Now().Add(hostVNCWriteTimeout))
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
			if msgType != websocket.BinaryMessage {
				// VNC has no text/control frames in this bridge — drop.
				continue
			}
			framesIn++
			bytesIn += int64(len(payload))
			if dispatchErr := p.agentHub.dispatchSkillStdin(agentConnPtr, runID, payload); dispatchErr != nil {
				log.Printf("[vnc-ws] run=%d browser→agent dispatch err after %d frames: %v", runID, framesIn, dispatchErr)
				return
			}
		}
	}()

	pingTicker := time.NewTicker(hostVNCPingInterval)
	defer pingTicker.Stop()
	for {
		select {
		case chunk, ok := <-bridge.stdoutSink:
			if !ok {
				log.Printf("[vnc-ws] run=%d stdoutSink closed", runID)
				return
			}
			framesOut++
			bytesOut += int64(len(chunk))
			if framesOut <= 3 || framesOut%50 == 0 {
				log.Printf("[vnc-ws] run=%d agent→browser frame#%d size=%d total=%d", runID, framesOut, len(chunk), bytesOut)
			}
			if err := writeBinary(chunk); err != nil {
				log.Printf("[vnc-ws] run=%d writeBinary err after %d frames: %v", runID, framesOut, err)
				return
			}
		case ev, ok := <-bridge.eventSink:
			if !ok {
				return
			}
			if ev.EventKind == "exit" {
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
		case <-agentConnPtr.close:
			writeMu.Lock()
			_ = wsConn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "agent disconnected"),
				time.Now().Add(time.Second))
			writeMu.Unlock()
			return
		}
	}
}

func (p *Plugin) handleHostVNCSessions(c *gin.Context) {
	if p.polarDisableVNCSkill {
		c.JSON(http.StatusForbidden, gin.H{"error": "vnc skill disabled on this dock"})
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
	c.JSON(http.StatusOK, gin.H{"sessions": p.vncSessions.listByHost(hostID)})
}

func (p *Plugin) handleHostVNCKick(c *gin.Context) {
	if p.polarDisableVNCSkill {
		c.JSON(http.StatusForbidden, gin.H{"error": "vnc skill disabled on this dock"})
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
	if _, ok := p.vncSessions.get(runID); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": errVNCSessionNotFound.Error()})
		return
	}
	_ = p.vncSessions.kick(runID)
	if conn := p.agentHub.lookupByHostID(hostID); conn != nil {
		_ = p.agentHub.dispatchSkillStop(conn, runID, "operator_kick")
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// buildHostWSURL constructs the ws[s] URL the browser should connect
// to for a given host skill kind + run_id. Handles X-Forwarded-Host
// + X-Forwarded-Proto + raw c.Request.Host fallback so non-default
// ports (:2443, :8888) round-trip into the URL — defaulting to bare
// hostname would send the browser to :443/:80 and fail closed.
//
// Lives here (rather than host_shell_handlers) because the helper is
// shared and the shell handler will be refactored to use it in a
// follow-up; for now both paths inline the same logic.
func (p *Plugin) buildHostWSURL(c *gin.Context, kind, hostID string, runID int64) string {
	scheme := "ws"
	if strings.HasPrefix(strings.ToLower(c.GetHeader("X-Forwarded-Proto")), "https") || c.Request.TLS != nil {
		scheme = "wss"
	}
	wsHost := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if wsHost == "" {
		wsHost = c.Request.Host
	}
	return scheme + "://" + wsHost + "/ws/host/" + hostID + "/" + kind + "/" + strconv.FormatInt(runID, 10)
}
