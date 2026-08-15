package hosts

// POST /internal/v1/hosts/enroll — machine-minted enrollment tokens.
//
// Sibling of the operator flow (POST /api/hosts/enroll, user session +
// team-admin). This one is for other plugin services on the same box
// (polar-cloud's cloud-svc creating a VM that must self-register on first
// boot). Loopback-only, same gate as /internal/v1/hosts/{touch,hello}.
//
// The caller passes the acting user_id (it already resolved the user via
// dock AuthVerify) so the token row keeps a real owner, and may ask for a
// longer TTL than the interactive 1h default — a VM image pull + boot can
// take a while — capped at 24h. Single-use semantics are unchanged.

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	enrollTTLMin     = time.Minute
	enrollTTLMaxMach = 24 * time.Hour
)

type internalEnrollReq struct {
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	HostName    string `json:"host_name"`
	TTLSeconds  int    `json:"ttl_seconds,omitempty"`
}

func (p *Plugin) handleInternalHostsEnroll(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	var req internalEnrollReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	ws := strings.TrimSpace(req.WorkspaceID)
	uid := strings.TrimSpace(req.UserID)
	name := strings.TrimSpace(req.HostName)
	if ws == "" || uid == "" || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace_id, user_id and host_name are required"})
		return
	}
	ttl := enrollmentTokenTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
		if ttl < enrollTTLMin {
			ttl = enrollTTLMin
		}
		if ttl > enrollTTLMaxMach {
			ttl = enrollTTLMaxMach
		}
	}
	now := time.Now().UTC()
	tokenID, raw, expiresAt, err := p.createEnrollmentTokenTTL(uid, ws, name, ttl, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create enrollment token: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token_id":   tokenID,
		"token":      raw,
		"expires_at": expiresAt,
		"host_name":  name,
	})
}
