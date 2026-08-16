package hosts

// GET /internal/v1/hosts/lookup?id=<host_id> | ?workspace_id=&name=
//
// Loopback-only read for sibling plugin services on the same box
// (polar-cloud's cloud-svc): resolve a host by id or by (workspace, name)
// and return liveness + its agents. cloud-svc uses it to decide a VM is
// "running" (its guest agent has a fresh last_seen_at) and to find the
// guest's host_id / bot for pinned compute-tasks.

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type lookupAgent struct {
	ID          string     `json:"id"`
	BotUserID   string     `json:"bot_user_id,omitempty"`
	LastHelloAt *time.Time `json:"last_hello_at,omitempty"`
}

func (p *Plugin) handleInternalHostLookup(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	id := strings.TrimSpace(c.Query("id"))
	ws := strings.TrimSpace(c.Query("workspace_id"))
	name := strings.TrimSpace(c.Query("name"))
	var (
		h   *Host
		err error
	)
	switch {
	case id != "":
		h, err = p.getHostByID(id)
	case ws != "" && name != "":
		h, err = p.scanHost(p.DB.QueryRow(hostSelectColumns+` WHERE workspace_id = $1 AND name = $2 ORDER BY created_at DESC LIMIT 1`, ws, name))
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "id or workspace_id+name required"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if h == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "host not found"})
		return
	}
	agents := []lookupAgent{}
	rows, qerr := p.DB.Query(`SELECT id, COALESCE(bot_user_id,''), last_hello_at FROM agents WHERE host_id = $1 ORDER BY last_hello_at DESC NULLS LAST`, h.ID)
	if qerr == nil {
		defer rows.Close()
		for rows.Next() {
			var a lookupAgent
			if rows.Scan(&a.ID, &a.BotUserID, &a.LastHelloAt) == nil {
				agents = append(agents, a)
			}
		}
	}
	online := false
	if h.LastSeenAt != nil && time.Since(*h.LastSeenAt) < 90*time.Second {
		online = true
	}
	// Live hub check beats last_seen_at when we have the conn.
	if conn := p.agentHub.lookupByHostID(h.ID); conn != nil {
		online = true
	}
	c.JSON(http.StatusOK, gin.H{
		"host": gin.H{
			"id": h.ID, "workspace_id": h.WorkspaceID, "name": h.Name, "os": h.OS, "arch": h.Arch,
			"last_seen_at": h.LastSeenAt, "last_seen_ip": h.LastSeenIP, "created_at": h.CreatedAt,
		},
		"online": online,
		"agents": agents,
	})
}
