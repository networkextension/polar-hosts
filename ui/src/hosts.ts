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
import {
  NEON,
  NOC,
  fanPositions,
  nocChip,
  nocPanel,
  nocSpoke,
  nocSvg,
} from "@networkextension/polar-ui-common/lib/neon-topo";
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
const hostsNetPanel = byId<HTMLElement>("hostsNetPanel");
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

// ── device icons (Lucide, ISC-licensed, inlined — no runtime dep) ─────────
// Keyed by os/model/virt so a host reads at a glance: laptop vs server vs
// VM vs phone, tinted by platform.
const LUCIDE: Record<string, string> = {
  laptop:
    '<path d="M18 5a2 2 0 0 1 2 2v8.526a2 2 0 0 0 .212.897l1.068 2.127a1 1 0 0 1-.9 1.45H3.62a1 1 0 0 1-.9-1.45l1.068-2.127A2 2 0 0 0 4 15.526V7a2 2 0 0 1 2-2z"/><path d="M20.054 15.987H3.946"/>',
  monitor:
    '<rect width="20" height="14" x="2" y="3" rx="2"/><line x1="8" x2="16" y1="21" y2="21"/><line x1="12" x2="12" y1="17" y2="21"/>',
  server:
    '<rect width="20" height="8" x="2" y="2" rx="2" ry="2"/><rect width="20" height="8" x="2" y="14" rx="2" ry="2"/><line x1="6" x2="6.01" y1="6" y2="6"/><line x1="6" x2="6.01" y1="18" y2="18"/>',
  smartphone: '<rect width="14" height="20" x="5" y="2" rx="2" ry="2"/><path d="M12 18h.01"/>',
  box:
    '<path d="M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z"/><path d="m3.3 7 8.7 5 8.7-5"/><path d="M12 22V12"/>',
  hardDrive:
    '<path d="M10 16h.01"/><path d="M2.212 11.577a2 2 0 0 0-.212.896V18a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-5.527a2 2 0 0 0-.212-.896L18.55 5.11A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/><path d="M21.946 12.013H2.054"/><path d="M6 16h.01"/>',
};

function deviceKind(h: Host): { key: string; color: string } {
  const os = (h.os || "").toLowerCase();
  const model = (h.host_info?.model_name || h.host_info?.hw_model || "").toLowerCase();
  const virt = (h.host_info?.virt || "").toLowerCase();
  const battery = h.host_info?.has_battery;
  if (os === "ios" || os === "android") return { key: "smartphone", color: "#0a84ff" };
  if (virt && virt !== "none") return { key: "box", color: "#a855f7" }; // VM / hypervisor guest
  if (os === "darwin" || os === "macos") {
    if (/(mini|studio|imac|mac ?pro|macpro)/.test(model)) return { key: "monitor", color: "#8e8e93" };
    if (battery === true || /macbook|laptop/.test(model)) return { key: "laptop", color: "#8e8e93" };
    return { key: "laptop", color: "#8e8e93" };
  }
  if (os === "linux") return { key: "server", color: "#dd7a18" };
  if (os === "freebsd") return { key: "server", color: "#c0392b" };
  if (os === "windows") return { key: "monitor", color: "#0a84ff" };
  return { key: "hardDrive", color: "#8e8e93" };
}

function deviceIconSVG(h: Host, size = 22): string {
  const { key, color } = deviceKind(h);
  return `<svg viewBox="0 0 24 24" width="${size}" height="${size}" fill="none" stroke="${color}" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${LUCIDE[key] || LUCIDE.hardDrive}</svg>`;
}

// A rounded badge wrapping the device icon; ringed green when the host is online.
function deviceBadge(h: Host, online: boolean): string {
  return `<div style="flex:0 0 auto; width:36px; height:36px; display:flex; align-items:center; justify-content:center; border-radius:9px; background:var(--surface-2,#f2f2f7); box-shadow:${online ? "0 0 0 2px #34c759 inset" : "none"};">${deviceIconSVG(h)}</div>`;
}

// ── 网络拓扑 — dark-NOC physical-network panel ────────────────────────────
// Classifies each interface the agent reported into a physical kind (wifi /
// ethernet / cellular) or the wg mesh link, each with a neon accent. The whole
// thing renders on a self-contained dark "big-screen" panel (its own palette,
// not the platform theme) so it reads like a data-center NOC widget.

type NetKind = "wifi" | "ethernet" | "cellular" | "mesh" | "bridge" | "other";

interface NetIface {
  name: string;
  kind: NetKind;
  color: string;
  glyph: string; // inner SVG of a 24-box icon
  label: string;
  ipv4?: string;
  ipv6: { addr: string; private: boolean }[];
}

