package hosts

// Agent Identity v4 — read endpoints for the agents table.
// See doc/arch/agent-identity-v4.md.
//
//   GET /api/agents                — workspace-scoped list
//   GET /api/agents/:id            — single agent + nested host block
//   GET /api/hosts/:id/agents      — agents on one host
//
// All three are workspace-member auth (requireAuthViaDock then
// requireWorkspaceID enforces the active workspace boundary).
// Phase F UI consumes these for /agents.html + /hosts.html.
//
// Mutating ops (kick, rename, etc.) are dock-side because they need
// the in-memory agent_hub registry — see /api/admin/agents/:id/kick
// on the dock surface.

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// handleListAgents returns every agent in the active workspace.
// JSON shape matches what Phase F's src/api/agents.ts expects: the
// hosts.name + host_info_json->>'hw_model' are joined in so a single
// fetch populates the agents table without an N+1 hosts probe.
func (p *Plugin) handleListAgents(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	agents, err := p.listAgents(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

// handleGetAgent returns one agent + a nested "host" block. 404 when
// the agent exists in another workspace (don't leak cross-tenant
// existence).
func (p *Plugin) handleGetAgent(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 agent id"})
		return
	}
	agent, err := p.getAgent(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent 不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if agent == nil || agent.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent 不存在或不在当前工作区"})
		return
	}
	// Nested host block — UI shows hw_model + os/arch on the agent
	// detail page so caller doesn't have to fan-out a separate /api/hosts/:id.
	host, _ := p.getHostByID(agent.HostID)
	c.JSON(http.StatusOK, gin.H{
		"agent": agent,
		"host":  host,
	})
}

// handleListAgentsByHost returns the agents attached to one host.
// Used by the host detail page's "▾ 实例 (N)" expansion. 404 when the
// host is in another workspace.
func (p *Plugin) handleListAgentsByHost(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	hostID := strings.TrimSpace(c.Param("id"))
	if hostID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 host id"})
		return
	}
	host, err := p.getHostByID(hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if host == nil || host.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "host 不存在或不在当前工作区"})
		return
	}
	agents, err := p.listAgentsByHost(hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"agents": agents})
}
