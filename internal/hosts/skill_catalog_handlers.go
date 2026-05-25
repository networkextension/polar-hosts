package hosts

// skill_catalog_handlers.go — REST surface for the skill_catalog table.
//
//   GET    /api/skill-catalog              list (workspace-scoped + platform)
//   GET    /api/skill-catalog/:id          detail
//   POST   /api/skill-catalog              admin: register a new bundle
//   POST   /api/skill-catalog/:id/retire   admin: soft-delete
//   GET    /api/skill-catalog/:id/download 302 to download_url
//
// Admin gates are dock-side (workspace owner / system role). Plugin
// trusts the role claim that came back from dock's AuthVerify
// (requireAuthViaDock populates user_id, workspace_id, role in ctx).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type createSkillCatalogRequest struct {
	Publisher    string          `json:"publisher"`
	SkillKind    string          `json:"skill_kind"`
	Version      string          `json:"version"`
	SHA256       string          `json:"sha256"`
	DownloadURL  string          `json:"download_url"`
	SizeBytes    int64           `json:"size_bytes"`
	ManifestJSON json.RawMessage `json:"manifest_json,omitempty"`
	DisplayName  string          `json:"display_name,omitempty"`
	Description  string          `json:"description,omitempty"`
	License      string          `json:"license,omitempty"`
	Homepage     string          `json:"homepage,omitempty"`
	IsPlatform   bool            `json:"is_platform,omitempty"`
	WorkspaceID  string          `json:"workspace_id,omitempty"`
}

func (p *Plugin) handleSkillCatalogList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	includeRetired := strings.EqualFold(c.Query("include_retired"), "true")
	items, err := p.ListSkillCatalog(workspaceID, includeRetired)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list catalog failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (p *Plugin) handleSkillCatalogGet(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
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
	c.JSON(http.StatusOK, gin.H{"item": e})
}

func (p *Plugin) handleSkillCatalogCreate(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	if !isAdminRole(workspaceRole(c)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin only"})
		return
	}
	var req createSkillCatalogRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
		return
	}
	entry := &SkillCatalogEntry{
		Publisher:    strings.TrimSpace(req.Publisher),
		SkillKind:    strings.TrimSpace(req.SkillKind),
		Version:      strings.TrimSpace(req.Version),
		SHA256:       strings.ToLower(strings.TrimSpace(req.SHA256)),
		DownloadURL:  strings.TrimSpace(req.DownloadURL),
		SizeBytes:    req.SizeBytes,
		ManifestJSON: req.ManifestJSON,
		DisplayName:  req.DisplayName,
		Description:  req.Description,
		License:      req.License,
		Homepage:     req.Homepage,
		IsPlatform:   req.IsPlatform,
		WorkspaceID:  strings.TrimSpace(req.WorkspaceID),
		CreatedBy:    userID,
	}
	// Default: workspace-scoped bundles attach to the caller's workspace
	// unless explicitly marked is_platform.
	if !entry.IsPlatform && entry.WorkspaceID == "" {
		if wid, ok := requireWorkspaceID(c); ok {
			entry.WorkspaceID = wid
		}
	}
	out, err := p.CreateSkillCatalogEntry(entry)
	if errors.Is(err, ErrSkillCatalogConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"item": out})
}

func (p *Plugin) handleSkillCatalogRetire(c *gin.Context) {
	if !isAdminRole(workspaceRole(c)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin only"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	if err := p.RetireSkillCatalogEntry(id); err != nil {
		if errors.Is(err, ErrSkillCatalogNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleSkillCatalogDownload issues a 302 to the catalog's download_url.
// The catalog row's download_url is treated as canonical — operators
// upload to wherever they want (R2, GH releases, internal mirror) and
// dock just redirects. P3 may add an option to proxy/sign URLs at this
// hop for private workspace bundles.
func (p *Plugin) handleSkillCatalogDownload(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
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
	// Visibility check matches ListSkillCatalog's WHERE clause.
	if !e.IsPlatform && e.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if e.RetiredAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "retired"})
		return
	}
	c.Redirect(http.StatusFound, e.DownloadURL)
}

func isAdminRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "admin", "owner", "platform-admin":
		return true
	}
	return false
}
