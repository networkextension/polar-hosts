// /hosts.html — Host module Phase 0 page logic.
//
// Behavior:
//   - List of workspace-scoped hosts on the left, click to open detail
//     on the right.
//   - "+ 注册主机" opens a modal that mints a one-time enrollment
//     token; UI shows the install snippet and a copy button.
//   - Detail panel shows os/arch, last-seen status, and the
//     advertised_skills array (read-only in P0 — enabling/configuring
//     skills lands with P1+).
//
// No skill enable / start / stop yet. That's by design; per
// doc/host-module-dev.md the foundation lands first.

import { fetchCurrentUser, logout } from "@networkextension/polar-ui-common/api/session";
import {
  deleteHost,
  deleteHostSkillCredential,
  enrollHost,
  fetchHost,
  fetchHostAgents,
  fetchHostSkillCredentials,
  fetchHostSkillsForHost,
  fetchHosts,
  putHostSkillCredential,
  renameHostSkill,
} from "./api/hosts.js";
import { byId } from "@networkextension/polar-ui-common/lib/dom";
import { hydrateSiteBrand, renderSidebarFoot } from "@networkextension/polar-ui-common/lib/site";
import { mountPlatformNav } from "@networkextension/polar-ui-common/lib/sidebar";
import { bindThemeSync, initStoredTheme } from "@networkextension/polar-ui-common/lib/theme";
import type { AgentListItem, Host, HostInfo, HostSkill, HostSkillCredential } from "./types/hosts.js";

initStoredTheme();
bindThemeSync();

// ── DOM ────────────────────────────────────────────────────────────────
const hostsList = byId<HTMLElement>("hostsList");
const hostsEmpty = byId<HTMLElement>("hostsEmpty");
const hostsPanel = byId<HTMLElement>("hostsPanel");
const hostsName = byId<HTMLElement>("hostsName");
const hostsStatusBadge = byId<HTMLElement>("hostsStatusBadge");
const hostsOSArch = byId<HTMLElement>("hostsOSArch");
const hostsSlug = byId<HTMLElement>("hostsSlug");
const hostsWgLink = byId<HTMLAnchorElement>("hostsWgLink");
const hostsSkillsList = byId<HTMLElement>("hostsSkillsList");

// Cross-link to the polar-wg module (Devices, filtered to this host). Derives
// wg.<env> from the current origin (hosts.<env>), matching the platform-nav
// absolute-URL convention.
function wgLinkURL(hostId: string): string {
  const host = window.location.host.replace(/^hosts\./, "wg.");
  return `${window.location.protocol}//${host}/wg-tokens.html?host_id=${encodeURIComponent(hostId)}`;
}
const hostsLastSeen = byId<HTMLElement>("hostsLastSeen");
const hostsLastSeenIP = byId<HTMLElement>("hostsLastSeenIP");
const hostsHardwareGrid = byId<HTMLElement>("hostsHardwareGrid");
const hostsAgentsList = byId<HTMLElement>("hostsAgentsList");
const hostsDeleteBtn = byId<HTMLButtonElement>("hostsDeleteBtn");

const hostsAddBtn = byId<HTMLButtonElement>("hostsAddBtn");
const hostsAddModal = byId<HTMLElement>("hostsAddModal");
const hostsAddModalCloseBtn = byId<HTMLButtonElement>("hostsAddModalCloseBtn");
const hostsAddForm = byId<HTMLFormElement>("hostsAddForm");
const hostsAddNameInput = byId<HTMLInputElement>("hostsAddName");
const hostsAddStatus = byId<HTMLElement>("hostsAddStatus");
const hostsAddSubmitBtn = byId<HTMLButtonElement>("hostsAddSubmitBtn");
const hostsAddTokenBox = byId<HTMLElement>("hostsAddTokenBox");
const hostsAddTokenSnippet = byId<HTMLElement>("hostsAddTokenSnippet");
const hostsAddCopyBtn = byId<HTMLButtonElement>("hostsAddCopyBtn");

const logoutBtn = byId<HTMLButtonElement>("logoutBtn");