const NET_GLYPH: Record<NetKind, string> = {
  // wifi: arcs + dot
  wifi: '<path d="M5 13a10 10 0 0 1 14 0"/><path d="M8.5 16.5a5 5 0 0 1 7 0"/><path d="M2 8.82a15 15 0 0 1 20 0"/><line x1="12" y1="20" x2="12.01" y2="20"/>',
  // cellular: signal bars
  cellular: '<path d="M2 20h.01"/><path d="M7 20v-4"/><path d="M12 20v-8"/><path d="M17 20V8"/><path d="M22 4v16"/>',
  // ethernet: rack/plug
  ethernet: '<rect width="20" height="8" x="2" y="2" rx="2"/><rect width="20" height="8" x="2" y="14" rx="2"/><line x1="6" x2="6.01" y1="6" y2="6"/><line x1="6" x2="6.01" y1="18" y2="18"/>',
  // mesh/wg: shield
  mesh: '<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/>',
  bridge: '<path d="M6 9v12"/><path d="M18 9v12"/><path d="M3 9a9 9 0 0 1 18 0"/><path d="M3 14h18"/>',
  other: '<circle cx="12" cy="12" r="3"/><path d="M12 2v4M12 18v4M2 12h4M18 12h4"/>',
};

// Maps each interface kind onto the shared neon palette (polar-ui-common).
const NET_COLOR: Record<NetKind, string> = {
  wifi: NEON.cyan,
  cellular: NEON.amber,
  ethernet: NEON.green,
  mesh: NEON.violet,
  bridge: NEON.blue,
  other: NEON.slate,
};

function classifyIface(name: string, os: string, hasBattery?: boolean): { kind: NetKind; label: string } {
  const n = name.toLowerCase();
  if (/^(utun|wgc|wg)\d/.test(n) || n === "wg0") return { kind: "mesh", label: "WG Mesh" };
  if (/^(pdp_ip|rmnet|wwan|ppp|ce|qmi)/.test(n)) return { kind: "cellular", label: "蜂窝 / 4G" };
  if (/^(wlan|wl|wlp|airport)/.test(n)) return { kind: "wifi", label: "Wi-Fi" };
  if (/^bridge/.test(n)) return { kind: "bridge", label: "网桥" };
  if ((os === "darwin" || os === "macos") && n === "en0") {
    // en0 is wifi on laptops, ethernet on desktops (no battery).
    return hasBattery ? { kind: "wifi", label: "Wi-Fi" } : { kind: "ethernet", label: "以太网" };
  }
  if (/^(en|eth|enp|eno|ens|em|igb|dpni|bond)/.test(n)) return { kind: "ethernet", label: "以太网" };
  return { kind: "other", label: "其他" };
}

function collectIfaces(host: Host): NetIface[] {
  const info = host.host_info || {};
  const v4 = info.ipv4_by_iface || {};
  const v6 = info.ipv6_by_iface || {};
  const names = new Set<string>([...Object.keys(v4), ...Object.keys(v6)]);
  const out: NetIface[] = [];
  for (const name of names) {
    if (/^lo\d*$|^lo$|^utun[0-3]$/.test(name) && !(v4[name] || (v6[name] || []).length)) continue;
    if (name === "lo" || name === "lo0") continue;
    const { kind, label } = classifyIface(name, host.os, host.host_info?.has_battery);
    out.push({ name, kind, label, color: NET_COLOR[kind], glyph: NET_GLYPH[kind], ipv4: v4[name], ipv6: v6[name] || [] });
  }
  // physical first (wifi, cellular, ethernet, bridge, other), mesh last
  const rank: Record<NetKind, number> = { wifi: 0, cellular: 1, ethernet: 2, bridge: 3, other: 4, mesh: 5 };
  out.sort((a, b) => rank[a.kind] - rank[b.kind] || a.name.localeCompare(b.name));
  return out;
}

