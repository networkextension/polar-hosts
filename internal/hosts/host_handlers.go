package hosts

// HTTP handlers for the Host module — Phase 0.
//
//   POST   /api/hosts/enroll          (admin) → mint one-time enroll token
//   POST   /api/hosts/register        (anyone with token) → create host row
//   GET    /api/hosts                 (workspace member) → list workspace hosts
//   GET    /api/hosts/:id             (workspace member) → host detail
//   DELETE /api/hosts/:id             (admin) → revoke + cascade
//
// All workspace-scoped (active workspace = X-Workspace-Id header,
// already resolved by AuthMiddleware). Mutations gated on
// TeamRoleAdmin in the active workspace.
//
// The register endpoint is special: it uses the enrollment token as
// the bearer (NOT the user's session cookie), since the agent doing
// the registration is on a different machine that has no user session.

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// handleHostEnroll mints a one-time agent_token for an admin who's
// about to install polar-agent on a new machine. The UI surfaces the
// raw token + a curl install snippet; operator copies, runs on the
// target host. Tokens TTL 1h, single-use (consumed by /register).
func (p *Plugin) handleHostEnroll(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
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
	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 不能为空"})
		return
	}
	now := time.Now().UTC()
	_, raw, expiresAt, err := p.createEnrollmentToken(userID, workspaceID, name, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法创建注册凭证"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token":      raw,
		"expires_at": expiresAt,
		"host_name":  name,
		// install snippet returned ready-to-copy; UI shows verbatim
		"install_hint": "在目标机器上运行：\n  polar-agent register --token=" + raw + " --name=\"" + name + "\"",
	})
}

// handleHostRegister is what the agent calls (POST with Bearer
// auth = the enroll token). On success the agent gets back a
// permanent agent_token raw string + the host_id. The original
// enroll token is consumed (no longer usable for re-register).
//
// Note: this endpoint does NOT require a user session cookie — the
// enroll token IS the auth. AuthMiddleware should be wired to skip
// session check on this path; routes registration in app.go uses
// a bare s.handleHostRegister without AuthMiddleware to make that
// explicit.
func (p *Plugin) handleHostRegister(c *gin.Context) {
	bearer := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if bearer == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return
	}
	var req struct {
		HostOS   string `json:"host_os"`
		HostArch string `json:"host_arch"`
	}
	_ = c.ShouldBindJSON(&req) // body optional; agent advertises arch/os via WS later anyway

	now := time.Now().UTC()
	tokenID, _, workspaceID, hostName, err := p.consumeEnrollmentToken(bearer, now)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or unknown enrollment token"})
		case errors.Is(err, errEnrollmentExpired):
			c.JSON(http.StatusGone, gin.H{"error": "enrollment token expired"})
		case errors.Is(err, errEnrollmentAlready):
			c.JSON(http.StatusConflict, gin.H{"error": "enrollment token already consumed"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "registration failed"})
		}
		return
	}
	host, err := p.createHost(workspaceID, hostName, tokenID, req.HostOS, req.HostArch, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法创建 host: " + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"host":           host,
		"agent_token_id": tokenID,
		// agent_token raw is the SAME bearer it just used — that
		// token now becomes its permanent credential. Agent caches it
		// locally and uses it for all subsequent WS attaches.
		"agent_token_raw": bearer,
	})
}

func (p *Plugin) handleHostList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	hosts, err := p.listHostsForWorkspace(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hosts": hosts})
}

func (p *Plugin) handleHostDetail(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 host id"})
		return
	}
	host, err := p.getHostByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if host == nil || host.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "host 不存在或不在当前工作区"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"host": host})
}

// handleHostSkillsListWorkspace returns every host_skill row for the
// active workspace — used by the bot edit page's binding picker
// (P1d). Workspace member auth (read-only); admin not required.
func (p *Plugin) handleHostSkillsListWorkspace(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	skills, err := p.listHostSkillsForWorkspace(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"skills": skills})
}

// handleHostSkillsListForHost returns host_skill rows for one host,
// used by the host detail page so the UI can render a skill card per
// row (with the per-skill credentials below it).
func (p *Plugin) handleHostSkillsListForHost(c *gin.Context) {
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
	skills, err := p.listHostSkills(hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"skills": skills})
}

func (p *Plugin) handleHostDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	if !teamRoleAllows(workspaceRole(c), TeamRoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要管理员权限"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 host id"})
		return
	}
	host, err := p.getHostByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if host == nil || host.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "host 不存在或不在当前工作区"})
		return
	}
	// Cascade is handled by the FK ON DELETE CASCADE on host_skills /
	// host_skill_runs. agent_token row is left intact (in case the
	// admin wants to inspect / revoke it separately); a follow-up PR
	// can add an optional `revoke_agent_token` query flag.
	if err := p.deleteHost(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
