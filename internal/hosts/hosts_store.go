package hosts

// Host module — Phase 0 store helpers.
//
// A Host is a named machine running polar-agent. It's workspace-scoped
// and linked 1:1 to an agent_tokens row (which holds the bearer auth).
// P0 only exercises the hosts table; host_skills + host_skill_runs are
// declared in applySchema for forward-compat with later phases but no
// helper here writes to them yet.
//
// Enrollment flow:
//   1. Admin calls POST /api/hosts/enroll → createEnrollmentToken()
//      mints a one-time bearer string + an agent_tokens row marked as
//      pending (coder_config={"pending_enrollment":true}). TTL is 1
//      hour, enforced by the consume step.
//   2. Operator runs polar-agent with that token → POST /api/hosts/
//      register → consumeEnrollmentToken() validates + flips the row
//      out of pending state, createHost() inserts the hosts row.
//   3. Agent attaches via WS → getHostByAgentToken() resolves host_id
//      and we cache it on agentConn (see agent_hub.go).
//
// Slug is auto-generated from name at create time; if a collision
// happens we append -2, -3, etc. inside the workspace.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	sdk "github.com/networkextension/polar-sdk"
)

// Host is the API + DB representation of a registered remote machine.
//
// v4 (doc/arch/agent-identity-v4.md): the MachineUUID field is gone.
// New rows' ID is sha256(salt + raw_uuid)[:32] minted by dock from the
// AgentRegister call; legacy rows keep their h_<random> PK. The
// agents table provides the logical instance layer with its own
// ag_<random32hex> identity.
type Host struct {
	ID                   string          `json:"id"`
	WorkspaceID          string          `json:"workspace_id"`
	Slug                 string          `json:"slug"`
	Name                 string          `json:"name"`
	AgentTokenID         *string         `json:"agent_token_id,omitempty"`
	OS                   string          `json:"os"`
	Arch                 string          `json:"arch"`
	LastSeenIP           string          `json:"last_seen_ip,omitempty"`
	AdvertisedSkillsJSON json.RawMessage `json:"advertised_skills,omitempty"`
	LastSeenAt           *time.Time      `json:"last_seen_at,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	// HostInfo is the static-fact blob the agent pushes once per
	// reconnect via /internal/v1/hosts/hello. Schema is agent-owned and
	// intentionally untyped here so adding fields like "virt" or
	// "cpu_model" doesn't require a dock cut. Empty object until the
	// agent has been upgraded to a build that ships the hello payload.
	HostInfo        map[string]any `json:"host_info,omitempty"`
	HostInfoSeenAt  *time.Time     `json:"host_info_seen_at,omitempty"`
	// v4 capacity peaks — agent samples since last hello, ratcheted up
	// via GREATEST in updateHostCapacityPeaks. Nullable in DB; surface as
	// pointers so UI can show "—" for never-sampled rows.
	MemPeakBytes *int64   `json:"mem_peak_bytes,omitempty"`
	CPUPeakPct   *float64 `json:"cpu_peak_pct,omitempty"`
	// v4 derived count — number of agents rows pointing at this host.
	// Populated on /api/hosts (LEFT JOIN COUNT); 0 for hosts pre-agent
	// model or single-host scans that skip the join.
	AgentsCount int `json:"agents_count"`
}

// Agent is the v4 logical-instance row (one host : N agents).
// agent.toml on the polar-agent box persists ID and reuses it on
// every reconnect.
type Agent struct {
	ID           string     `json:"id"`             // ag_<random32hex>
	WorkspaceID  string     `json:"workspace_id"`
	HostID       string     `json:"host_id"`
	Name         string     `json:"name"`
	BotUserID    string     `json:"bot_user_id,omitempty"`
	AgentTokenID string     `json:"agent_token_id"`
	OS           string     `json:"os,omitempty"`
	Arch         string     `json:"arch,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	LastHelloAt  *time.Time `json:"last_hello_at,omitempty"`
}

// AgentListItem is the row shape returned by /api/agents +
// /api/agents/:id + /api/hosts/:id/agents. Joined with hosts so the
// UI can render "agent on host <name>" without a fan-out call.
//
// Wire-shape contract (Phase F UI relies on these exact names):
//
//   - agent_token_id_suffix is the LAST 8 chars of agent_token_id
//     (the opaque row id, NOT the raw bearer). The raw bearer is shown
//     once at register time and only the sha256 hash is persisted; the
//     suffix here is a stable display fingerprint that does NOT leak
//     the bearer or enable replay.
//
//   - host_name is hosts.name (operator-chosen label); host_hw_model
//     is extracted from hosts.host_info_json (the agent hello payload).
//     Either may be empty for a never-said-hello host.
type AgentListItem struct {
	ID                   string  `json:"id"`
	WorkspaceID          string  `json:"workspace_id"`
	HostID               string  `json:"host_id"`
	HostName             string  `json:"host_name,omitempty"`
	HostHWModel          string  `json:"host_hw_model,omitempty"`
	Name                 string  `json:"name"`
	BotUserID            string  `json:"bot_user_id,omitempty"`
	AgentTokenIDSuffix   string  `json:"agent_token_id_suffix"`
	OS                   string  `json:"os,omitempty"`
	Arch                 string  `json:"arch,omitempty"`
	CreatedAt            string  `json:"created_at"`
	LastHelloAt          *string `json:"last_hello_at,omitempty"`
}