// renderHostNet paints the dark-NOC connectivity fan + the interface legend.
function renderHostNet(host: Host, online: boolean): void {
  const ifaces = collectIfaces(host);
  if (!ifaces.length) {
    hostsNetPanel.innerHTML = `<div class="chat-empty">尚未上报网络接口。等 polar-agent 重新 attach 一次就会自动登记。</div>`;
    return;
  }

  // ---- SVG fan: host node in center, each iface a neon spoke (shared kit) ----
  const W = 720;
  const H = 300;
  const cx = W / 2;
  const cy = H / 2;
  const ring = 108;
  const accent = online ? NEON.green : NOC.textMuted;
  const GLOW = "noc-glow"; // nocSvg/nocSpoke default filter id
  const pts = fanPositions(ifaces.length, cx, cy, ring);
  const spokes = ifaces.map((f, i) =>
    nocSpoke({ x1: cx, y1: cy, x2: pts[i].x, y2: pts[i].y, color: f.color, dashed: f.kind === "mesh" }),
  );
  // Interface nodes keep side-anchored name/IP labels (the host fan is
  // horizontal) rather than nocNode's below-node captions, so they stay bespoke.
  const nodes = ifaces.map((f, i) => {
    const { x, y } = pts[i];
    const labelRight = x >= cx;
    const lx = x + (labelRight ? 14 : -14);
    const ip = f.ipv4 || (f.ipv6[0] && f.ipv6[0].addr) || "";
    const anchor = labelRight ? "start" : "end";
    return (
      `<g filter="url(#${GLOW})"><circle cx="${x.toFixed(1)}" cy="${y.toFixed(1)}" r="16" fill="${NOC.nodeFill}" stroke="${f.color}" stroke-width="1.6"/>` +
      `<svg x="${(x - 8).toFixed(1)}" y="${(y - 8).toFixed(1)}" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="${f.color}" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${f.glyph}</svg></g>` +
      `<text x="${lx.toFixed(1)}" y="${(y - 2).toFixed(1)}" text-anchor="${anchor}" fill="${NOC.textDim}" font-size="11" font-weight="600">${escapeHTML(f.name)}</text>` +
      `<text x="${lx.toFixed(1)}" y="${(y + 11).toFixed(1)}" text-anchor="${anchor}" fill="${f.color}" font-size="10" font-family="monospace">${escapeHTML(ip)}</text>`
    );
  });
  const hostGlyph = deviceIconSVG(host, 26).replace(/stroke="#[0-9a-f]+"/i, `stroke="${accent}"`);
  const center =
    `<circle cx="${cx}" cy="${cy}" r="34" fill="${NOC.nodeFill}" stroke="${accent}" stroke-width="2" filter="url(#${GLOW})"/>` +
    `<circle cx="${cx}" cy="${cy}" r="42" fill="none" stroke="${accent}" stroke-width="1" opacity="0.35"/>` +
    `<g transform="translate(${cx - 13},${cy - 16})">${hostGlyph}</g>` +
    `<text x="${cx}" y="${cy + 22}" text-anchor="middle" fill="${NOC.textBright}" font-size="10" font-weight="600">${escapeHTML(host.name).slice(0, 16)}</text>`;
  const svg = nocSvg({
    width: W,
    height: H,
    inner: spokes.join("") + center + nodes.join(""),
    ariaLabel: "host network",
  });

  // ---- interface legend chips (shared nocChip) ----
  const wifiMac = host.host_info?.wifi_mac;
  const chips = ifaces
    .map((f) => {
      const lines: string[] = [];
      if (f.ipv4) {
        lines.push(`<div style="font-family:monospace;font-size:11px;color:${NOC.textDim}">${escapeHTML(f.ipv4)}</div>`);
      }
      const v6 = f.ipv6
        .map(
          (a) =>
            `<span style="font-family:monospace;font-size:10px;color:${NEON.slate}">${escapeHTML(a.addr)} <span style="color:${a.private ? NEON.amber : NEON.green}">${a.private ? "私网" : "公网"}</span></span>`,
        )
        .join("<br>");
      if (v6) lines.push(`<div style="margin-top:2px">${v6}</div>`);
      if (f.kind === "wifi" && wifiMac) {
        lines.push(`<div style="font-family:monospace;font-size:10px;color:${NOC.textMuted}">MAC ${escapeHTML(wifiMac)}</div>`);
      }
      return nocChip({ color: f.color, glyph: f.glyph, name: f.name, badge: f.label, lines });
    })
    .join("");

  hostsNetPanel.innerHTML = nocPanel({ svg, chipsHTML: chips });
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
        <div class="video-studio-project-item ${active}" data-host-id="${escapeHTML(h.id)}" style="display:flex; gap:10px; align-items:flex-start;">
          ${deviceBadge(h, online)}
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

  const online = isOnline(host);
  hostsName.innerHTML = `<span style="display:inline-flex; align-items:center; gap:8px;">${deviceIconSVG(host, 20)}${escapeHTML(host.name)}</span>`;
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

  // 网络拓扑 — dark-NOC connectivity panel: this host's physical NICs
  // (wifi/eth/cellular) + its wg mesh link, fanned out from a center node.
  renderHostNet(host, online);

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