// ── state ───────────────────────────────────────────────────────────────
let hosts: Host[] = [];
let activeHostId: string | null = null;
let activeHost: Host | null = null;
// P1d: persistent host_skill rows + per-skill credential lists,
// populated when a host is opened. Keyed by host_skill_id.
let activeHostSkills: HostSkill[] = [];
const credentialsBySkill: Map<number, HostSkillCredential[]> = new Map();
let credentialEncryptionActive = false;

// ── helpers ─────────────────────────────────────────────────────────────
function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function formatRelative(iso?: string): string {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  const diff = Date.now() - then;
  if (diff < 0) return new Date(iso).toLocaleString();
  if (diff < 5_000) return "刚刚";
  if (diff < 60_000) return `${Math.floor(diff / 1000)} 秒前`;
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;
  return `${Math.floor(diff / 86_400_000)} 天前`;
}

// Human-readable bytes → "32 GB" / "494 GB" / "1.2 TB". Base-1000
// (marketing GB, matches how Apple labels disk/RAM). 0/undefined → "—".
function formatBytes(n?: number): string {
  if (!n || n <= 0) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  return `${v >= 100 || i <= 1 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

// Online threshold: agent advertises every WS handshake + a future
// heartbeat will keep last_seen_at fresh. 60s gives generous slack
// for cellular / wifi roaming without false-yellow flicker.
const ONLINE_MS = 60_000;
function isOnline(host: Host): boolean {
  if (!host.last_seen_at) return false;
  return Date.now() - new Date(host.last_seen_at).getTime() < ONLINE_MS;
}

function setModalOpen(modal: HTMLElement, open: boolean): void {
  modal.classList.toggle("open", open);
  modal.setAttribute("aria-hidden", open ? "false" : "true");
}

function clearStatus(el: HTMLElement | null): void {
  if (!el) return;
  el.textContent = "";
  el.classList.remove("status-success", "status-error");
}

function setStatus(el: HTMLElement | null, msg: string, error = false): void {
  if (!el) return;
  el.textContent = msg;
  el.classList.toggle("status-error", error);
  el.classList.toggle("status-success", !error);
}

// ── list rendering ──────────────────────────────────────────────────────
function renderHostsList(): void {
  if (!hosts.length) {
    hostsList.innerHTML = `<div class="chat-empty">还没有主机。点右上角「＋ 注册主机」开始。</div>`;
    return;
  }
  hostsList.innerHTML = hosts
    .map((h) => {
      const online = isOnline(h);
      const dot = online ? "🟢" : "⚪";
      const status = online ? `online · ${formatRelative(h.last_seen_at)}` : `offline · ${formatRelative(h.last_seen_at)}`;
      const skillCount = (h.advertised_skills || []).length;
      const skillBadges = (h.advertised_skills || [])
        .slice(0, 4)
        .map((s) => `<span class="settings-value-pill" style="font-size:10px;">${escapeHTML(s.kind)}</span>`)
        .join(" ");
      const active = h.id === activeHostId ? "active" : "";
      // Basic info line: friendly model (if reported) + os/arch, then status.
      const model = h.host_info?.model_name || h.host_info?.hw_model || "";
      const agentCount = h.agents_count || 0;
      const basics = [
        model ? escapeHTML(model) : "",
        `${escapeHTML(h.os || "?")}/${escapeHTML(h.arch || "?")}`,
        agentCount ? `${agentCount} agent${agentCount > 1 ? "s" : ""}` : "",
      ]
        .filter(Boolean)
        .join(" · ");
      return `
        <div class="video-studio-project-item ${active}" data-host-id="${escapeHTML(h.id)}">
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">
              ${dot} ${escapeHTML(h.name)}
            </div>
            <div style="font-size:11px; color:var(--text-muted,#888); margin-top:2px;">
              ${basics}
            </div>
            <div style="font-size:11px; color:var(--text-muted,#888); margin-top:1px;">
              ${escapeHTML(status)}
            </div>
            <div style="margin-top:4px; display:flex; gap:4px; flex-wrap:wrap;">
              ${skillBadges}
              ${skillCount > 4 ? `<span style="font-size:10px; color:var(--text-muted,#888);">+${skillCount - 4}</span>` : ""}
            </div>
          </div>
        </div>`;
    })
    .join("");

  hostsList.querySelectorAll<HTMLElement>("[data-host-id]").forEach((el) => {
    el.addEventListener("click", () => {
      const id = el.getAttribute("data-host-id");
      if (id) void openHost(id);
    });
  });
}

// ── detail rendering ────────────────────────────────────────────────────
function renderHostDetail(host: Host): void {
  hostsEmpty.hidden = true;
  hostsPanel.hidden = false;
  activeHost = host;

  hostsName.textContent = host.name;
  const online = isOnline(host);
  hostsStatusBadge.textContent = online ? "🟢 online" : "⚪ offline";
  hostsStatusBadge.style.background = online ? "rgba(34,197,94,.12)" : "rgba(148,163,184,.12)";
  hostsOSArch.textContent = `${host.os || "?"}/${host.arch || "?"}`;
  hostsSlug.textContent = host.slug;
  hostsWgLink.href = wgLinkURL(host.id);
  hostsWgLink.hidden = false;

  // P1d: render persistent host_skills (auto-shimmed from advertise)
  // with inline credential forms. Loads async; show a placeholder
  // while the network call lands.
  hostsSkillsList.innerHTML = `<div class="chat-empty">加载 skills...</div>`;
  void loadHostSkillsForDetail(host.id);

  hostsLastSeen.textContent = host.last_seen_at ? formatRelative(host.last_seen_at) + " （" + new Date(host.last_seen_at).toLocaleString() + "）" : "—";
  hostsLastSeenIP.textContent = host.last_seen_ip ? `IP: ${host.last_seen_ip}` : "IP: —";

  // 硬件信息 — render the static host_info facts (model/cpu/mem/disk/wifi
  // + battery/fan chips). Falls back gracefully when a field is missing.
  renderHostHardware(host.host_info);

  // 配 Agent — the logical agents bound to this host. Loads async.
  hostsAgentsList.innerHTML = `<div class="chat-empty">加载 agents...</div>`;
  void loadHostAgents(host.id);

  renderHostMetricsSection(host);

  hostsDeleteBtn.hidden = false;
}

// ── 硬件信息 ─────────────────────────────────────────────────────────────
// Renders the agent-collected host_info as a label/value grid. Each cell
// is omitted when its source field is absent (collector failed / older
// agent), so the grid only shows what we actually know.
function renderHostHardware(info?: HostInfo): void {
  if (!info) {
    hostsHardwareGrid.innerHTML = `<div class="chat-empty" style="grid-column:1/-1;">尚未上报硬件信息。等 polar-agent 重新 attach 一次就会自动登记。</div>`;
    return;
  }
  const cells: Array<[string, string]> = [];
  const push = (label: string, val?: string) => {
    if (val && val !== "—") cells.push([label, val]);
  };
  push("型号", info.model_name || info.hw_model);
  push("CPU", info.cpu_brand ? `${info.cpu_brand}${info.cpu_cores ? ` · ${info.cpu_cores} 核` : ""}` : info.cpu_cores ? `${info.cpu_cores} 核` : undefined);
  push("内存", formatBytes(info.memory_bytes));
  push("磁盘", formatBytes(info.disk_total_bytes));
  if (info.gpu?.model) push("GPU", `${info.gpu.model}${info.gpu.cores ? ` · ${info.gpu.cores} 核` : ""}`);
  push("系统", info.os_version);
  push("Wi-Fi MAC", info.wifi_mac);
  if (info.virt) push("虚拟化", info.virt);

  // battery / fan presence chips (tri-state: undefined = unknown → omit).
  const chips: string[] = [];
  if (info.has_battery !== undefined) {
    chips.push(`<span class="settings-value-pill" style="font-size:11px;">${info.has_battery ? "🔋 有电池" : "🔌 无电池"}</span>`);
  }
  if (info.has_fan !== undefined) {
    chips.push(`<span class="settings-value-pill" style="font-size:11px;">${info.has_fan ? "🌀 有风扇" : "🪨 无风扇"}</span>`);
  }

  if (!cells.length && !chips.length) {
    hostsHardwareGrid.innerHTML = `<div class="chat-empty" style="grid-column:1/-1;">硬件信息为空。</div>`;
    return;
  }

  const grid = cells
    .map(
      ([label, val]) => `
      <div style="display:flex; flex-direction:column; gap:2px;">
        <span style="font-size:11px; color:var(--text-muted,#888);">${escapeHTML(label)}</span>
        <span style="font-size:13px; font-family:${label === "Wi-Fi MAC" ? "monospace" : "inherit"};">${escapeHTML(val)}</span>
      </div>`,
    )
    .join("");
  const chipRow = chips.length
    ? `<div style="grid-column:1/-1; display:flex; gap:6px; flex-wrap:wrap; margin-top:4px;">${chips.join(" ")}</div>`
    : "";
  hostsHardwareGrid.innerHTML = grid + chipRow;
}

// ── 配 Agent ─────────────────────────────────────────────────────────────
async function loadHostAgents(hostID: string): Promise<void> {
  const { response, data } = await fetchHostAgents(hostID);
  if (!response.ok) {
    hostsAgentsList.innerHTML = `<div class="chat-empty">加载 agents 失败：${escapeHTML(data.error || response.statusText)}</div>`;
    return;
  }
  renderAgents(data.agents || []);
}

function renderAgents(agents: AgentListItem[]): void {
  if (!agents.length) {
    hostsAgentsList.innerHTML = `<div class="chat-empty">这台主机还没有 agent。用上面的注册凭证在机器上跑 <code>polar-agent register</code> 即可绑定。</div>`;
    return;
  }
  hostsAgentsList.innerHTML = agents
    .map((a) => {
      const hello = a.last_hello_at ? `最后 hello ${formatRelative(a.last_hello_at)}` : "从未 hello";
      const bot = a.bot_user_id ? `bot ${escapeHTML(a.bot_user_id)}` : "";
      const meta = [a.os && a.arch ? `${escapeHTML(a.os)}/${escapeHTML(a.arch)}` : "", bot, hello]
        .filter(Boolean)
        .join(" · ");
      return `
        <div style="display:flex; flex-direction:column; gap:2px; padding:8px 10px; border:1px solid var(--border,#333); border-radius:8px;">
          <div style="font-weight:500; font-size:13px;">${escapeHTML(a.name)}</div>
          <div style="font-size:11px; color:var(--text-muted,#888);">${meta}</div>
          <div style="font-size:10px; color:var(--text-muted,#888); font-family:monospace;">${escapeHTML(a.id)} · token …${escapeHTML(a.agent_token_id_suffix)}</div>
        </div>`;
    })
    .join("");
}

// macmon dashboard embed — only meaningful for darwin (macmon is
// macOS-only). The Grafana URL assumes the dock host serves grafana at
// /grafana/ and the dashboard is provisioned with uid=macmon-overview.
// Iframe is the primary surface; the link is the fallback for browsers
// that refuse the embed (X-Frame-Options) or when Grafana hasn't
// enabled allow_embedding yet.
function renderHostMetricsSection(host: Host): void {
  const section = document.getElementById("hostsMetricsSection") as HTMLElement | null;
  const frame = document.getElementById("hostsMetricsFrame") as HTMLIFrameElement | null;
  const link = document.getElementById("hostsMetricsOpen") as HTMLAnchorElement | null;
  const fallback = document.getElementById("hostsMetricsFallback") as HTMLElement | null;
  if (!section || !frame || !link || !fallback) return;
  if ((host.os || "").toLowerCase() !== "darwin") {
    section.hidden = true;
    return;
  }
  section.hidden = false;
  fallback.hidden = true;
  const dashURL = "/grafana/d/macmon-overview/macmon-overview?from=now-30m&theme=dark&refresh=10s";
  // kiosk=tv strips the chrome (sidebar + topnav) for a clean embed.
  frame.src = dashURL + "&kiosk=tv";
  link.href = dashURL;
  // X-Frame-Options refusal doesn't fire onerror reliably; use a
  // load-timeout heuristic. If the iframe never reports loaded after
  // 8s, surface the fallback hint.
  let loaded = false;
  const onLoad = () => { loaded = true; };
  frame.addEventListener("load", onLoad, { once: true });
  setTimeout(() => {
    if (!loaded) fallback.hidden = false;
  }, 8000);
}

async function loadHostSkillsForDetail(hostID: string): Promise<void> {
  const { response, data } = await fetchHostSkillsForHost(hostID);
  if (!response.ok) {
    hostsSkillsList.innerHTML = `<div class="chat-empty">加载 skills 失败：${escapeHTML(data.error || response.statusText)}</div>`;
    return;
  }
  activeHostSkills = data.skills || [];
  // Fetch credentials for each skill in parallel — small N, fan-out
  // is fine.
  credentialsBySkill.clear();
  credentialEncryptionActive = false;
  await Promise.all(activeHostSkills.map(async (sk) => {
    const result = await fetchHostSkillCredentials(hostID, sk.id);
    if (result.response.ok) {
      credentialsBySkill.set(sk.id, result.data.credentials || []);
      if (result.data.encryption_active) {
        credentialEncryptionActive = true;
      }
    }
  }));
  renderHostSkillsWithCredentials(hostID);
}

function renderHostSkillsWithCredentials(hostID: string): void {
  if (!activeHostSkills.length) {
    hostsSkillsList.innerHTML = `<div class="chat-empty">尚未上报 skill。等 polar-agent 重新 attach 一次就会自动登记。</div>`;
    return;
  }
  const encryptionBadge = credentialEncryptionActive
    ? `<span style="font-size:10px; padding:2px 6px; background:rgba(34,197,94,.12); color:#16a34a; border-radius:4px;">🔒 加密</span>`
    : `<span style="font-size:10px; padding:2px 6px; background:rgba(245,158,11,.12); color:#d97706; border-radius:4px;">⚠️ 未加密 (POLAR_CREDENTIAL_KEY 未设置)</span>`;
  hostsSkillsList.innerHTML = activeHostSkills.map((sk) => renderSkillCard(hostID, sk, encryptionBadge)).join("");
  attachSkillCardListeners(hostID);
}

function renderSkillCard(hostID: string, sk: HostSkill, encryptionBadge: string): string {
  const creds = credentialsBySkill.get(sk.id) || [];
  const tools = Array.isArray((sk.config_json as Record<string, unknown>).tools)
    ? ((sk.config_json as Record<string, unknown>).tools as string[]).join(", ")
    : "";
  const mode = String((sk.config_json as Record<string, unknown>).mode || "");
  const subtitle = [mode, tools && `tools: ${tools}`].filter(Boolean).join(" · ");

  const credRows = creds.length
    ? creds.map((c) => `
        <div data-cred-key="${escapeHTML(c.key)}" style="display:flex; align-items:center; gap:8px; padding:6px 10px; background:var(--surface-muted,#f7f7f9); border-radius:6px; font-size:12px;">
          <span style="font-family:monospace; font-weight:500;">${escapeHTML(c.key)}</span>
          <span style="font-family:monospace; color:var(--text-muted,#888);">${escapeHTML(c.masked_value)}</span>
          ${c.encrypted ? "" : `<span style="font-size:10px; color:#d97706;">明文</span>`}
          <span style="flex:1;"></span>
          <button type="button" class="btn-inline" data-cred-delete="${escapeHTML(c.key)}" data-skill-id="${sk.id}" style="font-size:11px; color:#c33;">删除</button>
        </div>`).join("")
    : `<div style="font-size:11px; color:var(--text-muted,#888); padding:4px 0;">尚未配置 API key</div>`;

  // P2: Shell skill gets an "Open shell" button instead of the
  // credentials form (it doesn't use API keys). VNC skill is similar
  // — auth happens in the browser via noVNC and the operator launches
  // it from /console.html as a pane, not from a per-card button here.
  // Other skill kinds get the credential vault UI.
  const isShell = sk.kind === "shell";
  const isVNC = sk.kind === "vnc";
  const headerExtras = isShell
    ? `<button type="button" class="btn-inline btn-secondary" data-shell-open="${sk.id}" style="margin-left:auto; font-size:12px;">▶ Open shell</button>`
    : isVNC
      ? `<a class="btn-inline btn-secondary" href="/console.html" target="_blank" style="margin-left:auto; font-size:12px; text-decoration:none;">▶ 打开 console</a>`
      : "";

  const body = isShell
    ? `<div style="font-size:11px; color:var(--text-muted,#888); padding:4px 0;">Shell skill 通过 pty 实时打开 host 上的 bash。每个 host 最多 4 个并发会话。</div>`
    : isVNC
      ? `<div style="font-size:11px; color:var(--text-muted,#888); padding:4px 0;">VNC skill 把 agent 上的 VNC 服务器（默认 127.0.0.1:5900）桥接到浏览器 noVNC。先开启系统设置 → 通用 → 共享 → 屏幕共享；从 /console.html 选 vnc skill 启动，浏览器会弹框让你输 macOS 用户名 + 密码。每个 host 最多 2 个并发会话。</div>`
      : `
      <div style="display:flex; align-items:center; gap:8px; margin-bottom:8px;">
        <span style="font-size:11px; font-weight:500;">Credentials</span>
        ${encryptionBadge}
      </div>
      <div style="display:flex; flex-direction:column; gap:6px;">
        ${credRows}
      </div>
      <form data-skill-cred-form="${sk.id}" style="display:flex; gap:6px; margin-top:8px;">
        <input type="text" class="input" data-cred-key-input style="flex:1; font-family:monospace; font-size:12px;" placeholder="ANTHROPIC_API_KEY" maxlength="100" required />
        <input type="password" class="input" data-cred-value-input style="flex:2; font-family:monospace; font-size:12px;" placeholder="sk-..." maxlength="500" required />
        <button type="submit" class="btn-inline btn-secondary" style="font-size:12px;">保存</button>
      </form>`;

  // Editable display name. Defaults to the bare kind; operator can
  // rename to "Office Mac VNC" / "Build Server Shell" / etc. The id
  // and kind stay shown as small grey metadata so admins can still
  // map the row back to the DB / agent advertise.
  const displayName = sk.name && sk.name !== sk.kind ? sk.name : sk.kind;
  return `
    <div data-skill-id="${sk.id}" style="border:1px solid var(--surface-border,#e5e7eb); border-radius:8px; padding:12px; margin-bottom:10px;">
      <div style="display:flex; align-items:center; gap:8px; margin-bottom:8px;">
        <input type="text" class="input" data-skill-name-input="${sk.id}"
               value="${escapeHTML(displayName)}"
               aria-label="重命名 host_skill"
               style="flex:1; max-width:280px; font-size:13px; font-weight:600; padding:4px 8px;"
               maxlength="80" />
        <button type="button" class="btn-inline" data-skill-rename="${sk.id}"
                style="font-size:11px; padding:2px 8px;">重命名</button>
        <span style="font-size:11px; color:var(--text-muted,#888); font-family:monospace;">${escapeHTML(sk.kind)} · id=${sk.id}</span>
        ${subtitle ? `<span style="font-size:11px; color:var(--text-muted,#888);">— ${escapeHTML(subtitle)}</span>` : ""}
        ${headerExtras}
      </div>
      ${body}
    </div>
  `;
}

function attachSkillCardListeners(hostID: string): void {
  // Skill rename — click button OR press Enter in the name input.
  hostsSkillsList.querySelectorAll<HTMLButtonElement>("button[data-skill-rename]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const skillID = Number(btn.dataset.skillRename || 0);
      const input = hostsSkillsList.querySelector<HTMLInputElement>(
        `input[data-skill-name-input="${skillID}"]`,
      );
      if (!input || !skillID) return;
      const name = input.value.trim();
      if (!name) {
        alert("名称不能为空");
        return;
      }
      btn.disabled = true;
      const { response, data } = await renameHostSkill(hostID, skillID, name);
      btn.disabled = false;
      if (!response.ok) {
        alert(`重命名失败：${data.error || response.statusText}`);
        return;
      }
      // Refresh so the new name shows in the rest of the UI (picker etc.).
      await loadHostSkillsForDetail(hostID);
    });
  });
  hostsSkillsList.querySelectorAll<HTMLInputElement>("input[data-skill-name-input]").forEach((input) => {
    input.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter") {
        ev.preventDefault();
        const skillID = input.dataset.skillNameInput;
        const btn = hostsSkillsList.querySelector<HTMLButtonElement>(
          `button[data-skill-rename="${skillID}"]`,
        );
        btn?.click();
      }
    });
  });
  hostsSkillsList.querySelectorAll<HTMLFormElement>("form[data-skill-cred-form]").forEach((form) => {
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const skillID = Number(form.dataset.skillCredForm || 0);
      const keyInput = form.querySelector<HTMLInputElement>("[data-cred-key-input]");
      const valueInput = form.querySelector<HTMLInputElement>("[data-cred-value-input]");
      if (!keyInput || !valueInput || !skillID) return;
      const key = keyInput.value.trim();
      const value = valueInput.value;
      if (!key || !value) return;
      const submitBtn = form.querySelector<HTMLButtonElement>("button[type=submit]");
      if (submitBtn) submitBtn.disabled = true;
      const { response, data } = await putHostSkillCredential(hostID, skillID, key, value);
      if (submitBtn) submitBtn.disabled = false;
      if (!response.ok) {
        alert(`保存失败：${data.error || response.statusText}`);
        return;
      }
      keyInput.value = "";
      valueInput.value = "";
      await loadHostSkillsForDetail(hostID);
    });
  });
  hostsSkillsList.querySelectorAll<HTMLButtonElement>("button[data-cred-delete]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const key = btn.dataset.credDelete || "";
      const skillID = Number(btn.dataset.skillId || 0);
      if (!key || !skillID) return;
      if (!confirm(`删除 credential "${key}"?`)) return;
      btn.disabled = true;
      const { response, data } = await deleteHostSkillCredential(hostID, skillID, key);
      if (!response.ok) {
        alert(`删除失败：${data.error || response.statusText}`);
        btn.disabled = false;
        return;
      }
      await loadHostSkillsForDetail(hostID);
    });
  });
  // P2 (Shell): "Open shell" button lazy-loads the xterm bundle.
  hostsSkillsList.querySelectorAll<HTMLButtonElement>("button[data-shell-open]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const skillID = Number(btn.dataset.shellOpen || 0);
      if (!skillID) return;
      btn.disabled = true;
      try {
        // Lazy-load the xterm bundle. Dock serves these from
        // /scripts/hosts-shell.js (esbuild splits each top-level
        // ui/src/*.ts as its own entry). The URL path isn't a
        // TypeScript module, so we ts-ignore the import + cast.
        // @ts-ignore — runtime URL path, not a TS module
        const mod = (await import("/scripts/hosts-shell.js")) as {
          openShellModal: (opts: {
            hostId: string;
            hostSkillId: number;
            hostName?: string;
          }) => Promise<void>;
        };
        await mod.openShellModal({
          hostId: hostID,
          hostSkillId: skillID,
          hostName: activeHost?.name,
        });
      } catch (err) {
        alert(`Open shell failed: ${err instanceof Error ? err.message : String(err)}`);
      } finally {
        btn.disabled = false;
      }
    });
  });
}

async function loadHosts(): Promise<void> {
  const { response, data } = await fetchHosts();
  if (!response.ok) {
    hostsList.innerHTML = `<div class="chat-empty">加载失败：${escapeHTML(data.error || response.statusText)}</div>`;
    return;
  }
  hosts = data.hosts || [];
  renderHostsList();
  if (activeHostId) {
    const stillExists = hosts.find((h) => h.id === activeHostId);
    if (!stillExists) {
      activeHostId = null;
      activeHost = null;
      hostsEmpty.hidden = false;
      hostsPanel.hidden = true;
    }
  }
}

async function openHost(id: string): Promise<void> {
  activeHostId = id;
  renderHostsList(); // re-render to update active highlight
  const { response, data } = await fetchHost(id);
  if (!response.ok || !data.host) {
    hostsEmpty.hidden = false;
    hostsPanel.hidden = true;
    return;
  }
  renderHostDetail(data.host);
}

// ── modal: add host ─────────────────────────────────────────────────────
hostsAddBtn.addEventListener("click", () => {
  hostsAddNameInput.value = "";
  clearStatus(hostsAddStatus);
  hostsAddTokenBox.hidden = true;
  hostsAddSubmitBtn.disabled = false;
  hostsAddSubmitBtn.textContent = "生成注册凭证";
  setModalOpen(hostsAddModal, true);
  setTimeout(() => hostsAddNameInput.focus(), 0);
});

hostsAddModalCloseBtn.addEventListener("click", () => {
  setModalOpen(hostsAddModal, false);
  void loadHosts();
});

hostsAddForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = hostsAddNameInput.value.trim();
  if (!name) {
    setStatus(hostsAddStatus, "请填写主机名称", true);
    return;
  }
  hostsAddSubmitBtn.disabled = true;
  hostsAddSubmitBtn.textContent = "生成中...";
  setStatus(hostsAddStatus, "");

  const { response, data } = await enrollHost({ name });
  hostsAddSubmitBtn.disabled = false;
  hostsAddSubmitBtn.textContent = "生成注册凭证";

  if (!response.ok || !data.token) {
    setStatus(hostsAddStatus, data.error || "生成失败", true);
    return;
  }
  setStatus(hostsAddStatus, `凭证已生成，1 小时内有效。`, false);
  hostsAddTokenBox.hidden = false;
  hostsAddTokenSnippet.textContent = data.install_hint || `polar-agent register --token=${data.token} --name="${name}"`;
});

hostsAddCopyBtn.addEventListener("click", async () => {
  const text = hostsAddTokenSnippet.textContent || "";
  try {
    await navigator.clipboard.writeText(text);
    hostsAddCopyBtn.textContent = "已复制";
    setTimeout(() => {
      hostsAddCopyBtn.textContent = "复制";
    }, 1500);
  } catch {
    window.prompt("复制下面这段命令：", text);
  }
});

// ── delete host ─────────────────────────────────────────────────────────
hostsDeleteBtn.addEventListener("click", async () => {
  if (!activeHost) return;
  if (!window.confirm(`确认删除主机「${activeHost.name}」？关联的 skill 配置会一并清理（P0 阶段还没有配置，所以本质上只是从列表移除）。`)) {
    return;
  }
  const { response, data } = await deleteHost(activeHost.id);
  if (!response.ok) {
    window.alert(data.error || "删除失败");
    return;
  }
  activeHostId = null;
  activeHost = null;
  hostsEmpty.hidden = false;
  hostsPanel.hidden = true;
  hostsDeleteBtn.hidden = true;
  await loadHosts();
});

// ── boot ────────────────────────────────────────────────────────────────
async function boot(): Promise<void> {
  const { response, data } = await fetchCurrentUser();
  if (!response.ok) {
    window.location.href = "/login.html";
    return;
  }
  renderSidebarFoot(data);
  hydrateSiteBrand();
  void mountPlatformNav();
  await loadHosts();
  // Deep-link from another module (e.g. polar-wg device row): ?id=<host_id>
  // auto-opens that host's detail.
  const deepID = new URLSearchParams(window.location.search).get("id");
  if (deepID && hosts.some((h) => h.id === deepID)) {
    void openHost(deepID);
  }
}

logoutBtn?.addEventListener("click", () => {
  void logout();
});

void boot();
