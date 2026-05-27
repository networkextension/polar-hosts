package hosts

// Host module P1e — loopback-only co-located bootstrap.
//
//   POST /api/hosts/local-bootstrap   body: {name, host_os, host_arch}
//
// Provisions a host + permanent agent_token in one shot, skipping the
// usual enroll-token dance. Designed for the 1:1 deployment where
// polar-dock + polar-agent run on the same machine and connect via
// loopback. The endpoint is opt-in (POLAR_ALLOW_LOCAL_BOOTSTRAP=true)
// and the handler additionally rejects any request whose remote IP
// isn't loopback or that carries proxy headers — so even with the
// flag on, nothing remote can reach the trust shortcut.
//
// Ownership: the host's workspace_id is the first available workspace
// owned by the first admin user in the system. For a fresh single-box
// deploy this is unambiguous. If you have multiple admins, the first
// one wins; transfer via UI later if needed.

import (
	"database/sql"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (p *Plugin) handleHostLocalBootstrap(c *gin.Context) {
	if !p.polarAllowLocalBootstrap {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "local-bootstrap disabled; start polar-dock with POLAR_ALLOW_LOCAL_BOOTSTRAP=true to enable",
		})
		return
	}
	// Loopback check: RemoteAddr must be 127.0.0.1 or ::1. We don't
	// trust c.ClientIP() (gin's helper) here because that consults
	// X-Forwarded-For — defeating the loopback guard. Pull the raw
	// remote address from the request directly.
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		host = c.Request.RemoteAddr
	}
	if !isLoopbackAddr(host) {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "local-bootstrap only honors loopback requests (got " + host + ")",
		})
		return
	}
	// Proxy headers disqualify the request — even if RemoteAddr is
	// loopback, the presence of X-Forwarded-For / X-Real-IP means the
	// hop came through nginx (or similar), so the original client is
	// not trusted.
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"} {
		if strings.TrimSpace(c.GetHeader(hdr)) != "" {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "local-bootstrap rejects proxied requests (" + hdr + " header present)",
			})
			return
		}
	}

	var req struct {
		Name     string `json:"name"`
		HostOS   string `json:"host_os"`
		HostArch string `json:"host_arch"`
		// MachineUUID — see handleHostRegister. Same dedup contract:
		// non-empty → updateOrInsert by (workspace_id, machine_uuid).
		MachineUUID string `json:"machine_uuid"`
	}
	_ = c.ShouldBindJSON(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "localhost"
	}

	// Pick a bootstrap owner: first user with role='admin'. The
	// schema doesn't enforce uniqueness on role, so 'first' = lowest
	// created_at to keep this deterministic across reboots.
	userID, workspaceID, err := p.pickLocalBootstrapOwner()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "no admin user / workspace found; sign up via UI first, then retry local-bootstrap",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "bootstrap owner: " + err.Error()})
		return
	}

	now := time.Now().UTC()
	rawToken, tok, err := p.createAgentToken(userID, "local-bootstrap "+name, AgentCoderConfig{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mint agent_token: " + err.Error()})
		return
	}
	hostRow, err := p.createOrUpdateHostByMachineUUID(workspaceID, name, tok.ID, req.HostOS, req.HostArch, req.MachineUUID, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create host: " + err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"host":             hostRow,
		"agent_token_id":   tok.ID,
		"agent_token_raw":  rawToken,
		"workspace_id":     workspaceID,
		"bootstrap_user":   userID,
	})
}

// isLoopbackAddr returns true if the address is recognizably localhost.
// Accepts 127.0.0.0/8, ::1, and the bare "localhost" string (which
// shouldn't appear in RemoteAddr but doesn't hurt to handle).
func isLoopbackAddr(addr string) bool {
	if addr == "" {
		return false
	}
	if addr == "localhost" {
		return true
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// pickLocalBootstrapOwner returns (userID, workspaceID) for the
// first real admin in the system + their primary workspace. The
// primary workspace is the lowest-id row in teams that the user
// owns; for a fresh deploy there's exactly one (their personal
// team).
//
// Skips the seeded `system` admin: it's a service identity used by
// internal jobs (audit logs, default workspace owner for templated
// content) and has no real operator behind it. If local-bootstrap
// picks `system`, the resulting host lives in a workspace nobody
// browses to and the UI host list looks empty.
func (p *Plugin) pickLocalBootstrapOwner() (string, string, error) {
	var userID string
	err := p.DB.QueryRow(`
		SELECT id FROM users
		WHERE role = 'admin' AND id <> 'system'
		ORDER BY created_at ASC
		LIMIT 1`,
	).Scan(&userID)
	if err != nil {
		return "", "", err
	}
	var workspaceID string
	err = p.DB.QueryRow(`
		SELECT id FROM teams
		WHERE owner_user_id = $1
		ORDER BY created_at ASC
		LIMIT 1`,
		userID,
	).Scan(&workspaceID)
	if err != nil {
		return "", "", err
	}
	return userID, workspaceID, nil
}
