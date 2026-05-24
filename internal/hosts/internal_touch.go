package hosts

// /internal/v1/hosts/touch — dock posts here from its WS agent_hub when
// a connected polar-agent sends a `skill.advertise` frame. Pre-H9 the
// dock owned the hosts table and persisted directly; post-H9 the table
// moved into the polar_hosts DB which the dock can't reach, so the dock
// hands the data over via this endpoint instead.
//
// Auth: loopback-only. Both dock and polar-hosts-svc run on the same
// host (the deployment convention — see umbrella deploy.md). A request
// from anywhere else is refused with 403. Mutual TLS / HMAC would be
// cleaner but adds setup friction for a same-host call; loopback is
// the polar-allow-local-bootstrap pattern reused.

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type hostTouchRequest struct {
	HostID   string             `json:"host_id"`
	Skills   []AdvertisedSkill  `json:"skills"`
	RemoteIP string             `json:"remote_ip"`
	SeenAt   *time.Time         `json:"seen_at,omitempty"`
}

func (p *Plugin) handleInternalHostTouch(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	var req hostTouchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.HostID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "host_id required"})
		return
	}
	now := time.Now().UTC()
	if req.SeenAt != nil {
		now = req.SeenAt.UTC()
	}
	if err := p.updateHostAdvertisedSkills(req.HostID, req.Skills, req.RemoteIP, now); err != nil {
		log.Printf("internal/v1/hosts/touch: %s: %v", req.HostID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist failed"})
		return
	}
	for _, sk := range req.Skills {
		if _, err := p.ensureHostSkillFromAdvertise(req.HostID, sk, now); err != nil {
			// Per-skill failure is non-fatal; one bad row shouldn't drop the heartbeat.
			log.Printf("internal/v1/hosts/touch ensure_skill host=%s kind=%s: %v", req.HostID, sk.Kind, err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "host_id": req.HostID, "skills": len(req.Skills)})
}

// isLoopbackRequest returns true when the request reached the server on
// 127.0.0.1 / ::1. nginx is never in front of /internal/v1 (the dock
// dials the plugin svc directly on 127.0.0.1:<port>), so X-Forwarded-For
// is intentionally ignored.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// MarshalSkills is a tiny convenience for tests + the dock-side caller
// that wants to confirm the JSON shape it produces matches what we
// accept. Not used by the handler itself.
func MarshalSkills(s []AdvertisedSkill) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