// AdvertisedSkill mirrors the agent-side skill descriptor the WS
// handler persists via updateHostAdvertisedSkills. Kept as a struct
// (rather than json.RawMessage) so callers can introspect without
// re-parsing.
type AdvertisedSkill struct {
	Kind         string         `json:"kind"`
	Version      string         `json:"version,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
	// P1a additions — agent sends these when the skill came from a
	// bundle installed via skill.install. Empty for compiled-in skills.
	Publisher string `json:"publisher,omitempty"`
	InstallID string `json:"install_id,omitempty"`
	Source    string `json:"source,omitempty"` // "builtin" | "bundle"
}

// HostSkill is the persistent counterpart to AdvertisedSkill — the
// host_skills row materialized by the auto-shim when an agent
// advertises a skill kind, and later mutated by the operator via UI
// (enable/disable, config edits).
type HostSkill struct {
	ID         int64           `json:"id"`
	HostID     string          `json:"host_id"`
	Kind       string          `json:"kind"`
	Name       string          `json:"name"`
	ConfigJSON json.RawMessage `json:"config_json"`
	Enabled    bool            `json:"enabled"`
	AutoStart  bool            `json:"auto_start"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	// P1a additions — empty for older rows that predate the bundle
	// install path (source defaults to 'builtin' via SQL DEFAULT).
	Publisher string `json:"publisher,omitempty"`
	InstallID string `json:"install_id,omitempty"`
	Source    string `json:"source,omitempty"`
}

// pendingEnrollmentMarker is the coder_config JSON shape we use to
// mark an agent_tokens row as "minted but not yet consumed by an
// agent". Cleared by consumeEnrollmentToken on first successful use.
type pendingEnrollmentMarker struct {
	PendingEnrollment bool      `json:"pending_enrollment"`
	WorkspaceID       string    `json:"workspace_id"`
	HostName          string    `json:"host_name"`
	ExpiresAt         time.Time `json:"expires_at"`
}

const enrollmentTokenTTL = time.Hour

// ---- hosts CRUD --------------------------------------------------------

// RegisterAgentInput captures the v4 register payload — collected by the
// HTTP register handler, passed to registerAgent which chains the
// dock-side SDK calls + materializes the local hosts + agents rows.
type RegisterAgentInput struct {
	WorkspaceID    string
	Name           string
	MachineUUIDRaw string
	OS             string
	Arch           string
	BotUserID      string // optional; bind to existing bot when set
	HostInfo       map[string]any
}

// RegisterAgentResult is what the register handler returns to the
// agent CLI — the raw token is shown once (agent persists in
// agent.toml + we keep only the hash).
type RegisterAgentResult struct {
	AgentID   string
	HostID    string
	BotUserID string
	TokenRaw  string
	Server    string
}

// registerAgent is the v4 register entry — see
// doc/arch/agent-identity-v4.md. polar-hosts owns the local agents +
// hosts rows; dock owns the canonical agent_token, bot_user, and
// llm_proxy_token writes (we proxy via SDK.AgentRegister).
//
// On dock success we mirror the agents row locally so plugin-side
// joins ("list hosts → agents" UI) don't have to round-trip to dock
// per request. On dock failure we surface the error — no local row
// is created so the operator sees a single clean failure.
func (p *Plugin) registerAgent(in RegisterAgentInput, now time.Time) (*RegisterAgentResult, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.WorkspaceID = strings.TrimSpace(in.WorkspaceID)
	in.MachineUUIDRaw = strings.TrimSpace(in.MachineUUIDRaw)
	if in.Name == "" || in.WorkspaceID == "" {
		return nil, errors.New("name and workspace_id required")
	}
	if in.MachineUUIDRaw == "" {
		return nil, errors.New("machine_uuid_raw required (v4)")
	}
	if p.Dock == nil {
		return nil, errors.New("dock SDK client not initialized")
	}

	// Single round-trip: dock returns agent_id, host_id, bot_user_id,
	// token, server. Dock has already INSERTed its own agents +
	// hosts rows; we mirror them locally below.
	resp, err := p.Dock.AgentRegister(sdk.AgentRegisterRequest{
		WorkspaceID:    in.WorkspaceID,
		Name:           in.Name,
		MachineUUIDRaw: in.MachineUUIDRaw,
		HostInfo:       in.HostInfo,
		OS:             in.OS,
		Arch:           in.Arch,
		BotUserID:      in.BotUserID,
	})
	if err != nil {
		return nil, fmt.Errorf("dock AgentRegister: %w", err)
	}
	if strings.TrimSpace(resp.AgentID) == "" || strings.TrimSpace(resp.HostID) == "" {
		return nil, errors.New("dock AgentRegister returned empty agent_id/host_id")
	}

	// Local hosts row mirror. Slug is local-only (URL component); we
	// use a stable host-<id-prefix> when the operator-chosen name
	// would collide.
	slug, slugErr := p.uniqueHostSlugInWorkspace(in.WorkspaceID, in.Name)
	if slugErr != nil {
		slug = "host-" + safePrefix(resp.HostID, 8)
	}
	if _, err := p.DB.Exec(
		`INSERT INTO hosts (id, workspace_id, slug, name, os, arch, last_seen_at, first_seen_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $7)
		 ON CONFLICT (id) DO UPDATE
		   SET name         = EXCLUDED.name,
		       os           = COALESCE(NULLIF(EXCLUDED.os, ''),   hosts.os),
		       arch         = COALESCE(NULLIF(EXCLUDED.arch, ''), hosts.arch),
		       last_seen_at = EXCLUDED.last_seen_at`,
		resp.HostID, in.WorkspaceID, slug, in.Name, in.OS, in.Arch, now,
	); err != nil {
		return nil, fmt.Errorf("local host upsert: %w", err)
	}

	// Local agents row mirror — agent_token row exists on dock side,
	// FK to the local agent_tokens table requires we mirror the token
	// row first. We mint a placeholder agent_tokens row with the same
	// id + hash so the FK resolves. dock-side raw token is
	// authoritative; plugin keeps only the hash.
	atID, atErr := p.mirrorDockAgentTokenForAgent(resp.HostID, in.Name, in.WorkspaceID, resp.AgentTokenRaw, now)
	if atErr != nil {
		return nil, fmt.Errorf("mirror agent_token: %w", atErr)
	}
	if _, err := p.DB.Exec(
		`INSERT INTO agents (id, workspace_id, host_id, name, bot_user_id, agent_token_id, os, arch, created_at)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, NULLIF($7, ''), NULLIF($8, ''), $9)
		 ON CONFLICT (id) DO UPDATE
		   SET host_id     = EXCLUDED.host_id,
		       bot_user_id = COALESCE(EXCLUDED.bot_user_id, agents.bot_user_id),
		       os          = COALESCE(EXCLUDED.os, agents.os),
		       arch        = COALESCE(EXCLUDED.arch, agents.arch)`,
		resp.AgentID, in.WorkspaceID, resp.HostID, in.Name, resp.BotUserID, atID,
		in.OS, in.Arch, now,
	); err != nil {
		return nil, fmt.Errorf("local agent insert: %w", err)
	}

	return &RegisterAgentResult{
		AgentID:   resp.AgentID,
		HostID:    resp.HostID,
		BotUserID: resp.BotUserID,
		TokenRaw:  resp.AgentTokenRaw,
		Server:    resp.Server,
	}, nil
}

