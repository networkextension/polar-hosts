package hosts

// agent_ws_handlers.go — Phase 4a/4b: /ws/agent WebSocket endpoint.
// Phase 4a: basic WS plumbing + skill dispatch pumps.
// Phase 4b: bot_id wiring + tool_result/chat_reply frame handling so dock
//           can forward AI-loop dispatches via the dispatch API endpoints.

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var agentWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleAgentWS accepts a polar-agent WebSocket connection.
// Auth: raw agent bearer token, via ?token= or Authorization: Bearer.
// Token is verified by calling dock's /internal/v1/agent-tokens/verify
// over the HMAC-authenticated plugin SDK channel.
func (p *Plugin) handleAgentWS(c *gin.Context) {
	raw := strings.TrimSpace(c.Query("token"))
	if raw == "" {
		auth := c.GetHeader("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			raw = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if raw == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing agent token"})
		return
	}

	tokenID, userID, err := p.verifyAgentToken(raw)
	if err != nil {
		log.Printf("[agent ws] token verify RPC failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if tokenID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent token invalid or revoked"})
		return
	}

	botUserID := strings.TrimSpace(c.Query("bot_id"))
	agentID := strings.TrimSpace(c.Query("agent_id"))
	workdir := strings.TrimSpace(c.Query("workdir"))

	// Resolve host for this token. A token without a hosts row is allowed
	// (legacy path before /api/hosts/register); hostID is updated lazily
	// when the agent sends skill.advertise.
	hostID := ""
	if host, err := p.getHostByAgentToken(tokenID); err == nil && host != nil {
		hostID = host.ID
	}

	conn, err := agentWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[agent ws] upgrade failed token=%s: %v", tokenID, err)
		return
	}

	ac := &agentConn{
		tokenID:            tokenID,
		userID:             userID,
		botUserID:          botUserID,
		agentID:            agentID,
		hostID:             hostID,
		workdir:            workdir,
		send:               make(chan []byte, 64),
		close:              make(chan struct{}),
		pending:            map[string]chan agentToolResult{},
		chatPending:        map[string]chan agentChatReply{},
		skillPending:       map[int64]chan skillEventFrame{},
		skillStdoutPending: map[int64]chan []byte{},
	}
	if displaced := p.agentHub.register(ac); displaced != nil {
		log.Printf("[agent ws] kicking previous connection token=%s bot=%s", tokenID, botUserID)
		close(displaced.close)
	}
	log.Printf("[agent ws] connected token=%s bot=%s agent=%s host=%s", tokenID, botUserID, agentID, hostID)

	go p.runAgentWritePump(conn, ac)
	p.runAgentReadPump(conn, ac)
}

// verifyAgentToken calls dock's internal RPC to resolve a raw bearer token.
func (p *Plugin) verifyAgentToken(raw string) (tokenID, userID string, err error) {
	body, _ := json.Marshal(map[string]string{"token": raw})
	resp, err := p.Dock.Do(http.MethodPost, "/internal/v1/agent-tokens/verify", body)
	if err != nil {
		return "", "", err
	}
	var result struct {
		Valid   bool   `json:"valid"`
		TokenID string `json:"token_id"`
		UserID  string `json:"user_id"`
	}
	if err := readJSON(resp, &result); err != nil {
		return "", "", err
	}
	if !result.Valid {
		return "", "", nil
	}
	return result.TokenID, result.UserID, nil
}

func (p *Plugin) runAgentWritePump(conn *websocket.Conn, ac *agentConn) {
	pingT := time.NewTicker(30 * time.Second)
	defer pingT.Stop()
	for {
		select {
		case msg, ok := <-ac.send:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-pingT.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-ac.close:
			return
		}
	}
}

