// Host module Phase 2 (UI) — xterm.js modal for the Shell skill.
//
// This is a SEPARATE top-level entry so the xterm.js bundle only
// loads when an admin actually clicks "Open shell". hosts.ts uses
// the browser's native dynamic import to fetch this module on
// demand:
//
//   const mod = await import("/scripts/hosts-shell.js");
//   mod.openShellModal(hostId, hostSkillId);
//
// The build script (ui/scripts/build.mjs) bundles every top-level
// ui/src/*.ts as its own /scripts/<name>.js entry, with splitting
// disabled — so xterm.js + addons get inlined here, NOT in hosts.js.

import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import "@xterm/xterm/css/xterm.css";

import { openHostShell } from "./api/hosts.js";

const MODAL_ID = "hostShellModal";

export interface OpenShellOptions {
  hostId: string;
  hostSkillId: number;
  hostName?: string;
}

// openShellModal: builds the modal DOM if missing, mints a run via
// POST /open, opens the WS, mounts xterm. Returns when the modal is
// dismissed (so the caller doesn't have to await — usually fire and
// forget).
export async function openShellModal(opts: OpenShellOptions): Promise<void> {
  const overlay = ensureModalDOM();
  const titleEl = overlay.querySelector<HTMLElement>("[data-shell-title]")!;
  const statusEl = overlay.querySelector<HTMLElement>("[data-shell-status]")!;
  const termContainer = overlay.querySelector<HTMLElement>("[data-shell-term]")!;
  const closeBtn = overlay.querySelector<HTMLButtonElement>("[data-shell-close]")!;

  titleEl.textContent = `${opts.hostName || opts.hostId} — bash`;
  statusEl.textContent = "正在请求 shell session…";
  termContainer.innerHTML = ""; // clear previous run
  overlay.removeAttribute("hidden");
  document.body.classList.add("modal-open");

  // Default size that fits the modal viewport; FitAddon resizes
  // before the first byte is sent so bash sees the right TTY size.
  const term = new Terminal({
    fontFamily: "ui-monospace, 'JetBrains Mono', monospace",
    fontSize: 13,
    cursorBlink: true,
    convertEol: false,
    theme: { background: "#0f172a", foreground: "#e2e8f0" },
  });
  const fitAddon = new FitAddon();
  const linksAddon = new WebLinksAddon();
  term.loadAddon(fitAddon);
  term.loadAddon(linksAddon);
  term.open(termContainer);
  setTimeout(() => fitAddon.fit(), 0);
  term.focus();

  let ws: WebSocket | null = null;
  let resizeDebounce: number | null = null;
  let disposed = false;

  const dispose = () => {
    if (disposed) return;
    disposed = true;
    if (ws && ws.readyState <= WebSocket.OPEN) {
      try {
        ws.close(1000, "user_closed");
      } catch {
        /* ignore */
      }
    }
    term.dispose();
    overlay.setAttribute("hidden", "");
    document.body.classList.remove("modal-open");
    window.removeEventListener("resize", onWindowResize);
    closeBtn.removeEventListener("click", dispose);
    document.removeEventListener("keydown", onKeyDown);
  };

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === "Escape" && (e.target as HTMLElement)?.tagName !== "TEXTAREA") {
      // Esc to close, BUT only if focus is outside the terminal — otherwise
      // Esc is a perfectly valid keystroke for the shell (e.g. vim).
      const focusInTerm = termContainer.contains(document.activeElement);
      if (!focusInTerm) {
        dispose();
      }
    }
  };

  const sendResize = () => {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    try {
      fitAddon.fit();
    } catch {
      return;
    }
    ws.send(JSON.stringify({ kind: "resize", rows: term.rows, cols: term.cols }));
  };

  const onWindowResize = () => {
    if (resizeDebounce) {
      window.clearTimeout(resizeDebounce);
    }
    resizeDebounce = window.setTimeout(sendResize, 120);
  };

  closeBtn.addEventListener("click", dispose);
  document.addEventListener("keydown", onKeyDown);
  window.addEventListener("resize", onWindowResize);

  // 1. POST /open to mint a run and get the WS URL.
  const fit = fitAddon.proposeDimensions() || { rows: 30, cols: 100 };
  const openRes = await openHostShell(opts.hostId, opts.hostSkillId, {
    rows: fit.rows,
    cols: fit.cols,
  });
  if (!openRes.response.ok || !openRes.data.ws_url) {
    const msg = openRes.data.error || `HTTP ${openRes.response.status}`;
    term.writeln(`\x1b[31m[polar-shell] open failed: ${msg}\x1b[0m`);
    statusEl.textContent = `open failed: ${msg}`;
    return;
  }
  statusEl.textContent = `connected · run_id=${openRes.data.run_id}`;

  // 2. Open the byte-bridge WS. binaryType=arraybuffer so we can
  //    feed Uint8Array straight to xterm.
  ws = new WebSocket(openRes.data.ws_url);
  ws.binaryType = "arraybuffer";

  ws.addEventListener("open", () => {
    term.writeln(`\x1b[2m[polar-shell] connected (run_id=${openRes.data.run_id})\x1b[0m`);
    sendResize(); // tell agent the real rows/cols once
  });
  ws.addEventListener("message", (ev) => {
    if (typeof ev.data === "string") {
      // Server doesn't send text frames to the browser; ignore.
      return;
    }
    term.write(new Uint8Array(ev.data));
  });
  ws.addEventListener("close", (ev) => {
    term.writeln(`\r\n\x1b[2m[polar-shell] session ended (code=${ev.code} ${ev.reason || ""})\x1b[0m`);
    statusEl.textContent = `closed · code=${ev.code}`;
    // Disable input rather than auto-dismiss; operator decides
    // when to close the modal.
    term.options.disableStdin = true;
  });
  ws.addEventListener("error", () => {
    term.writeln(`\r\n\x1b[31m[polar-shell] WS error\x1b[0m`);
  });

  // 3. Pipe keystrokes → WS as binary frames.
  term.onData((data) => {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(new TextEncoder().encode(data));
  });
  // Terminal-level resize (e.g. from xterm's own logic, not just window resize).
  term.onResize(() => sendResize());
}

function ensureModalDOM(): HTMLElement {
  const existing = document.getElementById(MODAL_ID);
  if (existing) return existing;
  const overlay = document.createElement("div");
  overlay.id = MODAL_ID;
  overlay.setAttribute("hidden", "");
  overlay.style.cssText = [
    "position:fixed",
    "inset:0",
    "background:rgba(0,0,0,0.85)",
    "z-index:9000",
    "display:flex",
    "flex-direction:column",
    "padding:24px",
  ].join(";");
  overlay.innerHTML = `
    <div style="display:flex; align-items:center; gap:12px; color:#e2e8f0; margin-bottom:12px;">
      <strong data-shell-title style="font-size:14px;">shell</strong>
      <span data-shell-status style="font-size:12px; color:#94a3b8;">—</span>
      <span style="flex:1"></span>
      <button data-shell-close type="button" class="btn-inline" style="background:#dc2626; color:#fff; border-color:#dc2626;">关闭 (Esc)</button>
    </div>
    <div data-shell-term style="flex:1; min-height:0; background:#0f172a; border-radius:8px; overflow:hidden;"></div>
    <div style="margin-top:8px; font-size:11px; color:#64748b;">⓵ session 限制：每个 host 最多 4 个并发 · 闲置 30min 自动断 · 硬上限 6h</div>
  `;
  document.body.appendChild(overlay);
  return overlay;
}
