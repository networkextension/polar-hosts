package hosts

// helpers.go — small functions copied from dock that the moved
// handlers depend on. Kept here so hosts-svc has no compile-time
// dependency on the dock package.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// agentTokenPrefix — mirrors dock's agent_store.go. Raw enrollment +
// agent tokens get this prefix so they're greppable in logs.
const agentTokenPrefix = "polar_agent_"

// generateAgentToken — returns (raw, sha256-hex). Used by enrollment
// token creation in hosts_store.go.
func generateAgentToken() (string, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw := agentTokenPrefix + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:]), nil
}

// hashAgentToken — sha256(raw) hex. For matching incoming tokens
// against the stored hash column.
func hashAgentToken(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// systemUserID — sentinel "system" user that owns workspace-agnostic
// resources. Matches dock's ai_agent.go constant.
const systemUserID = "system"

// Team role constants — mirror dock's teams_store.go. Used by the
// require-admin guards in host_handlers.go etc.
const (
	TeamRoleOwner  = "owner"
	TeamRoleAdmin  = "admin"
	TeamRoleMember = "member"
	TeamRoleViewer = "viewer"
)

// teamRoleAllows reports whether actorRole is at least minRole. Order:
// owner > admin > member > viewer. Unknown roles fall below viewer.
func teamRoleAllows(actorRole, minRole string) bool {
	rank := func(r string) int {
		switch r {
		case TeamRoleOwner:
			return 4
		case TeamRoleAdmin:
			return 3
		case TeamRoleMember:
			return 2
		case TeamRoleViewer:
			return 1
		default:
			return 0
		}
	}
	return rank(actorRole) >= rank(minRole)
}

// generateSessionID — random base64-URL token. 32 random bytes.
func generateSessionID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

// buildUploadFilename mirrors dock's handler_helpers.go function:
// timestamp + 8-char random + lower-case extension. Falls back to .img
// for non-alnum extensions.
func buildUploadFilename(original string) string {
	ext := strings.ToLower(filepath.Ext(original))
	if ext == "" || len(ext) > 8 {
		ext = ".img"
	} else {
		for _, r := range ext[1:] {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				ext = ".img"
				break
			}
		}
	}
	return fmt.Sprintf("%s_%s%s", time.Now().Format("20060102_150405"), generateSessionID()[:8], ext)
}

// requireUserID pulls the session user id from gin ctx (set by
// requireAuthViaDock). Returns ("", false) + writes 500 if missing
// (indicates middleware misorder).
func requireUserID(c *gin.Context) (string, bool) {
	v, _ := c.Get(ctxKeyUserID)
	id, ok := v.(string)
	if !ok || id == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return "", false
	}
	return id, true
}

// requireWorkspaceID pulls active workspace id from ctx; same shape
// as requireUserID.
func requireWorkspaceID(c *gin.Context) (string, bool) {
	v, _ := c.Get(ctxKeyWorkspaceID)
	id, ok := v.(string)
	if !ok || id == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return "", false
	}
	return id, true
}

// workspaceRole returns the caller's role in the active workspace.
// AuthVerify-fed; empty when middleware hasn't run.
func workspaceRole(c *gin.Context) string {
	v, _ := c.Get(ctxKeyUserRole)
	role, _ := v.(string)
	return role
}

// parseInt64Param parses a positive int64 path parameter; writes a
// 400 + returns (0,false) on failure.
func parseInt64Param(c *gin.Context, name string) (int64, bool) {
	raw := c.Param(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return 0, false
	}
	return id, true
}

// generateResourceID mirrors dock's resource-id helper: 16 random
// bytes hex. Used by createAgentToken etc.
func generateResourceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
