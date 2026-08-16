// Package hosts is the polar-agent host management plugin (hosts,
// host_skills, host_skill_runs, host_skill_credentials, console_layouts).
//
// agent_* tables (agent_tokens / agent_hub WS) STAY in dock and are
// reached via SDK; see stubs.go for the cross-domain shims.
//
// Cross-domain user/team/bot lookups go through dock SDK. Notably:
//   - bot_users.host_skill_id is a reverse FK FROM dock TO this
//     plugin's host_skills(id); the cutover PR drops dock's FK and
//     leaves it as a TEXT pointer there (lookups via this plugin's
//     own future /internal/v1/host-skills/:id endpoint).
package hosts

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/networkextension/polar-sdk"
)

type Plugin struct {
	// createdByCache: host_id → user_id credited on auto-created host_skills
	// rows (see resolveHostCreatedBy). sync.Map because touch/advertise is
	// concurrent per host.
	createdByCache sync.Map
	DB             *sql.DB
	Dock           *sdk.Client
	Name           string
	Listen         string
	Ver            string
	BlobDir        string // $POLAR_HOSTS_BLOB_DIR — agent install scripts, skill logs
	MetricsTok     string

	// SystemWorkspaceID is dock's "system" user's personal team.
	// Pre-fetched once at New() via /internal/v1/users/system/workspace
	// so the hot path doesn't hit dock per request. Local-bootstrap +
	// agent-token issuance both stamp this for the workspace owner.
	SystemWorkspaceID string

	// polarCredentialKey — AES-256-GCM key for host_skill_credentials.
	// Sourced from $HOSTS_SKILL_KEY (hex). Empty/wrong-length means
	// "store plaintext" + UI badge flips to "encryption inactive".
	polarCredentialKey []byte

	// polarAllowLocalBootstrap — P1e: POST /api/hosts/local-bootstrap
	// returns an agent token without auth. Default false.
	polarAllowLocalBootstrap bool

	// polarDisableShellSkill / polarDisableVNCSkill — operator escape
	// hatches. When true the corresponding POST .../open endpoints
	// 503 instead of dispatching to polar-agent.
	polarDisableShellSkill bool
	polarDisableVNCSkill   bool

	// shellSessions / vncSessions — in-process registries of currently
	// open shell/VNC channels keyed by run_id. Backed by host_skill_runs
	// rows for persistence; the in-process map is the live operator view.
	shellSessions *shellSessionRegistry
	vncSessions   *vncSessionRegistry

	// agentHub — Phase 4a: real WS hub. See agent_hub.go.
	agentHub *agentHub

	metrics   *hostsMetrics
	startedAt time.Time
}

type Config struct {
	DBDSN                    string
	DockBase                 string
	PluginName               string
	PluginToken              string
	Listen                   string
	BuildVersion             string
	BlobDir                  string
	MetricsToken             string
	SkillKeyHex              string // $HOSTS_SKILL_KEY — 64 hex chars (32 bytes)
	PolarAllowLocalBootstrap bool
	PolarDisableShellSkill   bool
	PolarDisableVNCSkill     bool
}

// Default per-host + per-process caps for shell/VNC. Mirror dock's
// app.go constants; tweak in follow-up if metrics warrant.
const (
	globalShellMax = 64
	globalVNCMax   = 16
)

