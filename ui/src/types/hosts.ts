// Type definitions for the Host module (Phase 0).
// Mirror the Go-side structs in internal/app/dock/hosts_store.go.

// Inlined to break the dock-internal `./dashboard.js` dep that doesn't
// exist in the extracted repo. Same shape every plugin uses.
type ErrorResponse = { error?: string };

export type AdvertisedSkill = {
  kind: string;
  version?: string;
  capabilities?: Record<string, unknown>;
};

// HostInfo — static facts the agent collects at register/hello (opaque
// JSONB server-side). Mirrors hostinfo.HostInfo in polar-agent. All
// optional: a collector that failed simply omits its key.
export type HostInfo = {
  hw_model?: string;
  model_name?: string; // friendly ("MacBook Pro") — darwin
  cpu_brand?: string;
  cpu_cores?: number;
  memory_bytes?: number;
  gpu?: { vendor?: string; model?: string; cores?: number };
  os_version?: string;
  kernel?: string;
  virt?: string;
  // Tier-1/2 static facts (polar-agent P0)
  wifi_mac?: string;
  disk_total_bytes?: number;
  has_battery?: boolean;
  has_fan?: boolean;
};

export type Host = {
  id: string;
  workspace_id: string;
  slug: string;
  name: string;
  agent_token_id?: string;
  os: string;
  arch: string;
  last_seen_ip?: string;
  advertised_skills?: AdvertisedSkill[];
  last_seen_at?: string;
  created_at: string;
  // v4 + host-info additions (optional — older rows / dock versions omit)
  host_info?: HostInfo;
  agents_count?: number;
  mem_peak_bytes?: number;
  cpu_peak_pct?: number;
};

// AgentListItem — one logical agent bound to a host (v4). From
// GET /api/hosts/:id/agents. Mirrors AgentListItem in hosts_store.go.
export type AgentListItem = {
  id: string;
  host_id: string;
  host_name?: string;
  name: string;
  bot_user_id?: string;
  agent_token_id_suffix: string;
  os?: string;
  arch?: string;
  created_at: string;
  last_hello_at?: string;
};

export type AgentListResponse = ErrorResponse & {
  agents?: AgentListItem[];
};

export type HostListResponse = ErrorResponse & {
  hosts?: Host[];
};

export type HostDetailResponse = ErrorResponse & {
  host?: Host;
};

export type HostEnrollPayload = {
  name: string;
};

export type HostEnrollResponse = ErrorResponse & {
  token?: string;
  expires_at?: string;
  host_name?: string;
  install_hint?: string;
};

// P1d additions ----------------------------------------------------------

export type HostSkill = {
  id: number;
  host_id: string;
  kind: string;
  name: string;
  config_json: Record<string, unknown>;
  enabled: boolean;
  auto_start: boolean;
  created_by: string;
  created_at: string;
  updated_at: string;
};

export type HostSkillWithHost = HostSkill & {
  host_slug: string;
  host_name: string;
};

export type HostSkillListResponse = ErrorResponse & {
  skills?: HostSkill[];
};

export type HostSkillsWithHostListResponse = ErrorResponse & {
  skills?: HostSkillWithHost[];
};

// P1c.4: credential vault. The raw `value` is NEVER returned from
// the server — only masked_value + the encrypted bit.
export type HostSkillCredential = {
  id: number;
  host_skill_id: number;
  key: string;
  masked_value: string;
  encrypted: boolean;
  last_used_at?: string;
  created_by: string;
  created_at: string;
  updated_at: string;
};

export type HostSkillCredentialListResponse = ErrorResponse & {
  credentials?: HostSkillCredential[];
  encryption_active?: boolean;
};

// P2 (Shell skill) -------------------------------------------------------

export type ShellOpenResponse = ErrorResponse & {
  run_id?: number;
  ws_url?: string;
  host_id?: string;
  opened_at?: string;
};

export type ShellSession = {
  run_id: number;
  host_id: string;
  host_skill_id: number;
  operator_user_id: string;
  opened_at: string;
};

export type ShellSessionListResponse = ErrorResponse & {
  sessions?: ShellSession[];
};

// VNC skill --------------------------------------------------------------

export type VNCOpenResponse = ErrorResponse & {
  run_id?: number;
  ws_url?: string;
  host_id?: string;
  target?: string;
  opened_at?: string;
};

export type VNCSession = {
  run_id: number;
  host_id: string;
  host_skill_id: number;
  operator_user_id: string;
  opened_at: string;
  target?: string;
};

export type VNCSessionListResponse = ErrorResponse & {
  sessions?: VNCSession[];
};
