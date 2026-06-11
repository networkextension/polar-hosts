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

// agentConn / agentHub / skillEventFrame / skillStartEnvelope live in
// agent_hub.go (Phase 4a). Stub removed.