func New(ctx context.Context, cfg Config) (*Plugin, error) {
	cfg.PluginName = strings.TrimSpace(cfg.PluginName)
	if cfg.PluginName == "" {
		cfg.PluginName = "hosts"
	}
	if strings.TrimSpace(cfg.DBDSN) == "" {
		return nil, errors.New("hosts.New: DBDSN required")
	}
	if strings.TrimSpace(cfg.DockBase) == "" {
		return nil, errors.New("hosts.New: DockBase required")
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		return nil, errors.New("hosts.New: PluginToken required")
	}
	if strings.TrimSpace(cfg.BlobDir) == "" {
		return nil, errors.New("hosts.New: BlobDir required")
	}

	db, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("open polar_hosts: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping polar_hosts: %w", err)
	}

	if err := os.MkdirAll(cfg.BlobDir, 0o755); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mkdir blob_dir: %w", err)
	}

	dock := sdk.NewClient(cfg.DockBase, cfg.PluginName, sdk.DeriveHMACKey(cfg.PluginToken))
	resp, err := dock.Do(http.MethodGet, "/internal/v1/ping", nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dock ping: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = db.Close()
		return nil, fmt.Errorf("dock /ping rejected: HTTP %d", resp.StatusCode)
	}

	// Pre-fetch the system workspace ID. host_skill rows are stamped
	// with this for platform-wide resources (the local-bootstrap path
	// creates a host owned by the system user). If dock hasn't
	// bootstrapped the system user yet (cold box), log + continue
	// with empty so the first write 500s cleanly.
	sysResp, err := dock.Do(http.MethodGet, "/internal/v1/users/system/workspace", nil)
	systemWorkspaceID := ""
	if err == nil {
		var body struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := readJSON(sysResp, &body); err == nil {
			systemWorkspaceID = body.WorkspaceID
		}
	}
	if systemWorkspaceID == "" {
		log.Printf("hosts: WARN system workspace lookup failed; local-bootstrap will fail until dock bootstraps system user (err=%v)", err)
	}

	var credentialKey []byte
	if strings.TrimSpace(cfg.SkillKeyHex) != "" {
		k, decErr := hex.DecodeString(strings.TrimSpace(cfg.SkillKeyHex))
		if decErr != nil {
			log.Printf("hosts: WARN HOSTS_SKILL_KEY not valid hex: %v — credentials stored in plaintext", decErr)
		} else if len(k) != iosdistResourceKeyBytes {
			log.Printf("hosts: WARN HOSTS_SKILL_KEY length=%d want=%d — credentials stored in plaintext", len(k), iosdistResourceKeyBytes)
		} else {
			credentialKey = k
		}
	} else {
		log.Printf("hosts: HOSTS_SKILL_KEY not set — host_skill_credentials stored in plaintext")
	}

	return &Plugin{
		DB:                       db,
		Dock:                     dock,
		Name:                     cfg.PluginName,
		Listen:                   cfg.Listen,
		Ver:                      cfg.BuildVersion,
		BlobDir:                  cfg.BlobDir,
		MetricsTok:               cfg.MetricsToken,
		SystemWorkspaceID:        systemWorkspaceID,
		polarCredentialKey:       credentialKey,
		polarAllowLocalBootstrap: cfg.PolarAllowLocalBootstrap,
		polarDisableShellSkill:   cfg.PolarDisableShellSkill,
		polarDisableVNCSkill:     cfg.PolarDisableVNCSkill,
		shellSessions:            newShellSessionRegistry(globalShellMax),
		vncSessions:              newVNCSessionRegistry(globalVNCMax),
		agentHub:                 newAgentHub(),
		metrics:                  newHostsMetrics(),
		startedAt:                time.Now(),
	}, nil
}

// readJSON — minimal JSON unmarshaller; SDK has the same helper but
// it's unexported. Local copy avoids the import-cycle gymnastics.
func readJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	return dec.Decode(out)
}