// mirrorDockAgentTokenForAgent inserts a local agent_tokens row keyed
// off the agent name so the FK on the agents table resolves. dock owns
// the canonical row (with the raw plaintext hashed); we only store the
// hash + an opaque id. Idempotent on (token_hash).
func (p *Plugin) mirrorDockAgentTokenForAgent(hostID, agentName, workspaceID, rawToken string, now time.Time) (string, error) {
	if strings.TrimSpace(rawToken) == "" {
		// Idempotent register path — dock returned existing agent;
		// look up the existing token row by agent name pattern.
		var atID string
		err := p.DB.QueryRow(
			`SELECT agent_token_id FROM agents WHERE workspace_id = $1 AND name = $2 LIMIT 1`,
			workspaceID, agentName,
		).Scan(&atID)
		if err == nil && atID != "" {
			return atID, nil
		}
		// Fall through to a stub-row mint if the lookup miss happens.
	}
	id := generateResourceID()
	hash := ""
	if strings.TrimSpace(rawToken) != "" {
		hash = hashAgentToken(rawToken)
	} else {
		// Idempotent path: dock didn't return a token. Generate a
		// stable-but-unused stub so the FK isn't NULL.
		hash = "v4-stub-" + id
	}
	// Resolve the owner_user_id from the workspace's owner so
	// agent_tokens.user_id is populated.
	var ownerUserID string
	if err := p.DB.QueryRow(
		`SELECT u.id FROM users u WHERE u.role = 'admin' ORDER BY u.created_at LIMIT 1`,
	).Scan(&ownerUserID); err != nil {
		return "", fmt.Errorf("resolve owner: %w", err)
	}
	if _, err := p.DB.Exec(
		`INSERT INTO agent_tokens (id, user_id, name, token_hash, coder_config, created_at)
		 VALUES ($1, $2, $3, $4, '{}'::jsonb, $5)
		 ON CONFLICT (token_hash) DO NOTHING`,
		id, ownerUserID, "agent:"+agentName, hash, now,
	); err != nil {
		return "", err
	}
	// Re-fetch by hash because ON CONFLICT may have skipped the insert.
	var resolvedID string
	if err := p.DB.QueryRow(
		`SELECT id FROM agent_tokens WHERE token_hash = $1 LIMIT 1`, hash,
	).Scan(&resolvedID); err != nil {
		return "", err
	}
	_ = hostID
	return resolvedID, nil
}

// safePrefix returns the first n chars of s, or s if shorter. Used
// to derive a slug fallback from a host_id without panicking.
func safePrefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// updateAgentLastHelloAt + updateHostCapacityPeaks are called from the
// /internal/v1/hosts/hello bridge. The hello frame in v4 carries
// agent_id (when present) → we bump the agents row's last_hello_at
// and the host's mem_peak / cpu_peak via GREATEST so the columns only
// ratchet up. Empty agent_id falls through to the legacy host-only path.
func (p *Plugin) updateAgentLastHelloAt(agentID string, now time.Time) error {
	if strings.TrimSpace(agentID) == "" {
		return nil
	}
	_, err := p.DB.Exec(`UPDATE agents SET last_hello_at = $2 WHERE id = $1`, agentID, now)
	return err
}

