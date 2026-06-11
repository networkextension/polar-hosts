package hosts

// agent_hub.go — Phase 4a/4b: in-process agentHub for polar-hosts.
// Phase 4a: real hub replacing agentHubStub; skill dispatch for shell/VNC.
// Phase 4b: full dispatch for dock's AI loop (tool_call, chat_message, frame).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// ── Wire types (mirrors dock's agent_hub.go so polar-agent can unmarshal identically) ──

type skillStartEnvelope struct {
	Kind      string         `json:"kind"` // "skill.start"
	RunID     int64          `json:"run_id"`
	SkillKind string         `json:"skill_kind"`
	Config    map[string]any `json:"config"`
}

type skillEventFrame struct {
	Kind      string         `json:"kind"` // "skill.event"
	RunID     int64          `json:"run_id"`
	EventKind string         `json:"event_kind"`
	Data      map[string]any `json:"data,omitempty"`
}

type agentToolResult struct {
	Kind   string         `json:"kind"` // "tool_result"
	ID     string         `json:"id"`
	OK     bool           `json:"ok"`
	Stdout string         `json:"stdout,omitempty"`
	Stderr string         `json:"stderr,omitempty"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type agentChatReply struct {
	Kind    string `json:"kind"` // "chat_reply"
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Content string `json:"content,omitempty"`
	Stderr  string `json:"stderr,omitempty"`
	Error   string `json:"error,omitempty"`
}

const (
	agentToolCallTimeout = 60 * time.Second
	agentChatTimeout     = 2 * time.Hour
	agentSendTimeout     = 5 * time.Second
)

var (
	errAgentNotAttached  = errors.New("agent not attached")
	errAgentDisconnected = errors.New("agent disconnected")
	errAgentSendTimeout  = errors.New("agent send buffer full")
	errAgentTimeout      = errors.New("agent timed out")
)

// agentConn is one live polar-agent WebSocket connection.
type agentConn struct {
	tokenID   string
	userID    string
	botUserID string
	agentID   string
	hostID    string

	// Populated from hello frame.
	tool         string
	workdir      string
	capabilities []string

	send  chan []byte
	close chan struct{}

	// Tool-call round-trip (tool_call → tool_result).
	pendingMu sync.Mutex
	pending   map[string]chan agentToolResult

	// Chat passthrough round-trip (chat_message → chat_reply).
	chatPendingMu sync.Mutex
	chatPending   map[string]chan agentChatReply

	// Skill lifecycle events (skill.start → skill.event).
	skillPendingMu sync.Mutex
	skillPending   map[int64]chan skillEventFrame

	// Skill stdout stream (skill.event event_kind=stdout).
	skillStdoutMu      sync.Mutex
	skillStdoutPending map[int64]chan []byte
}

func (c *agentConn) hasPassthrough() bool {
	for _, cap := range c.capabilities {
		if cap == "passthrough" || cap == "kimi" {
			return true
		}
	}
	return false
}

// ── Tool-call pending ──────────────────────────────────────────────────────

func (c *agentConn) trackPending(id string) chan agentToolResult {
	ch := make(chan agentToolResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	return ch
}

func (c *agentConn) deliverResult(r agentToolResult) {
	c.pendingMu.Lock()
	ch, ok := c.pending[r.ID]
	if ok {
		delete(c.pending, r.ID)
	}
	c.pendingMu.Unlock()
	if ok {
		ch <- r
	}
}

// ── Chat-reply pending ─────────────────────────────────────────────────────

func (c *agentConn) trackChatPending(id string) chan agentChatReply {
	ch := make(chan agentChatReply, 1)
	c.chatPendingMu.Lock()
	c.chatPending[id] = ch
	c.chatPendingMu.Unlock()
	return ch
}

func (c *agentConn) deliverChatReply(r agentChatReply) {
	c.chatPendingMu.Lock()
	ch, ok := c.chatPending[r.ID]
	if ok {
		delete(c.chatPending, r.ID)
	}
	c.chatPendingMu.Unlock()
	if ok {
		ch <- r
	}
}

// ── Skill pending (shell/VNC lifecycle) ───────────────────────────────────

func (c *agentConn) trackSkillPending(runID int64) chan skillEventFrame {
	ch := make(chan skillEventFrame, 16)
	c.skillPendingMu.Lock()
	c.skillPending[runID] = ch
	c.skillPendingMu.Unlock()
	return ch
}

func (c *agentConn) untrackSkillPending(runID int64) {
	c.skillPendingMu.Lock()
	delete(c.skillPending, runID)
	c.skillPendingMu.Unlock()
}

func (c *agentConn) deliverSkillEvent(ev skillEventFrame) {
	c.skillPendingMu.Lock()
	ch, ok := c.skillPending[ev.RunID]
	c.skillPendingMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- ev:
	default:
	}
}

// ── Skill stdout ──────────────────────────────────────────────────────────

func (c *agentConn) trackSkillStdout(runID int64) chan []byte {
	ch := make(chan []byte, 256)
	c.skillStdoutMu.Lock()
	c.skillStdoutPending[runID] = ch
	c.skillStdoutMu.Unlock()
	return ch
}

func (c *agentConn) untrackSkillStdout(runID int64) {
	c.skillStdoutMu.Lock()
	delete(c.skillStdoutPending, runID)
	c.skillStdoutMu.Unlock()
}

func (c *agentConn) deliverSkillStdout(runID int64, chunk []byte) {
	c.skillStdoutMu.Lock()
	ch, ok := c.skillStdoutPending[runID]
	c.skillStdoutMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- chunk:
	default:
	}
}

// ── Hub ───────────────────────────────────────────────────────────────────

type agentHub struct {
	mu      sync.Mutex
	byToken map[string]*agentConn // tokenID → conn
	byBot   map[string]*agentConn // botUserID → conn (for dispatch)
}

func newAgentHub() *agentHub {
	return &agentHub{
		byToken: map[string]*agentConn{},
		byBot:   map[string]*agentConn{},
	}
}

func (h *agentHub) register(c *agentConn) *agentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.byToken[c.tokenID]
	h.byToken[c.tokenID] = c
	if c.botUserID != "" {
		h.byBot[c.botUserID] = c
	}
	return prev
}

func (h *agentHub) unregister(c *agentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.byToken[c.tokenID]; ok && cur == c {
		delete(h.byToken, c.tokenID)
	}
	if c.botUserID != "" {
		if cur, ok := h.byBot[c.botUserID]; ok && cur == c {
			delete(h.byBot, c.botUserID)
		}
	}
}

// lookup resolves bot_user_id or agent_id to a live conn.
func (h *agentHub) lookup(key string) *agentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.byBot[key]; ok {
		return c
	}
	for _, c := range h.byToken {
		if c.agentID == key {
			return c
		}
	}
	return nil
}

// lookupByHostID returns the live conn for a host. Linear scan — fine
// for a typical fleet of <10 attached agents per hosts-svc process.
func (h *agentHub) lookupByHostID(hostID string) *agentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.byToken {
		if c.hostID == hostID {
			return c
		}
	}
	return nil
}

func (h *agentHub) lookupByTokenID(tokenID string) *agentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byToken[tokenID]
}

// ── AI-loop dispatch (Phase 4b) ───────────────────────────────────────────

// dispatchToolCall sends a tool_call to the agent and waits for the result.
func (h *agentHub) dispatchToolCall(botUserID, callID, tool string, args map[string]any) (*agentToolResult, error) {
	conn := h.lookup(botUserID)
	if conn == nil {
		return nil, errAgentNotAttached
	}
	payload, err := json.Marshal(map[string]any{
		"kind": "tool_call",
		"id":   callID,
		"tool": tool,
		"args": args,
	})
	if err != nil {
		return nil, err
	}
	resultCh := conn.trackPending(callID)
	select {
	case conn.send <- payload:
	case <-conn.close:
		return nil, errAgentDisconnected
	case <-time.After(agentSendTimeout):
		return nil, errAgentSendTimeout
	}
	select {
	case r := <-resultCh:
		return &r, nil
	case <-conn.close:
		return nil, errAgentDisconnected
	case <-time.After(agentToolCallTimeout):
		conn.pendingMu.Lock()
		delete(conn.pending, callID)
		conn.pendingMu.Unlock()
		return nil, errAgentTimeout
	}
}

// dispatchChatMessage sends a chat_message to the agent and waits for the reply.
func (h *agentHub) dispatchChatMessage(botUserID string, threadID int64, content, workdirSubpath, gitRemoteURL string) (*agentChatReply, error) {
	conn := h.lookup(botUserID)
	if conn == nil {
		return nil, errAgentNotAttached
	}
	if !conn.hasPassthrough() {
		return nil, errors.New("agent does not have passthrough capability")
	}
	id := generateID()
	payload, err := json.Marshal(map[string]any{
		"kind":            "chat_message",
		"id":              id,
		"thread_id":       threadID,
		"content":         content,
		"workdir_subpath": workdirSubpath,
		"git_remote_url":  gitRemoteURL,
	})
	if err != nil {
		return nil, err
	}
	resultCh := conn.trackChatPending(id)
	select {
	case conn.send <- payload:
	case <-conn.close:
		return nil, errAgentDisconnected
	case <-time.After(agentSendTimeout):
		return nil, errAgentSendTimeout
	}
	select {
	case r := <-resultCh:
		return &r, nil
	case <-conn.close:
		return nil, errAgentDisconnected
	case <-time.After(agentChatTimeout):
		conn.chatPendingMu.Lock()
		delete(conn.chatPending, id)
		conn.chatPendingMu.Unlock()
		return nil, errAgentTimeout
	}
}

// dispatchFrame sends an arbitrary JSON frame to the agent. Fire-and-forget —
// used for research_task and task.wake where dock doesn't wait for a reply.
func (h *agentHub) dispatchFrame(botUserID string, frame map[string]any) error {
	conn := h.lookup(botUserID)
	if conn == nil {
		return errAgentNotAttached
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	select {
	case conn.send <- payload:
		return nil
	case <-conn.close:
		return errAgentDisconnected
	case <-time.After(agentSendTimeout):
		return errAgentSendTimeout
	}
}

// ── Skill dispatch (shell/VNC, Phase 4a) ──────────────────────────────────

func (h *agentHub) dispatchSkillStart(conn *agentConn, env skillStartEnvelope) error {
	if conn == nil {
		return errAgentNotAttached
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	select {
	case conn.send <- payload:
		return nil
	case <-conn.close:
		return errAgentDisconnected
	case <-time.After(agentSendTimeout):
		return errAgentSendTimeout
	}
}

func (h *agentHub) dispatchSkillStop(conn *agentConn, runID int64, reason string) error {
	if conn == nil {
		return errAgentNotAttached
	}
	payload, err := json.Marshal(map[string]any{
		"kind": "skill.stop", "run_id": runID, "reason": reason,
	})
	if err != nil {
		return err
	}
	select {
	case conn.send <- payload:
		return nil
	case <-conn.close:
		return errAgentDisconnected
	case <-time.After(agentSendTimeout):
		return errAgentSendTimeout
	}
}

func (h *agentHub) dispatchSkillStdin(conn *agentConn, runID int64, data []byte) error {
	if conn == nil {
		return errAgentNotAttached
	}
	payload, err := json.Marshal(map[string]any{
		"kind": "skill.stdin", "run_id": runID,
		"bytes_b64": base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		return err
	}
	select {
	case conn.send <- payload:
		return nil
	case <-conn.close:
		return errAgentDisconnected
	case <-time.After(agentSendTimeout):
		return errAgentSendTimeout
	}
}

func (h *agentHub) dispatchSkillResize(conn *agentConn, runID int64, rows, cols uint16) error {
	if conn == nil {
		return errAgentNotAttached
	}
	payload, err := json.Marshal(map[string]any{
		"kind": "skill.resize", "run_id": runID, "rows": rows, "cols": cols,
	})
	if err != nil {
		return err
	}
	select {
	case conn.send <- payload:
		return nil
	case <-conn.close:
		return errAgentDisconnected
	case <-time.After(agentSendTimeout):
		return errAgentSendTimeout
	}
}

// generateID returns a short unique correlation ID for pending requests.
// Uses crypto-quality randomness via the host package's own helper.
func generateID() string {
	return generateResourceID()
}
