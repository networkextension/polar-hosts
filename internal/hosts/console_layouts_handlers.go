package hosts

// HTTP handlers for /api/console/layouts/* — per-user named tile
// layouts for the /console.html workspace.
//
//   GET    /api/console/layouts             list active workspace's layouts owned by caller
//   POST   /api/console/layouts             create  body: {name, panes_config}
//   PATCH  /api/console/layouts/:layoutId   update  body: {name?, panes_config?}
//   DELETE /api/console/layouts/:layoutId   delete
//
// All four require workspace membership (existing AuthMiddleware
// behavior); layouts are owner-scoped — the WHERE clauses prevent one
// user from reading / editing another's layout in the same workspace.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

func (p *Plugin) handleConsoleLayoutList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	layouts, err := p.listConsoleLayoutsForOwner(workspaceID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"layouts": layouts})
}

func (p *Plugin) handleConsoleLayoutCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	var req struct {
		Name        string              `json:"name" binding:"required"`
		PanesConfig []ConsolePaneConfig `json:"panes_config" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	layout, err := p.createConsoleLayout(workspaceID, userID, req.Name, req.PanesConfig, time.Now().UTC())
	if err != nil {
		// Unique-violation = duplicate (workspace, user, name).
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "已存在同名布局"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"layout": layout})
}

func (p *Plugin) handleConsoleLayoutUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("layoutId")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid layoutId"})
		return
	}
	var req struct {
		Name        *string              `json:"name"`
		PanesConfig *[]ConsolePaneConfig `json:"panes_config"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	layout, err := p.updateConsoleLayout(workspaceID, userID, id, req.Name, req.PanesConfig, time.Now().UTC())
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "已存在同名布局"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if layout == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "布局不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"layout": layout})
}

func (p *Plugin) handleConsoleLayoutDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("layoutId")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid layoutId"})
		return
	}
	deleted, err := p.deleteConsoleLayout(workspaceID, userID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "布局不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
