package hosts

// agent_dispatch_handlers.go — Phase 4b: loopback-only endpoints dock
// calls to dispatch frames to agents after the /ws/agent nginx cutover.
// No HMAC — same loopback-only trust model as /internal/v1/hosts/touch.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ── GET /internal/v1/agents/presence ──────────────────────────────────────
// Returns whether a bot's agent is currently connected + its capabilities.
// Dock uses this to decide passthrough vs tool-loop vs offline for a given
// bot before attempting a dispatch.

func (p *Plugin) handleInternalAgentPresence(c *gin.Context) {
	botID := strings.TrimSpace(c.Query("bot_id"))
	if botID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bot_id required"})
		return
	}
	conn := p.agentHub.lookup(botID)
	if conn == nil {
		c.JSON(http.StatusOK, gin.H{
			"bot_user_id": botID,
			"attached":    false,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"bot_user_id":   botID,
		"attached":      true,
		"has_passthrough": conn.hasPassthrough(),
		"tool":          conn.tool,
		"workdir":       conn.workdir,
		"capabilities":  conn.capabilities,
		"host_id":       conn.hostID,
	})
}

// ── POST /internal/v1/agents/dispatch/tool-call ───────────────────────────
// Dock posts a tool_call to an agent and waits for the tool_result.
// Timeout: 60 s (same as dock's in-process agentToolCallTimeout).

type agentDispatchToolCallReq struct {
	BotID  string         `json:"bot_id" binding:"required"`
	CallID string         `json:"call_id" binding:"required"`
	Tool   string         `json:"tool" binding:"required"`
	Args   map[string]any `json:"args"`
}

func (p *Plugin) handleInternalAgentDispatchToolCall(c *gin.Context) {
	var req agentDispatchToolCallReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Args == nil {
		req.Args = map[string]any{}
	}

	result, err := p.agentHub.dispatchToolCall(req.BotID, req.CallID, req.Tool, req.Args)
	if err != nil {
		status := dispatchErrStatus(err)
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── POST /internal/v1/agents/dispatch/chat-message ────────────────────────
// Dock posts a chat_message (passthrough path) and waits for the chat_reply.
// Timeout: 2 h (same as dock's in-process agentChatTimeout).

type agentDispatchChatReq struct {
	BotID          string `json:"bot_id" binding:"required"`
	ThreadID       int64  `json:"thread_id"`
	Content        string `json:"content" binding:"required"`
	WorkdirSubpath string `json:"workdir_subpath"`
	GitRemoteURL   string `json:"git_remote_url"`
}

func (p *Plugin) handleInternalAgentDispatchChatMessage(c *gin.Context) {
	var req agentDispatchChatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	reply, err := p.agentHub.dispatchChatMessage(req.BotID, req.ThreadID, req.Content, req.WorkdirSubpath, req.GitRemoteURL)
	if err != nil {
		status := dispatchErrStatus(err)
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, reply)
}

// ── POST /internal/v1/agents/dispatch/frame ───────────────────────────────
// Fire-and-forget: dock posts an arbitrary JSON frame (research_task,
// task.wake) to an agent. Returns 200 as soon as the frame is enqueued
// on the WS send buffer.

type agentDispatchFrameReq struct {
	BotID string         `json:"bot_id" binding:"required"`
	Frame map[string]any `json:"frame" binding:"required"`
}

func (p *Plugin) handleInternalAgentDispatchFrame(c *gin.Context) {
	var req agentDispatchFrameReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := p.agentHub.dispatchFrame(req.BotID, req.Frame); err != nil {
		status := dispatchErrStatus(err)
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "queued_at": time.Now().UTC()})
}

func dispatchErrStatus(err error) int {
	switch {
	case errors.Is(err, errAgentNotAttached):
		return http.StatusServiceUnavailable
	case errors.Is(err, errAgentTimeout):
		return http.StatusGatewayTimeout
	case errors.Is(err, errAgentDisconnected), errors.Is(err, errAgentSendTimeout):
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}