func (p *Plugin) runAgentReadPump(conn *websocket.Conn, ac *agentConn) {
	defer func() {
		p.agentHub.unregister(ac)
		select {
		case <-ac.close:
		default:
			close(ac.close)
		}
		_ = conn.Close()
		log.Printf("[agent ws] disconnected token=%s bot=%s host=%s", ac.tokenID, ac.botUserID, ac.hostID)
	}()

	conn.SetReadLimit(8 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		if ac.hostID != "" {
			_ = p.touchHostLastSeen(ac.hostID, time.Now().UTC())
		}
		return nil
	})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			log.Printf("[agent ws] bad message token=%s: %v", ac.tokenID, err)
			continue
		}

		switch head.Kind {
		case "hello":
			var h struct {
				Capabilities []string `json:"capabilities"`
				Tool         string   `json:"tool"`
				Workdir      string   `json:"workdir,omitempty"`
			}
			if err := json.Unmarshal(raw, &h); err == nil {
				ac.capabilities = h.Capabilities
				if h.Tool != "" {
					ac.tool = strings.TrimSpace(h.Tool)
				}
				if h.Workdir != "" && ac.workdir == "" {
					ac.workdir = strings.TrimSpace(h.Workdir)
				}
				log.Printf("[agent ws] hello token=%s bot=%s caps=%v tool=%q", ac.tokenID, ac.botUserID, h.Capabilities, h.Tool)
			}
			// Lazily resolve hostID on hello if not already set.
			if ac.hostID == "" {
				if host, herr := p.getHostByAgentToken(ac.tokenID); herr == nil && host != nil {
					ac.hostID = host.ID
				}
			}

		case "tool_result":
			var r agentToolResult
			if err := json.Unmarshal(raw, &r); err != nil {
				log.Printf("[agent ws] tool_result parse failed token=%s: %v", ac.tokenID, err)
				continue
			}
			ac.deliverResult(r)

		case "chat_reply":
			var r agentChatReply
			if err := json.Unmarshal(raw, &r); err != nil {
				log.Printf("[agent ws] chat_reply parse failed token=%s: %v", ac.tokenID, err)
				continue
			}
			ac.deliverChatReply(r)

		case "skill.advertise":
			var msg struct {
				Skills []AdvertisedSkill `json:"skills"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				log.Printf("[agent ws] skill.advertise parse failed token=%s: %v", ac.tokenID, err)
				continue
			}
			if ac.hostID == "" {
				host, herr := p.getHostByAgentToken(ac.tokenID)
				if herr != nil || host == nil {
					log.Printf("[agent ws] skill.advertise token=%s has no host row, dropping %d skills", ac.tokenID, len(msg.Skills))
					continue
				}
				ac.hostID = host.ID
			}
			now := time.Now().UTC()
			remoteIP := c2ip(conn.RemoteAddr().String())
			if err := p.updateHostAdvertisedSkills(ac.hostID, msg.Skills, remoteIP, now); err != nil {
				log.Printf("[agent ws] persist advertised skills host=%s: %v", ac.hostID, err)
				continue
			}
			for _, sk := range msg.Skills {
				if _, err := p.ensureHostSkillFromAdvertise(ac.hostID, sk, now); err != nil {
					log.Printf("[agent ws] ensure host_skill host=%s kind=%s: %v", ac.hostID, sk.Kind, err)
				}
			}
			log.Printf("[agent ws] skill.advertise host=%s skills=%d", ac.hostID, len(msg.Skills))

		case "skill.event":
			var ev skillEventFrame
			if err := json.Unmarshal(raw, &ev); err != nil {
				log.Printf("[agent ws] skill.event parse failed token=%s: %v", ac.tokenID, err)
				continue
			}
			if ev.EventKind == "stdout" {
				if b64, _ := ev.Data["bytes_b64"].(string); b64 != "" {
					chunk, err := base64.StdEncoding.DecodeString(b64)
					if err == nil {
						ac.deliverSkillStdout(ev.RunID, chunk)
					}
				}
				continue
			}
			ac.deliverSkillEvent(ev)

		default:
			log.Printf("[agent ws] unknown kind=%s token=%s", head.Kind, ac.tokenID)
		}
	}
}

// c2ip extracts the IP part from a "host:port" remote address string.
func c2ip(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}