func (p *Plugin) updateHostCapacityPeaks(hostID string, memPeakBytes int64, cpuPeakPct float64, now time.Time) error {
	if strings.TrimSpace(hostID) == "" {
		return nil
	}
	_, err := p.DB.Exec(
		`UPDATE hosts
		 SET mem_peak_bytes = GREATEST(COALESCE(mem_peak_bytes, 0), $2),
		     cpu_peak_pct   = GREATEST(COALESCE(cpu_peak_pct, 0),   $3),
		     last_seen_at   = $4
		 WHERE id = $1`,
		hostID, memPeakBytes, cpuPeakPct, now,
	)
	return err
}

func (p *Plugin) getHostByID(id string) (*Host, error) {
	return p.scanHost(p.DB.QueryRow(hostSelectColumns+` WHERE id = $1`, id))
}

func (p *Plugin) getHostBySlug(workspaceID, slug string) (*Host, error) {
	return p.scanHost(p.DB.QueryRow(hostSelectColumns+` WHERE workspace_id = $1 AND slug = $2`, workspaceID, slug))
}

// getHostByAgentToken is on the hot path of every agent WS message
// (called once at attach to cache host_id on agentConn). It tries the
// v4 chain first (agent_tokens → agents.agent_token_id → agents.host_id
// → hosts.id) and falls back to the v3 chain (hosts.agent_token_id)
// for legacy rows that haven't been migrated to v4 yet.
//
// v4-first ordering matters: hosts.agent_token_id is NULL for every
// v4-registered host (see doc/arch/agent-identity-v4.md), so a v3-only
// query silently drops every v4 agent's skill.advertise frames. The v3
// fallback covers the small number of pre-v4 hosts still in prod.
func (p *Plugin) getHostByAgentToken(agentTokenID string) (*Host, error) {
	if strings.TrimSpace(agentTokenID) == "" {
		return nil, nil
	}
	// v4: token → agents.agent_token_id → agents.host_id → hosts.id.
	// We use a scalar subquery in WHERE rather than a JOIN: hosts and
	// agents share column names (id, workspace_id, name, agent_token_id,
	// os, arch, created_at), so a JOIN against the unqualified
	// hostSelectColumns SELECT list throws "column reference is
	// ambiguous". The subquery keeps the outer SELECT a plain
	// `FROM hosts` and reuses hostSelectColumns verbatim (including its
	// own agents_count correlated subquery, which is independently
	// scoped to `FROM agents a`).
	host, err := p.scanHost(p.DB.QueryRow(
		hostSelectColumns+`
		 WHERE id = (
		   SELECT host_id FROM agents
		    WHERE agent_token_id = $1
		    ORDER BY created_at DESC
		    LIMIT 1
		 )`, agentTokenID))
	if err == nil && host != nil {
		return host, nil
	}
	// v3 fallback: token → hosts.agent_token_id
	return p.scanHost(p.DB.QueryRow(hostSelectColumns+` WHERE agent_token_id = $1 LIMIT 1`, agentTokenID))
}

