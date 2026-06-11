package hosts

// agent_hub.go — Phase 4a: real agentHub for polar-hosts.
// Replaces agentHubStub from stubs.go. Keyed by tokenID (one conn per
// live agent token). The shell/VNC supervisors call lookupByHostID and
// the dispatch methods unchanged; the stubs just log+ErrNotWired, these
// actually send WS frames.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// skillStartEnvelope is the platform → agent dispatch payload.
// Mirrors dock's agent_hub.go so polar-agent unmarshals identically.
type skillStartEnvelope struct {
	Kind      string         `json:"kind"` // "skill.start"
	RunID     int64          `json:"run_id"`
	SkillKind string         `json:"skill_kind"`
	Config    map[string]any `json:"config"`
}

// skillEventFrame is the agent → hub lifecycle event.
type skillEventFrame struct {
	Kind      string         `json:"kind"` // "skill.event"
	RunID     int64          `json:"run_id"`
	EventKind string         `json:"event_kind"`
	Data      map[string]any `json:"data,omitempty"`
}

// agentConn is one live polar-agent WebSocket connection.
type agentConn struct {
	tokenID string
	userID  string
	hostID  string // resolved from hosts table; may be "" for legacy agents

	send  chan []byte
	close chan struct{}

	skillPendingMu sync.Mutex
	skillPending   map[int64]chan skillEventFrame

	skillStdoutMu      sync.Mutex
	skillStdoutPending map[int64]chan []byte
}

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

// agentHub holds all live connections keyed by tokenID.
type agentHub struct {
	mu      sync.Mutex
	byToken map[string]*agentConn
}

func newAgentHub() *agentHub {
	return &agentHub{byToken: map[string]*agentConn{}}
}

// register adds conn; displaces any previous conn with the same tokenID
// (reconnect). Returns the displaced conn so the caller can close it.
func (h *agentHub) register(c *agentConn) *agentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.byToken[c.tokenID]
	h.byToken[c.tokenID] = c
	return prev
}

func (h *agentHub) unregister(c *agentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.byToken[c.tokenID]; ok && cur == c {
		delete(h.byToken, c.tokenID)
	}
}

// lookupByHostID returns the live conn for a host. Linear scan is fine —
// a typical box runs <10 attached agents.
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

var (
	errAgentNotAttached  = errors.New("agent not attached")
	errAgentDisconnected = errors.New("agent disconnected")
	errAgentSendTimeout  = errors.New("agent send buffer full")
)

const agentSendTimeout = 5 * time.Second

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
		"kind":   "skill.stop",
		"run_id": runID,
		"reason": reason,
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
		"kind":      "skill.stdin",
		"run_id":    runID,
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
		"kind":   "skill.resize",
		"run_id": runID,
		"rows":   rows,
		"cols":   cols,
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
