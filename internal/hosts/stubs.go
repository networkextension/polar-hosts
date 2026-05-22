package hosts

// stubs.go — placeholder shims for dock-internal helpers + types that
// the extracted handler code references. The extraction PR keeps these
// as no-ops / log-and-fail-clean stubs so the package compiles + boots;
// the follow-up PR wires real SDK calls or migrates the underlying
// helpers.
//
// Each block names which dock function/type/field it stubs and what
// the real wiring will look like.

import (
	"errors"
	"log"
	"sync"
	"time"
)

// ---- iosdist crypto constants -----------------------------------------
//
// host_skill_credentials_store.go uses iosdistResourceKeyBytes (the AES
// key length) — defined in dock's iosdist_crypto.go. Local copy keeps
// crypto behavior identical.
const iosdistResourceKeyBytes = 32

// ---- AgentToken / AgentCoderConfig ------------------------------------
//
// hosts module references these types via createAgentToken (used in
// host_local_bootstrap_handler). The real implementations live in
// dock's agent_store.go; agent_* stays in dock. We keep the shape so
// the extracted handler compiles; the stub createAgentToken below
// returns a clean error until the SDK gets an AgentTokenCreate surface.

type AgentCoderEntry struct {
	Mode string `json:"mode,omitempty"`
}

type AgentCoderConfig struct {
	Kimi   AgentCoderEntry `json:"kimi"`
	Claude AgentCoderEntry `json:"claude"`
	Codex  AgentCoderEntry `json:"codex"`
}

type AgentToken struct {
	ID             string           `json:"id"`
	UserID         string           `json:"user_id"`
	Name           string           `json:"name"`
	CoderConfig    AgentCoderConfig `json:"coder_config"`
	HostOS         string           `json:"host_os"`
	HostArch       string           `json:"host_arch"`
	HostName       string           `json:"host_name"`
	HostIP         string           `json:"host_ip"`
	LastAttachedAt *time.Time       `json:"last_attached_at,omitempty"`
	LastUsedAt     *time.Time       `json:"last_used_at,omitempty"`
	RevokedAt      *time.Time       `json:"revoked_at,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

// createAgentToken — was dock's agent_store.go method. Issues a raw
// token + persists hash to dock's agent_tokens table. Since agent_*
// stays in dock, the extracted local-bootstrap handler can't write
// directly. TODO(extract): add SDK AgentTokenCreate surface.
// Returns ErrNotWired so the handler 500s cleanly.
func (p *Plugin) createAgentToken(userID, name string, coders AgentCoderConfig) (string, *AgentToken, error) {
	log.Printf("hosts: TODO(extract) createAgentToken user=%s name=%q — SDK has no agent-token surface, returning ErrNotWired", userID, name)
	return "", nil, errors.New("hosts: createAgentToken not wired (agent_tokens lives in dock; needs SDK surface)")
}

// ---- agentConn / agentHub / skillEventFrame ---------------------------
//
// The WS dispatch path lives in dock's agent_hub.go — polar-agent
// attaches to dock via /api/agent/attach, and dock holds the live
// connection. Shell/VNC supervisors here originally walked that
// in-process connection. After extraction, the connection is on the
// other side of an HTTP boundary; the proper fix is a WS proxy or
// SDK SkillDispatch endpoint.
//
// For the build-green PR we stub agentConn as an opaque struct with
// the fields the supervisor reads (close chan) and stub the hub to
// log + return nil/ErrNotWired. The supervisor still runs; it just
// short-circuits with a 503-equivalent log line.
//
// TODO(extract): wire `/api/agent/skill-dispatch` via SDK or proxy
// the WS frames through dock. See polar-hosts WIP-NOTES.md.

type skillEventFrame struct {
	Kind      string         `json:"kind"`
	RunID     int64          `json:"run_id"`
	EventKind string         `json:"event_kind"`
	Data      map[string]any `json:"data,omitempty"`
}

// skillStartEnvelope mirrors dock's agent_hub.go: the dispatch payload
// the agent unmarshals to launch a skill.
type skillStartEnvelope struct {
	Kind      string         `json:"kind"`
	RunID     int64          `json:"run_id"`
	SkillKind string         `json:"skill_kind"`
	Config    map[string]any `json:"config"`
}

type agentConn struct {
	tokenID   string
	userID    string
	botUserID string
	hostID    string
	close     chan struct{}

	skillPendingMu sync.Mutex
	skillPending   map[int64]chan skillEventFrame

	skillStdoutMu sync.Mutex
	skillStdout   map[int64]chan []byte
}

func (c *agentConn) trackSkillPending(runID int64) chan skillEventFrame {
	c.skillPendingMu.Lock()
	defer c.skillPendingMu.Unlock()
	if c.skillPending == nil {
		c.skillPending = map[int64]chan skillEventFrame{}
	}
	ch := make(chan skillEventFrame, 16)
	c.skillPending[runID] = ch
	return ch
}

func (c *agentConn) untrackSkillPending(runID int64) {
	c.skillPendingMu.Lock()
	defer c.skillPendingMu.Unlock()
	delete(c.skillPending, runID)
}

func (c *agentConn) trackSkillStdout(runID int64) chan []byte {
	c.skillStdoutMu.Lock()
	defer c.skillStdoutMu.Unlock()
	if c.skillStdout == nil {
		c.skillStdout = map[int64]chan []byte{}
	}
	ch := make(chan []byte, 256)
	c.skillStdout[runID] = ch
	return ch
}

func (c *agentConn) untrackSkillStdout(runID int64) {
	c.skillStdoutMu.Lock()
	defer c.skillStdoutMu.Unlock()
	delete(c.skillStdout, runID)
}

// agentHubStub stands in for dock's agentHub. lookupByHostID returns
// nil (offline) — the handlers all guard on nil so this keeps them
// fail-clean rather than dispatching into a nil pointer.
// dispatchSkillStart/Stop/Stdin/Resize log + return ErrNotWired.
type agentHubStub struct{}

func (h *agentHubStub) lookupByHostID(hostID string) *agentConn {
	log.Printf("hosts: TODO(extract) agentHub.lookupByHostID host=%s — WS hub stays in dock; returning nil (treated as offline)", hostID)
	return nil
}

func (h *agentHubStub) dispatchSkillStart(conn *agentConn, env skillStartEnvelope) error {
	log.Printf("hosts: TODO(extract) dispatchSkillStart run=%d kind=%s — WS path not wired", env.RunID, env.SkillKind)
	return errors.New("hosts: dispatchSkillStart not wired (agent_hub stays in dock; needs SDK SkillDispatch surface)")
}

func (h *agentHubStub) dispatchSkillStop(conn *agentConn, runID int64, reason string) error {
	log.Printf("hosts: TODO(extract) dispatchSkillStop run=%d reason=%s — WS path not wired", runID, reason)
	return errors.New("hosts: dispatchSkillStop not wired")
}

func (h *agentHubStub) dispatchSkillStdin(conn *agentConn, runID int64, payload []byte) error {
	log.Printf("hosts: TODO(extract) dispatchSkillStdin run=%d bytes=%d — WS path not wired", runID, len(payload))
	return errors.New("hosts: dispatchSkillStdin not wired")
}

func (h *agentHubStub) dispatchSkillResize(conn *agentConn, runID int64, rows, cols uint16) error {
	log.Printf("hosts: TODO(extract) dispatchSkillResize run=%d rows=%d cols=%d — WS path not wired", runID, rows, cols)
	return errors.New("hosts: dispatchSkillResize not wired")
}