func (p *Plugin) RegisterRoutes(r gin.IRouter) {
	r.GET("/healthz", p.handleHealthz)
	r.GET("/metrics", p.handleMetricsExposition)

	// Phase 4a: agent WebSocket endpoint. No auth middleware — bears its
	// own token auth (verified via dock /internal/v1/agent-tokens/verify).
	// nginx still routes /ws/agent → dock until Phase 4b cutover.
	r.GET("/ws/agent", p.handleAgentWS)

	// Phase 4c: browser ↔ agent shell/VNC byte bridges. Auth is via the
	// same requireAuthViaDock middleware used by the /api/* routes — it
	// runs before the WS upgrade so standard Bearer + workspace headers work.
	r.GET("/ws/host/:id/shell/:run_id", p.requireAuthViaDock(), p.handleHostShellWS)
	r.GET("/ws/host/:id/vnc/:run_id", p.requireAuthViaDock(), p.handleHostVNCWS)

	// Dock-to-plugin internal API. Loopback-only auth — dock dials
	// the plugin svc directly on 127.0.0.1 and posts skill.advertise
	// touches here when the WS agent_hub sees an advertise frame.
	// See internal_touch.go.
	r.POST("/internal/v1/hosts/touch", p.handleInternalHostTouch)
	r.POST("/internal/v1/hosts/hello", p.handleInternalHostsHello)
	// polar-cloud: machine-minted enrollment tokens for VMs that must
	// self-register on first boot. Loopback-only. See internal_enroll.go.
	r.POST("/internal/v1/hosts/enroll", p.handleInternalHostsEnroll)
	// polar-cloud: host liveness/agents by id or (workspace,name). Loopback-only.
	r.GET("/internal/v1/hosts/lookup", p.handleInternalHostLookup)
	r.GET("/internal/v1/skill-catalog/:id", p.handleInternalSkillCatalogGet)

	// Phase 4b: dock calls these after the /ws/agent nginx cutover so the
	// AI chat loop (tool calls, passthrough, research) can reach agents
	// connected to polar-hosts. Loopback-only — no HMAC required.
	r.GET("/internal/v1/agents/presence", p.handleInternalAgentPresence)
	r.POST("/internal/v1/agents/dispatch/tool-call", p.handleInternalAgentDispatchToolCall)
	r.POST("/internal/v1/agents/dispatch/chat-message", p.handleInternalAgentDispatchChatMessage)
	r.POST("/internal/v1/agents/dispatch/frame", p.handleInternalAgentDispatchFrame)
	r.POST("/internal/v1/agents/kick", p.handleInternalAgentKick)

	// Mirror dock's /api/hosts/* + /api/host-skills/* + /api/console/*
	// routes so nginx can flip with a single proxy_pass redirect
	// (see scripts/nginx/hosts-svc-snippet.conf).
	api := r.Group("/api")
	{
		// Local-bootstrap is its own anon endpoint guarded by env flag.
		api.POST("/hosts/local-bootstrap", p.handleHostLocalBootstrap)
		// Bearer = enroll token (NOT session); no AuthMiddleware.
		api.POST("/hosts/register", p.handleHostRegister)

		auth := api.Group("", p.requireAuthViaDock())
		{
			auth.POST("/hosts/enroll", p.handleHostEnroll)
			auth.GET("/hosts", p.handleHostList)
			auth.GET("/hosts/topology", p.handleHostsTopology)
			auth.GET("/hosts/:id", p.handleHostDetail)
			auth.DELETE("/hosts/:id", p.handleHostDelete)

			// Agent Identity v4 read endpoints. See
			// doc/arch/agent-identity-v4.md + agents_handlers.go.
			// Workspace-member auth (mutations e.g. kick live on dock
			// because they need the in-memory agent_hub registry).
			auth.GET("/agents", p.handleListAgents)
			auth.GET("/agents/:id", p.handleGetAgent)
			auth.GET("/hosts/:id/agents", p.handleListAgentsByHost)

			auth.GET("/host-skills", p.handleHostSkillsListWorkspace)
			auth.GET("/hosts/:id/skills", p.handleHostSkillsListForHost)
			auth.PATCH("/hosts/:id/skills/:skillId", p.handleHostSkillRename)

			auth.GET("/hosts/:id/skills/:skillId/credentials", p.handleHostSkillCredentialsList)
			auth.PUT("/hosts/:id/skills/:skillId/credentials/:key", p.handleHostSkillCredentialsPut)
			auth.DELETE("/hosts/:id/skills/:skillId/credentials/:key", p.handleHostSkillCredentialsDelete)

			auth.POST("/hosts/:id/skills/:skillId/shell/open", p.handleHostShellOpen)
			auth.GET("/hosts/:id/skills/shell/sessions", p.handleHostShellSessions)
			auth.DELETE("/hosts/:id/skills/shell/sessions/:run_id", p.handleHostShellKick)

			auth.POST("/hosts/:id/skills/:skillId/vnc/open", p.handleHostVNCOpen)
			auth.GET("/hosts/:id/skills/vnc/sessions", p.handleHostVNCSessions)
			auth.DELETE("/hosts/:id/skills/vnc/sessions/:run_id", p.handleHostVNCKick)

			auth.GET("/console/layouts", p.handleConsoleLayoutList)
			auth.POST("/console/layouts", p.handleConsoleLayoutCreate)
			auth.PATCH("/console/layouts/:layoutId", p.handleConsoleLayoutUpdate)
			auth.DELETE("/console/layouts/:layoutId", p.handleConsoleLayoutDelete)

			// P0b: skill catalog. List + detail open to any workspace
			// member; create + retire require admin (checked in handler).
			auth.GET("/skill-catalog", p.handleSkillCatalogList)
			auth.POST("/skill-catalog", p.handleSkillCatalogCreate)
			auth.GET("/skill-catalog/:id", p.handleSkillCatalogGet)
			auth.POST("/skill-catalog/:id/retire", p.handleSkillCatalogRetire)
			auth.GET("/skill-catalog/:id/download", p.handleSkillCatalogDownload)
		}
	}
}

