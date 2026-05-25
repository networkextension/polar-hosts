package hosts

// internal_skill_catalog.go — loopback-trusted catalog lookup for
// dock. Same auth-skip pattern as internal_touch.go: dock dials the
// plugin svc on 127.0.0.1, polar-hosts trusts the loopback origin,
// no bearer required.
//
//   GET /internal/v1/skill-catalog/:id?workspace_id=<wid>
//
// workspace_id query param is the operator's session workspace
// (dock forwards it from the AuthMiddleware-populated context).
// polar-hosts uses it to enforce P3 visibility: returns 404 when
// the entry is workspace-private and doesn't match.
//
// Companion to polar-dock skill_install_handlers.go (P3-dock,
// merged in polar-dock#332). Should have shipped in polar-hosts#5
// but a force-push amend got lost in the merge race.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (p *Plugin) handleInternalSkillCatalogGet(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	workspaceID := strings.TrimSpace(c.Query("workspace_id"))
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace_id query param required"})
		return
	}
	e, err := p.GetSkillCatalogEntry(id)
	if errors.Is(err, ErrSkillCatalogNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Same visibility rule as handleSkillCatalogGet (auth-gated).
	if !e.IsPlatform && e.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if e.RetiredAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "retired"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"item": e})
}
