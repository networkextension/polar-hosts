package hosts

// /internal/v1/hosts/hello — dock posts here from its WS agent_hub when
// a polar-agent (re)connects and sends a `hello` frame with a host_info
// payload (static facts: virt, memory_bytes, cpu_model, kernel, …).
//
// Deliberately a separate channel from /internal/v1/hosts/touch. Touch
// fires on every skill.advertise (cheap per-tick heartbeat); hello fires
// once per agent reconnect with the bigger static-info blob. Splitting
// them now keeps the door open for a third "dynamic metrics" channel
// (load, free mem, disk %) without overloading either of the existing
// endpoints.
//
// Auth: loopback-only, same gate as the touch endpoint. Both dock and
// polar-hosts-svc run on the same host in the standard deploy. See
// internal_touch.go for the rationale on skipping HMAC.

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type internalHelloReq struct {
	HostID      string          `json:"host_id"`
	HostInfo    json.RawMessage `json:"host_info"`
	HelloSeenAt *time.Time      `json:"hello_seen_at,omitempty"`
	// v4 additions (doc/arch/agent-identity-v4.md). When AgentID is
	// non-empty we bump agents.last_hello_at + optionally use the
	// agents row to resolve host_id (so a legacy hello with empty
	// HostID still updates the right host).
	AgentID      string  `json:"agent_id,omitempty"`
	MemPeakBytes int64   `json:"mem_peak_bytes,omitempty"`
	CPUPeakPct   float64 `json:"cpu_peak_pct,omitempty"`
}

func (p *Plugin) handleInternalHostsHello(c *gin.Context) {
	if !isLoopbackRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "loopback only"})
		return
	}
	var req internalHelloReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	hostID := strings.TrimSpace(req.HostID)
	agentID := strings.TrimSpace(req.AgentID)

	// v4 resolution: when only agent_id is supplied, look up host_id
	// from the agents table. This lets a v4 agent send a thin hello
	// keyed only on its persistent ag_<...> identity.
	if hostID == "" && agentID != "" {
		_ = p.DB.QueryRow(
			`SELECT host_id FROM agents WHERE id = $1 LIMIT 1`,
			agentID,
		).Scan(&hostID)
	}
	if hostID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "host_id or agent_id required"})
		return
	}
	// Validate the inner blob is *some* JSON. We pass it through
	// untouched so the agent-side schema can grow without a dock cut,
	// but malformed JSON would corrupt the column.
	if len(req.HostInfo) == 0 {
		req.HostInfo = json.RawMessage(`{}`)
	} else if !json.Valid(req.HostInfo) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "host_info is not valid JSON"})
		return
	}
	now := time.Now().UTC()
	if req.HelloSeenAt != nil {
		now = req.HelloSeenAt.UTC()
	}
	updated, err := p.updateHostInfo(hostID, req.HostInfo, now)
	if err != nil {
		log.Printf("internal/v1/hosts/hello: %s: %v", hostID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist failed"})
		return
	}
	// v4 — capacity peaks + agents.last_hello_at. Best-effort; the
	// hello path must NOT 500 when these UPDATE no rows.
	if req.MemPeakBytes > 0 || req.CPUPeakPct > 0 {
		if uerr := p.updateHostCapacityPeaks(hostID, req.MemPeakBytes, req.CPUPeakPct, now); uerr != nil {
			log.Printf("internal/v1/hosts/hello: capacity peak UPDATE failed for %s: %v", hostID, uerr)
		}
	}
	if agentID != "" {
		if uerr := p.updateAgentLastHelloAt(agentID, now); uerr != nil {
			log.Printf("internal/v1/hosts/hello: agents.last_hello_at UPDATE failed for %s: %v", agentID, uerr)
		}
	}
	// Unknown host_id is NOT an error — the dock forwards every hello
	// fail-soft and we don't want a legacy / pre-register agent to fill
	// the log with 500s. Surface updated=false so the caller can metric
	// on it if they care.
	c.JSON(http.StatusOK, gin.H{"ok": true, "host_id": hostID, "updated": updated})
}

// errInvalidHostInfo signals the JSON payload was not parseable.
// Currently unused (we validate with json.Valid before calling the
// store helper) — kept exported-ish so a future change that pushes
// validation into the store can return it without an enum redesign.
var errInvalidHostInfo = errors.New("host_info is not valid JSON")