func (p *Plugin) Start(ctx context.Context) {
	go p.heartbeatLoop(ctx)
}

func (p *Plugin) Close() error {
	if p.DB != nil {
		return p.DB.Close()
	}
	return nil
}

func (p *Plugin) handleHealthz(c *gin.Context) {
	dbOK := true
	if err := p.DB.PingContext(c.Request.Context()); err != nil {
		dbOK = false
	}
	status := http.StatusOK
	if !dbOK {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{
		"plugin":         p.Name,
		"version":        p.Ver,
		"uptime_seconds": int64(time.Since(p.startedAt).Seconds()),
		"db_ok":          dbOK,
		"blob_dir":       p.BlobDir,
		"go":             runtime.Version(),
	})
}

func (p *Plugin) handleMetricsExposition(c *gin.Context) {
	if p.MetricsTok == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if c.GetHeader("Authorization") != "Bearer "+p.MetricsTok {
		c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	promhttp.HandlerFor(p.metrics.registry, promhttp.HandlerOpts{}).ServeHTTP(c.Writer, c.Request)
}

func (p *Plugin) heartbeatLoop(ctx context.Context) {
	p.beat(ctx)
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.beat(ctx)
		}
	}
}

// hostsUIRoutes — sidebar entries this plugin contributes. Heartbeated
// up to dock; aggregated into /api/plugin-ui-routes for polar-dock-ui's
// dynamic sidebar. See task #196.
var hostsUIRoutes = []sdk.UIRoute{
	{Path: "/hosts.html", Label: "主机", Icon: "server", Order: 30},
}

func (p *Plugin) beat(_ context.Context) {
	err := p.Dock.Heartbeat(sdk.HeartbeatOpts{
		Version:       p.Ver,
		Endpoint:      p.Listen,
		UptimeSeconds: int64(time.Since(p.startedAt).Seconds()),
		UIRoutes:      hostsUIRoutes,
	})
	if err != nil {
		log.Printf("hosts: heartbeat failed: %v", err)
	}
}

type hostsMetrics struct {
	registry *prometheus.Registry
	upGauge  prometheus.Gauge
}

func newHostsMetrics() *hostsMetrics {
	m := &hostsMetrics{registry: prometheus.NewRegistry()}
	m.upGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "polar_hosts_up",
		Help: "Always 1 while hosts-svc is serving.",
	})
	m.registry.MustRegister(m.upGauge)
	m.upGauge.Set(1)
	return m
}
