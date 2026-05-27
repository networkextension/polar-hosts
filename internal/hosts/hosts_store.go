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
	// MachineUUID is the stable per-machine fingerprint (darwin
	// IOPlatformUUID / linux machine-id / freebsd smbios) used by
	// createHost to dedup a re-register from the same physical box.
	// Empty when the agent's collector failed or when this row was
	// created before the column existed.
	MachineUUID string `json:"machine_uuid,omitempty"`
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

// createHost is the entry the register handlers call. It is a thin
// wrapper around createOrUpdateHostByMachineUUID that preserves the
// pre-PR signature for callers that don't (yet) pass machine_uuid.
//
// TODO(#216-phase2): drop the local hosts table and have polar-hosts
// read hosts back from dock via a new SDK GET surface.
func (p *Plugin) createHost(workspaceID, name, agentTokenID, hostOS, hostArch string, now time.Time) (*Host, error) {
	return p.createOrUpdateHostByMachineUUID(workspaceID, name, agentTokenID, hostOS, hostArch, "", now)
}

// createOrUpdateHostByMachineUUID either inserts a new hosts row OR
// updates an existing one when (workspace_id, machine_uuid) already
// has a hit. Empty machineUUID means "no fingerprint available" (older
// agent or collector failed): skip dedup and fall through to legacy
// INSERT — that matches the pre-PR behavior exactly.
//
// Dual-write to dock follows the same rollback contract as before:
// on dock-side failure we DELETE the local row so the operator sees
// one clean error rather than a half-registered host that silently
// drops skill.advertise. The UPDATE branch doesn't need a rollback
// (we didn't mutate dock state yet) — dock's matching upsert in
// /internal/v1/hosts/issue dedups on the same key idempotently.
func (p *Plugin) createOrUpdateHostByMachineUUID(workspaceID, name, agentTokenID, hostOS, hostArch, machineUUID string, now time.Time) (*Host, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	name = strings.TrimSpace(name)
	if workspaceID == "" || name == "" {
		return nil, errors.New("workspace_id and name are required")
	}
	machineUUID = strings.TrimSpace(machineUUID)
	atID := strings.TrimSpace(agentTokenID)
	osTrim := strings.TrimSpace(hostOS)
	archTrim := strings.TrimSpace(hostArch)

	// Dedup branch: only when machine_uuid is non-empty. If the box
	// already has a row for this workspace, repurpose it instead of
	// minting a new one.
	if machineUUID != "" {
		existing, err := p.getHostByWorkspaceMachineUUID(workspaceID, machineUUID)
		if err != nil {
			return nil, fmt.Errorf("lookup by machine_uuid: %w", err)
		}
		if existing != nil {
			// Operator may have re-registered with a different name; keep
			// the old slug stable (it's part of URLs / WS subscription
			// keys) but refresh name + OS/arch + agent_token + os/arch.
			var atPtr *string
			if atID != "" {
				atPtr = &atID
			}
			if _, err := p.DB.Exec(
				`UPDATE hosts
				 SET name           = $1,
				     agent_token_id = $2,
				     os             = $3,
				     arch           = $4
				 WHERE id = $5`,
				name, atPtr, osTrim, archTrim, existing.ID,
			); err != nil {
				return nil, fmt.Errorf("update existing host on re-register: %w", err)
			}
			// Dual-write to dock with the same machine_uuid so dock's
			// matching upsert hits its UPDATE branch (rather than
			// inserting a duplicate). PR 3 wires the dock side.
			if p.Dock != nil {
				if _, derr := p.Dock.IssueHost(sdk.HostIssueRequest{
					ID:           existing.ID,
					WorkspaceID:  workspaceID,
					Slug:         existing.Slug,
					Name:         name,
					AgentTokenID: atID,
					OS:           osTrim,
					Arch:         archTrim,
					MachineUUID:  machineUUID,
				}); derr != nil {
					// Don't roll back the local UPDATE — the row was
					// already correct before this call and the dock side
					// is idempotent on retry. Just surface the error.
					return nil, fmt.Errorf("dock issue host (dedup update): %w", derr)
				}
			}
			// Refresh + return the updated row.
			updated, err := p.getHostByID(existing.ID)
			if err != nil {
				return nil, fmt.Errorf("reload host after update: %w", err)
			}
			return updated, nil
		}
	}

	// No existing row (or no machine_uuid to dedup on): mint fresh.
	slug, err := p.uniqueHostSlugInWorkspace(workspaceID, name)
	if err != nil {
		return nil, fmt.Errorf("generate slug: %w", err)
	}
	host := &Host{
		ID:          "h_" + generateResourceID(),
		WorkspaceID: workspaceID,
		Slug:        slug,
		Name:        name,
		OS:          osTrim,
		Arch:        archTrim,
		MachineUUID: machineUUID,
		CreatedAt:   now,
	}
	if atID != "" {
		v := atID
		host.AgentTokenID = &v
	}
	// machine_uuid column may be empty TEXT or NULL — we pass through
	// as-is (the partial unique index excludes empties so duplicate
	// "" values don't collide).
	var muPtr any
	if machineUUID != "" {
		muPtr = machineUUID
	}
	if _, err := p.DB.Exec(
		`INSERT INTO hosts (id, workspace_id, slug, name, agent_token_id, os, arch, machine_uuid, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		host.ID, host.WorkspaceID, host.Slug, host.Name,
		host.AgentTokenID, host.OS, host.Arch, muPtr, host.CreatedAt,
	); err != nil {
		return nil, err
	}
	// Dual-write to dock. Rollback the local row on failure so the
	// register call returns a single clean error instead of leaving a
	// half-registered host that silently drops its skill.advertise.
	if p.Dock != nil {
		if _, derr := p.Dock.IssueHost(sdk.HostIssueRequest{
			ID:           host.ID,
			WorkspaceID:  host.WorkspaceID,
			Slug:         host.Slug,
			Name:         host.Name,
			AgentTokenID: atID,
			OS:           host.OS,
			Arch:         host.Arch,
			MachineUUID:  machineUUID,
		}); derr != nil {
			if _, rerr := p.DB.Exec(`DELETE FROM hosts WHERE id = $1`, host.ID); rerr != nil {
				log.Printf("[#216] createHost: dock issue failed AND local rollback failed: dock_err=%v rollback_err=%v host_id=%s", derr, rerr, host.ID)
			}
			return nil, fmt.Errorf("dock issue host: %w", derr)
		}
	}
	return host, nil
}

// getHostByWorkspaceMachineUUID returns the row for one machine in one
// workspace (the dedup key). Backed by the partial unique index
// uniq_hosts_workspace_machine_uuid declared in hosts-schema.sql.
// Returns (nil, nil) when no row exists — caller uses that as the
// "fresh INSERT" signal.
func (p *Plugin) getHostByWorkspaceMachineUUID(workspaceID, machineUUID string) (*Host, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	machineUUID = strings.TrimSpace(machineUUID)
	if workspaceID == "" || machineUUID == "" {
		return nil, nil
	}
	return p.scanHost(p.DB.QueryRow(
		hostSelectColumns+` WHERE workspace_id = $1 AND machine_uuid = $2 LIMIT 1`,
		workspaceID, machineUUID,
	))
}

func (p *Plugin) getHostByID(id string) (*Host, error) {
	return p.scanHost(p.DB.QueryRow(hostSelectColumns+` WHERE id = $1`, id))
}

func (p *Plugin) getHostBySlug(workspaceID, slug string) (*Host, error) {
	return p.scanHost(p.DB.QueryRow(hostSelectColumns+` WHERE workspace_id = $1 AND slug = $2`, workspaceID, slug))
}

// getHostByAgentToken is on the hot path of every agent WS message
// (called once at attach to cache host_id on agentConn). Backed by the
// partial index idx_hosts_agent_token in the schema.
func (p *Plugin) getHostByAgentToken(agentTokenID string) (*Host, error) {
	if strings.TrimSpace(agentTokenID) == "" {
		return nil, nil
	}
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
// Side effect: if hostInfoJSON includes a non-empty machine_uuid field,
// we also backfill the hosts.machine_uuid column. That lets legacy rows
// (registered before the column existed) become dedup-able as soon as
// their agent reconnects with an updated build — the column drives
// the partial unique index, so future re-registers go through the
// UPDATE branch in createOrUpdateHostByMachineUUID. We deliberately
// only write the column when:
//   - the parsed value is non-empty (don't blank an existing UUID), AND
//   - the row's current machine_uuid is empty (don't churn — if the
//     column was set, it's the source of truth; an agent re-collect
//     that returns a different value is a logic-board swap that needs
//     operator review, not silent rewrite).
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

	// Best-effort machine_uuid backfill — see comment above.
	if n > 0 {
		var parsed struct {
			MachineUUID string `json:"machine_uuid"`
		}
		if jerr := json.Unmarshal([]byte(payload), &parsed); jerr == nil {
			mu := strings.TrimSpace(parsed.MachineUUID)
			if mu != "" {
				// COALESCE-style guard: only write when current column
				// is empty / NULL. Returning the no-op silently is
				// fine — n still reflects the host_info update.
				if _, uerr := p.DB.Exec(
					`UPDATE hosts SET machine_uuid = $2
					 WHERE id = $1 AND COALESCE(machine_uuid, '') = ''`,
					hostID, mu,
				); uerr != nil {
					// Log only — never fail the hello path because the
					// backfill hiccuped. Most likely cause: partial
					// unique index conflict (another row in the same
					// workspace already claimed this UUID), which is
					// exactly the "duplicate hosts to merge" case the
					// operator will surface via the SQL one-liner.
					log.Printf("hosts: machine_uuid backfill for %s failed: %v", hostID, uerr)
				}
			}
		}
	}
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

const hostSelectColumns = `SELECT id, workspace_id, slug, name,
	    agent_token_id, COALESCE(os, ''), COALESCE(arch, ''),
	    COALESCE(last_seen_ip, ''),
	    COALESCE(advertised_skills_json::text, '[]'),
	    last_seen_at, created_at,
	    COALESCE(host_info_json::text, '{}'),
	    host_info_seen_at,
	    COALESCE(machine_uuid, '')
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
	var machineUUID string
	if err := row.Scan(
		&h.ID, &h.WorkspaceID, &h.Slug, &h.Name,
		&agentTokenID, &h.OS, &h.Arch, &h.LastSeenIP,
		&skillsText, &lastSeenAt, &h.CreatedAt,
		&hostInfoText, &hostInfoSeenAt, &machineUUID,
	); err != nil {
		return nil, err
	}
	h.MachineUUID = machineUUID
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
