package hosts

// DELETE /internal/v1/hosts/:id — retire a host that a sibling plugin owns
// the lifecycle of (polar-cloud: the guest of a destroyed VM). Loopback-only,
// same gate as /internal/v1/hosts/{enroll,lookup}.
//
// Effect: kick the live agent connection (if any), delete the hosts row
// (agents / host_skills / host_skill_runs cascade), and revoke the local
// agent_tokens rows that belonged to its agents so a lingering guest cannot
// reconnect. dock's own agents/agent_tokens mirror is left to dock (token
// source of truth; a dock-side revoke hook is a follow-up). Idempotent:
// unknown id → 200 {deleted:false}.

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (p *Plugin) handleInternalHostDelete(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	hostID := strings.TrimSpace(c.Param("id"))
	if hostID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	var name, ws string
	if err := p.DB.QueryRow(`SELECT name, workspace_id FROM hosts WHERE id = $1`, hostID).Scan(&name, &ws); err != nil {
		c.JSON(http.StatusOK, gin.H{"deleted": false, "host_id": hostID})
		return
	}
	// agent tokens to revoke (before the cascade removes the agents rows)
	tokenIDs := []string{}
	if rows, err := p.DB.Query(`SELECT agent_token_id FROM agents WHERE host_id = $1`, hostID); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil && id != "" {
				tokenIDs = append(tokenIDs, id)
			}
		}
		rows.Close()
	}
	kicked := false
	if conn := p.agentHub.lookupByHostID(hostID); conn != nil {
		select {
		case <-conn.close:
		default:
			close(conn.close)
		}
		kicked = true
	}
	if err := p.deleteHost(hostID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete host: " + err.Error()})
		return
	}
	revoked := 0
	now := time.Now().UTC()
	for _, id := range tokenIDs {
		if res, err := p.DB.Exec(`UPDATE agent_tokens SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1`, id, now); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				revoked++
			}
		}
	}
	log.Printf("[internal] host %s (%s, ws %s) deleted: kicked=%v tokens_revoked=%d", hostID, name, ws, kicked, revoked)
	c.JSON(http.StatusOK, gin.H{"deleted": true, "host_id": hostID, "name": name, "workspace_id": ws, "kicked": kicked, "tokens_revoked": revoked})
}
