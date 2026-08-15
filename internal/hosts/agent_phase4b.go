package hosts

// agent_phase4b.go — bits dock used to do on its own /ws/agent read pump
// that must live here once nginx routes /ws/agent to hosts-svc:
//   • wg_peer_status metric → dock POST /internal/v1/wg-peer-status
//   • POST /internal/v1/agents/kick (loopback) so dock's admin kick works.

import (
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// forwardWGPeerStatusToDock pushes one wireguard peer-status sample into
// dock's wgPeerCache. dock's push handler synthesises host_id from the
// plugin name when empty, so we always send ours.
func (p *Plugin) forwardWGPeerStatusToDock(hostID string, data map[string]any) {
	if p.Dock == nil || data == nil {
		return
	}
	iface, _ := data["iface"].(string)
	pub, _ := data["iface_public_key"].(string)
	if strings.TrimSpace(iface) == "" || strings.TrimSpace(pub) == "" {
		return
	}
	body := map[string]any{
		"host_id":          hostID,
		"iface":            iface,
		"iface_public_key": pub,
		"data":             data,
	}
	resp, err := p.Dock.Do(http.MethodPost, "/internal/v1/wg-peer-status", body)
	if err != nil {
		log.Printf("[agent ws] wg-peer-status push host=%s: %v", hostID, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[agent ws] wg-peer-status push host=%s: HTTP %d", hostID, resp.StatusCode)
	}
}

// POST /internal/v1/agents/kick {agent_id?, bot_id?} — close a live agent
// connection. Loopback-only (dock → hosts-svc). Returns {kicked: bool}.
func (p *Plugin) handleInternalAgentKick(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
		BotID   string `json:"bot_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	var conn *agentConn
	if id := strings.TrimSpace(req.AgentID); id != "" {
		conn = p.agentHub.lookup(id)
	}
	if conn == nil {
		if id := strings.TrimSpace(req.BotID); id != "" {
			conn = p.agentHub.lookup(id)
		}
	}
	if conn == nil {
		c.JSON(http.StatusOK, gin.H{"kicked": false})
		return
	}
	select {
	case <-conn.close:
	default:
		close(conn.close)
	}
	log.Printf("[agent ws] kicked by dock: token=%s bot=%s agent=%s", conn.tokenID, conn.botUserID, conn.agentID)
	c.JSON(http.StatusOK, gin.H{"kicked": true})
}

// resolveHostIDForAgentConn maps a connecting agent to its hosts.id.
// Order: (1) agents.agent_token_id = token (canonical v4 link);
// (2) agents.id = agent_id from the connect query — robust when our copy
// of agents.agent_token_id is stale (dock re-minted the token on a
// re-register and the dual-write missed; observed on zen 2026-08-16);
// (3) agents.bot_user_id = bot_id. On (2)/(3) we self-heal the stale
// agent_token_id so the next connect hits (1). Empty when unknown.
func (p *Plugin) resolveHostIDForAgentConn(tokenID, agentID, botUserID string) string {
	if host, err := p.getHostByAgentToken(tokenID); err == nil && host != nil {
		return host.ID
	}
	var hostID string
	if agentID != "" {
		if err := p.DB.QueryRow(`SELECT host_id FROM agents WHERE id = $1 LIMIT 1`, agentID).Scan(&hostID); err == nil && hostID != "" {
			p.healAgentTokenLink(agentID, tokenID)
			return hostID
		}
	}
	if botUserID != "" {
		var aid string
		if err := p.DB.QueryRow(`SELECT id, host_id FROM agents WHERE bot_user_id = $1 ORDER BY last_hello_at DESC NULLS LAST LIMIT 1`, botUserID).Scan(&aid, &hostID); err == nil && hostID != "" {
			p.healAgentTokenLink(aid, tokenID)
			return hostID
		}
	}
	return ""
}

func (p *Plugin) healAgentTokenLink(agentID, tokenID string) {
	if agentID == "" || tokenID == "" {
		return
	}
	if _, err := p.DB.Exec(`UPDATE agents SET agent_token_id = $2 WHERE id = $1 AND (agent_token_id IS DISTINCT FROM $2)`, agentID, tokenID); err != nil {
		log.Printf("[agent ws] heal agents.agent_token_id agent=%s: %v", agentID, err)
	} else {
		log.Printf("[agent ws] healed agents.agent_token_id agent=%s token=%s", agentID, tokenID)
	}
}
