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
	if hostID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "host_id required"})
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