func (p *Plugin) listHostsForWorkspace(workspaceID string) ([]Host, error) {
	rows, err := p.DB.Query(
		hostSelectColumns+` WHERE workspace_id = $1 ORDER BY last_seen_at DESC NULLS LAST, created_at DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Host{}
	for rows.Next() {
		h, err := p.scanHostRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

// ---- agents read helpers (v4) -----------------------------------------
//
// All three helpers materialize AgentListItem rows (joined with hosts
// for host_name + host_hw_model). The joins are on agents.host_id =
// hosts.id, backed by the idx_agents_host index + hosts(id) PK; cost is
// constant per row regardless of workspace size.
//
// agent_token_id_suffix is computed in SQL via RIGHT(at.id, 8) so the
// raw bearer never crosses the wire — at.id is the opaque row id, not
// the plaintext token. See AgentListItem doc for the security rationale.

const agentSelectColumns = `
	SELECT a.id, a.workspace_id, a.host_id,
	       COALESCE(h.name, ''),
	       COALESCE(h.host_info_json->>'hw_model', ''),
	       a.name,
	       COALESCE(a.bot_user_id, ''),
	       RIGHT(a.agent_token_id, 8),
	       COALESCE(a.os, ''),
	       COALESCE(a.arch, ''),
	       a.created_at,
	       a.last_hello_at
	  FROM agents a
	  LEFT JOIN hosts h ON h.id = a.host_id`

func (p *Plugin) scanAgentList(rows *sql.Rows) ([]AgentListItem, error) {
	defer rows.Close()
	out := []AgentListItem{}
	for rows.Next() {
		it, err := scanAgentListRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

func scanAgentListRow(row rowScanner) (*AgentListItem, error) {
	var it AgentListItem
	var lastHello sql.NullTime
	var createdAt sql.NullTime
	if err := row.Scan(
		&it.ID, &it.WorkspaceID, &it.HostID,
		&it.HostName, &it.HostHWModel,
		&it.Name, &it.BotUserID, &it.AgentTokenIDSuffix,
		&it.OS, &it.Arch,
		&createdAt, &lastHello,
	); err != nil {
		return nil, err
	}
	if createdAt.Valid {
		it.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	if lastHello.Valid {
		v := lastHello.Time.UTC().Format("2006-01-02T15:04:05Z")
		it.LastHelloAt = &v
	}
	return &it, nil
}

// listAgents returns every agents row in the workspace, ordered by
// most-recent-hello-first. Empty slice (never nil) on no match.
func (p *Plugin) listAgents(workspaceID string) ([]AgentListItem, error) {
	rows, err := p.DB.Query(
		agentSelectColumns+` WHERE a.workspace_id = $1
		 ORDER BY a.last_hello_at DESC NULLS LAST, a.created_at DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	return p.scanAgentList(rows)
}

// getAgent resolves a single agent by id. Returns (nil, sql.ErrNoRows)
// when the id is unknown so the handler can produce a clean 404.
func (p *Plugin) getAgent(agentID string) (*AgentListItem, error) {
	row := p.DB.QueryRow(agentSelectColumns+` WHERE a.id = $1`, agentID)
	return scanAgentListRow(row)
}

// listAgentsByHost returns the agents attached to one host row.
// Ordering matches listAgents so the UI sees a consistent shape.
func (p *Plugin) listAgentsByHost(hostID string) ([]AgentListItem, error) {
	rows, err := p.DB.Query(
		agentSelectColumns+` WHERE a.host_id = $1
		 ORDER BY a.last_hello_at DESC NULLS LAST, a.created_at DESC`,
		hostID,
	)
	if err != nil {
		return nil, err
	}
	return p.scanAgentList(rows)
}

// updateHostAdvertisedSkills replaces the JSON snapshot and bumps
// last_seen_at + last_seen_ip in one statement. Called from the WS
// dispatch when a skill.advertise message arrives.
func (p *Plugin) updateHostAdvertisedSkills(hostID string, skills []AdvertisedSkill, remoteIP string, now time.Time) error {
	if strings.TrimSpace(hostID) == "" {
		return errors.New("host_id is required")
	}
	if skills == nil {
		skills = []AdvertisedSkill{}
	}
	payload, err := json.Marshal(skills)
	if err != nil {
		return fmt.Errorf("marshal skills: %w", err)
	}
	_, err = p.DB.Exec(
		`UPDATE hosts
		 SET advertised_skills_json = $2,
		     last_seen_at = $3,
		     last_seen_ip = COALESCE(NULLIF($4, ''), last_seen_ip)
		 WHERE id = $1`,
		hostID, string(payload), now, strings.TrimSpace(remoteIP),
	)
	return err
}

// updateHostInfo writes the host_info_json + host_info_seen_at columns
// for one host (polar-dock#351). Idempotent — re-running with the same
// payload is a no-op observable side-effect-wise. Returns whether the
// UPDATE actually matched a row so the caller can surface "unknown
// host" without making it an error (a legacy / pre-register agent
// reconnecting MUST NOT 500 the hello).
//
// Empty / null payload is allowed and stored as '{}'::jsonb; that keeps
// the "old agent, no host_info yet" case explicit in the row rather
// than silent.
//
// Implementation note: jsonb param is passed as string(), NOT []byte —
// lib/pq sends []byte as bytea which cannot cast to jsonb. (Same
// gotcha bit polar-dock e7d80b9 on plugin_modules.ui_routes.)
//
// v4: the machine_uuid backfill side effect is gone — host identity
// now lives in the PK (sha256(salt + raw_uuid)), not a separate column.
func (p *Plugin) updateHostInfo(hostID string, hostInfoJSON []byte, seenAt time.Time) (bool, error) {
	if strings.TrimSpace(hostID) == "" {
		return false, errors.New("host_id is required")
	}
	payload := string(hostInfoJSON)
	if strings.TrimSpace(payload) == "" {
		payload = "{}"
	}
	res, err := p.DB.Exec(
		`UPDATE hosts
		 SET host_info_json    = $2::jsonb,
		     host_info_seen_at = $3
		 WHERE id = $1`,
		hostID, payload, seenAt,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ensureHostSkillFromAdvertise materializes a host_skills row for an
// agent-advertised skill kind, upserting on (host_id, kind). Returns
// the row's id so the auto-shim can later bind it to a bot via
// bot_users.host_skill_id. Called from the WS dispatch right after
// updateHostAdvertisedSkills so the dock-side host_skills table tracks
// every advertised skill kind, ready for explicit operator enable in
// the UI.
//
// created_by resolves through the host's agent_token to the enroller's
// user_id — auto-created rows are credited to the user who registered
// the host (the closest thing we have to a "system user" without
// inventing one). config_json snapshots the agent's advertised
// capabilities so the UI can show "this skill exposes tools X, Y, Z".
func (p *Plugin) ensureHostSkillFromAdvertise(hostID string, skill AdvertisedSkill, now time.Time) (int64, error) {
	if strings.TrimSpace(hostID) == "" {
		return 0, errors.New("host_id is required")
	}
	kind := strings.TrimSpace(skill.Kind)
	if kind == "" {
		return 0, errors.New("skill.kind is required")
	}
	caps := skill.Capabilities
	if caps == nil {
		caps = map[string]any{}
	}
	configJSON, err := json.Marshal(caps)
	if err != nil {
		return 0, fmt.Errorf("marshal capabilities: %w", err)
	}
	// Friendly display name auto-derived from kind. Operator can
	// rename via UI later (lands with the skill config form PR).
	name := kind

	// Resolve created_by from agent_tokens.user_id via hosts.agent_token_id.
	// If the host has no agent_token_id (defensive: shouldn't happen in
	// practice since register() always sets it), fall back to the
	// workspace owner.
	var createdBy string
	err = p.DB.QueryRow(`
		SELECT COALESCE(
			(SELECT at.user_id FROM agent_tokens at
			 JOIN hosts h ON h.agent_token_id = at.id
			 WHERE h.id = $1),
			(SELECT t.owner_user_id FROM teams t
			 JOIN hosts h ON h.workspace_id = t.id
			 WHERE h.id = $1)
		)`, hostID).Scan(&createdBy)
	if err != nil {
		return 0, fmt.Errorf("resolve created_by for host=%s: %w", hostID, err)
	}
	if createdBy == "" {
		return 0, fmt.Errorf("could not resolve a user_id for host=%s — neither agent_token nor workspace owner", hostID)
	}

	// P1a: publisher / install_id / source default to '' / '' /
	// 'builtin' for older agents that don't advertise them; bundle-
	// installed skills set all three. UPDATE clause keeps them in sync
	// when the agent re-advertises (e.g. after a fresh install bumps
	// the install_id).
	source := strings.TrimSpace(skill.Source)
	if source == "" {
		source = "builtin"
	}
	var id int64
	err = p.DB.QueryRow(`
		INSERT INTO host_skills (host_id, kind, name, config_json, publisher, install_id, source, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9, $9)
		ON CONFLICT (host_id, kind) DO UPDATE
			SET config_json = EXCLUDED.config_json,
			    publisher   = EXCLUDED.publisher,
			    install_id  = EXCLUDED.install_id,
			    source      = EXCLUDED.source,
			    updated_at  = EXCLUDED.updated_at
		RETURNING id`,
		hostID, kind, name, string(configJSON),
		strings.TrimSpace(skill.Publisher), strings.TrimSpace(skill.InstallID), source,
		createdBy, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert host_skills (host=%s, kind=%s): %w", hostID, kind, err)
	}
	return id, nil
}

// listHostSkillsForWorkspace returns every host_skills row across
// every host in a workspace. Used by the bot edit page picker (P1d)
// to enumerate "skills this bot can bind to". Joins through hosts to
// scope; FK on host_skills.host_id makes the join cheap.
func (p *Plugin) listHostSkillsForWorkspace(workspaceID string) ([]HostSkillWithHost, error) {
	rows, err := p.DB.Query(`
		SELECT hs.id, hs.host_id, hs.kind, hs.name, hs.config_json::text,
		       hs.enabled, hs.auto_start,
		       hs.created_by, hs.created_at, hs.updated_at,
		       h.slug, h.name
		FROM host_skills hs
		JOIN hosts h ON h.id = hs.host_id
		WHERE h.workspace_id = $1
		ORDER BY h.name, hs.kind`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostSkillWithHost
	for rows.Next() {
		var (
			hs         HostSkill
			configText string
			hostSlug   string
			hostName   string
		)
		if err := rows.Scan(&hs.ID, &hs.HostID, &hs.Kind, &hs.Name, &configText,
			&hs.Enabled, &hs.AutoStart, &hs.CreatedBy, &hs.CreatedAt, &hs.UpdatedAt,
			&hostSlug, &hostName); err != nil {
			return nil, err
		}
		hs.ConfigJSON = json.RawMessage(configText)
		out = append(out, HostSkillWithHost{
			HostSkill: hs,
			HostSlug:  hostSlug,
			HostName:  hostName,
		})
	}
	return out, rows.Err()
}

// HostSkillWithHost embeds a HostSkill plus a couple of denormalized
// fields from the parent host so the FE picker doesn't have to fetch
// hosts separately.
type HostSkillWithHost struct {
	HostSkill
	HostSlug string `json:"host_slug"`
	HostName string `json:"host_name"`
}

// listHostSkills returns every host_skills row for a host, ordered by
// kind. Used by the host detail UI to show "skills currently materialized
// from advertise + their config_json".
func (p *Plugin) listHostSkills(hostID string) ([]HostSkill, error) {
	rows, err := p.DB.Query(`
		SELECT id, host_id, kind, name, config_json::text, enabled, auto_start,
		       created_by, created_at, updated_at
		FROM host_skills
		WHERE host_id = $1
		ORDER BY kind`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostSkill
	for rows.Next() {
		var hs HostSkill
		var configText string
		if err := rows.Scan(&hs.ID, &hs.HostID, &hs.Kind, &hs.Name, &configText,
			&hs.Enabled, &hs.AutoStart, &hs.CreatedBy, &hs.CreatedAt, &hs.UpdatedAt); err != nil {
			return nil, err
		}
		hs.ConfigJSON = json.RawMessage(configText)
		out = append(out, hs)
	}
	return out, rows.Err()
}

// updateHostSkillName renames a host_skill row. Operator-facing alias
// for the auto-generated default name (which is the bare kind string).
// Returns sql.ErrNoRows if the row doesn't exist.
func (p *Plugin) updateHostSkillName(skillID int64, name string, now time.Time) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("name cannot be empty")
	}
	if len(trimmed) > 80 {
		return errors.New("name too long (max 80 chars)")
	}
	res, err := p.DB.Exec(
		`UPDATE host_skills SET name = $1, updated_at = $2 WHERE id = $3`,
		trimmed, now, skillID,
	)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// touchHostLastSeen is the no-skills heartbeat — called on every WS
// frame so the UI's "online N seconds ago" stays accurate without
// requiring an advertise.
func (p *Plugin) touchHostLastSeen(hostID string, now time.Time) error {
	if strings.TrimSpace(hostID) == "" {
		return nil
	}
	_, err := p.DB.Exec(`UPDATE hosts SET last_seen_at = $2 WHERE id = $1`, hostID, now)
	return err
}

func (p *Plugin) deleteHost(hostID string) error {
	_, err := p.DB.Exec(`DELETE FROM hosts WHERE id = $1`, hostID)
	return err
}

// ---- enrollment --------------------------------------------------------

// createEnrollmentToken mints a one-time-use bearer string for an
// operator-initiated "+ Add Host" flow. Stored as a normal agent_tokens
// row (so the agent WS handshake doesn't need a special code path) but
// marked pending via coder_config so consumeEnrollmentToken can
// distinguish from a fully-registered token.
//
// Task #216 (split-brain fix): after the local INSERT we dual-write to
// dock via SDK.IssueAgentToken so the canonical row also lands in
// ideamesh.agent_tokens — without that, dock's /ws/agent auth
// (resolveAgentToken) 401s on the agent's first connect. If the SDK
// call fails we roll back the local row so the two DBs stay in sync.
//
// TODO(#216-phase2): once we're confident in the dual-write path, drop
// the local agent_tokens table and have polar-hosts read tokens back
// from dock via a new SDK GET surface.
func (p *Plugin) createEnrollmentToken(userID, workspaceID, hostName string, now time.Time) (string, string, time.Time, error) {
	userID = strings.TrimSpace(userID)
	workspaceID = strings.TrimSpace(workspaceID)
	hostName = strings.TrimSpace(hostName)
	if userID == "" || workspaceID == "" || hostName == "" {
		return "", "", time.Time{}, errors.New("user_id, workspace_id, and host_name are required")
	}
	raw, hashHex, err := generateAgentToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	expiresAt := now.Add(enrollmentTokenTTL)
	marker := pendingEnrollmentMarker{
		PendingEnrollment: true,
		WorkspaceID:       workspaceID,
		HostName:          hostName,
		ExpiresAt:         expiresAt,
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		return "", "", time.Time{}, err
	}
	tokenID := generateResourceID()
	tokenName := "enroll:" + hostName
	if _, err := p.DB.Exec(
		`INSERT INTO agent_tokens (id, user_id, name, token_hash, coder_config, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tokenID, userID, tokenName, hashHex, string(markerJSON), now,
	); err != nil {
		return "", "", time.Time{}, err
	}
	// Dual-write to dock. On failure roll back the local row so the
	// operator sees one clean error rather than a half-registered token
	// that silently 401s its agent later.
	if p.Dock != nil {
		if _, derr := p.Dock.IssueAgentToken(sdk.AgentTokenIssueRequest{
			ID:              tokenID,
			UserID:          userID,
			Name:            tokenName,
			TokenHash:       hashHex,
			CoderConfigJSON: string(markerJSON),
		}); derr != nil {
			if _, rerr := p.DB.Exec(`DELETE FROM agent_tokens WHERE id = $1`, tokenID); rerr != nil {
				log.Printf("[#216] createEnrollmentToken: dock issue failed AND local rollback failed: dock_err=%v rollback_err=%v token_id=%s", derr, rerr, tokenID)
			}
			return "", "", time.Time{}, fmt.Errorf("dock issue agent_token: %w", derr)
		}
	}
	return tokenID, raw, expiresAt, nil
}

// consumeEnrollmentToken validates a pending token, returns the
// (workspace_id, host_name) the admin chose at mint time, clears the
// pending marker, and returns the (tokenID, userID) so the caller can
// proceed to createHost. Errors map cleanly to HTTP statuses:
//   - sql.ErrNoRows           → 401 (bad token)
//   - errEnrollmentExpired    → 410 (timed out, 1h TTL)
//   - errEnrollmentAlready    → 409 (already consumed)
func (p *Plugin) consumeEnrollmentToken(rawToken string, now time.Time) (tokenID, userID, workspaceID, hostName string, err error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		err = sql.ErrNoRows
		return
	}
	hash := hashAgentToken(rawToken)
	var coderJSON string
	err = p.DB.QueryRow(
		`SELECT id, user_id, COALESCE(coder_config::text, '{}')
		 FROM agent_tokens
		 WHERE token_hash = $1 AND revoked_at IS NULL LIMIT 1`,
		hash,
	).Scan(&tokenID, &userID, &coderJSON)
	if err != nil {
		return
	}
	var marker pendingEnrollmentMarker
	if jerr := json.Unmarshal([]byte(coderJSON), &marker); jerr != nil || !marker.PendingEnrollment {
		err = errEnrollmentAlready
		return
	}
	if now.After(marker.ExpiresAt) {
		err = errEnrollmentExpired
		return
	}
	// Clear the pending marker so subsequent uses get errEnrollmentAlready.
	if _, uerr := p.DB.Exec(
		`UPDATE agent_tokens SET coder_config = '{}'::jsonb WHERE id = $1`,
		tokenID,
	); uerr != nil {
		err = uerr
		return
	}
	workspaceID = marker.WorkspaceID
	hostName = marker.HostName
	return
}

var (
	errEnrollmentExpired = errors.New("enrollment token expired")
	errEnrollmentAlready = errors.New("enrollment token already consumed")
)

// ---- internals ---------------------------------------------------------

// hostSelectColumns surfaces every column the UI needs in one round
// trip. v4 additions: mem_peak_bytes + cpu_peak_pct (24h capacity
// ratchets, populated by /internal/v1/hosts/hello) and a derived
// agents_count via a correlated subquery (LEFT JOIN COUNT was the
// alternative; subquery keeps the query plan flat — Postgres
// hash-joins the agents row group on host_id in either case, and the
// subquery avoids changing the row cardinality of the outer SELECT
// so a row-at-a-time getHostByID path works without GROUP BY
// gymnastics). agents.host_id is indexed (idx_agents_host) so the
// subquery is an index lookup per row, not a seqscan.
const hostSelectColumns = `SELECT id, workspace_id, slug, name,
	    agent_token_id, COALESCE(os, ''), COALESCE(arch, ''),
	    COALESCE(last_seen_ip, ''),
	    COALESCE(advertised_skills_json::text, '[]'),
	    last_seen_at, created_at,
	    COALESCE(host_info_json::text, '{}'),
	    host_info_seen_at,
	    mem_peak_bytes,
	    cpu_peak_pct,
	    (SELECT COUNT(*) FROM agents a WHERE a.host_id = hosts.id) AS agents_count
	  FROM hosts`

// scanHost handles both QueryRow + Query results via the io.Reader-ish
// scanner pattern.
type rowScanner interface {
	Scan(dest ...any) error
}

func (p *Plugin) scanHost(row rowScanner) (*Host, error) {
	h, err := scanHostInto(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return h, err
}

func (p *Plugin) scanHostRow(row rowScanner) (*Host, error) {
	return scanHostInto(row)
}

func scanHostInto(row rowScanner) (*Host, error) {
	var h Host
	var agentTokenID sql.NullString
	var lastSeenAt sql.NullTime
	var skillsText string
	var hostInfoText string
	var hostInfoSeenAt sql.NullTime
	var memPeakBytes sql.NullInt64
	var cpuPeakPct sql.NullFloat64
	var agentsCount int64
	if err := row.Scan(
		&h.ID, &h.WorkspaceID, &h.Slug, &h.Name,
		&agentTokenID, &h.OS, &h.Arch, &h.LastSeenIP,
		&skillsText, &lastSeenAt, &h.CreatedAt,
		&hostInfoText, &hostInfoSeenAt,
		&memPeakBytes, &cpuPeakPct, &agentsCount,
	); err != nil {
		return nil, err
	}
	if memPeakBytes.Valid {
		v := memPeakBytes.Int64
		h.MemPeakBytes = &v
	}
	if cpuPeakPct.Valid {
		v := cpuPeakPct.Float64
		h.CPUPeakPct = &v
	}
	h.AgentsCount = int(agentsCount)
	if agentTokenID.Valid {
		v := agentTokenID.String
		h.AgentTokenID = &v
	}
	if lastSeenAt.Valid {
		v := lastSeenAt.Time
		h.LastSeenAt = &v
	}
	h.AdvertisedSkillsJSON = json.RawMessage(skillsText)
	// host_info_json is always present (DEFAULT '{}'), but decode
	// defensively — a corrupt row shouldn't 500 the list endpoint.
	if hostInfoText != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(hostInfoText), &parsed); err == nil && len(parsed) > 0 {
			h.HostInfo = parsed
		}
	}
	if hostInfoSeenAt.Valid {
		v := hostInfoSeenAt.Time
		h.HostInfoSeenAt = &v
	}
	return &h, nil
}

var slugSanitize = regexp.MustCompile(`[^a-z0-9-]+`)

// uniqueHostSlugInWorkspace turns "My Box" into "my-box", checks the
// (workspace_id, slug) UNIQUE constraint, and appends -2/-3 etc. until
// it finds a free name. Bounded to 50 attempts to defend against
// pathological inputs that all collide.
func (p *Plugin) uniqueHostSlugInWorkspace(workspaceID, name string) (string, error) {
	base := strings.ToLower(strings.TrimSpace(name))
	base = strings.ReplaceAll(base, " ", "-")
	base = slugSanitize.ReplaceAllString(base, "")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "host"
	}
	if len(base) > 48 {
		base = base[:48]
	}
	candidate := base
	for i := 1; i <= 50; i++ {
		var exists bool
		err := p.DB.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM hosts WHERE workspace_id = $1 AND slug = $2)`,
			workspaceID, candidate,
		).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
		i++
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return "", errors.New("could not find free slug after 50 attempts")
}
