import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { getCurrentWindow } from "@tauri-apps/api/window";
import type {
  UiState,
  Progress,
  FeedItem,
  SetupStatus,
  StartSetupPayload,
  ModEntry,
  VersionEntry,
} from "./types";

// ---------------------------------------------------------------------------
// The webview is a view over Rust-owned state. The core state (phase, feed,
// progress) arrives via events; the richer dashboard data (setup status, mods,
// versions) is pulled on demand through invoke commands and cached here. All
// mutations still go through commands — the webview never owns durable state.
// ---------------------------------------------------------------------------

type Tab = "dashboard" | "mods" | "versions" | "settings";

let state: UiState | null = null;
let progress: Progress | null = null;
let feed: FeedItem[] = [];

// Local view state.
let tab: Tab = "dashboard";
let mods: ModEntry[] | null = null;
let modsError: string | null = null;
let modsLoading = false;
let modSearch = "";
let versions: VersionEntry[] | null = null;
let versionsError: string | null = null;
let versionsLoading = false;
let inviteCode: string | null = null;

// Setup wizard state.
let setup: SetupStatus | null = null;
let setupSubmitting = false;
// Latched true once we've successfully kicked off a setup run, so the live
// progress view stays put even if the first status poll hasn't recorded the job
// as running yet (avoids the form flashing back / a double submit).
let setupStarted = false;
let setupError: string | null = null;
let advancedOpen = false;
let setupPollTimer: number | null = null;
// Whether we've done the one-shot status fetch on entering the wizard (detects a
// setup already in progress from a previous session).
let setupChecked = false;

const app = document.getElementById("app")!;

function h(html: string): string {
  return html;
}

function esc(s: string): string {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]!),
  );
}

function relTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const secs = Math.max(0, (Date.now() - then) / 1000);
  if (secs < 60) return "just now";
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function isAdmin(s: UiState): boolean {
  return s.role === "admin";
}

function connectionDot(): string {
  const c = state?.connection ?? "connecting";
  const title = c === "ok" ? "Connected" : c === "connecting" ? "Connecting…" : "Connection error";
  return h(`<span class="dot dot-${c}" title="${esc(title)}"></span>`);
}

// ===========================================================================
// Connect screen (first run / join)
// ===========================================================================

function renderConnect(): string {
  return h(`
    <div class="screen-center">
      <section class="join">
        <div class="brand"><span class="emerald"></span><h1>JarJar</h1></div>
        <p class="muted">Connect to your server to get started. Admins: paste the server URL and
          admin invite code the daemon printed during install.</p>
        <label>Server URL
          <input id="join-url" type="url" placeholder="https://pack.example.com" />
        </label>
        <label>Invite / admin code
          <input id="join-code" type="text" placeholder="AB2C3D4E" autocomplete="off" />
        </label>
        <label>Your name
          <input id="join-name" type="text" placeholder="alice" autocomplete="off" />
        </label>
        <button id="join-btn" class="primary">Connect</button>
        <p id="join-err" class="error-text"></p>
      </section>
    </div>
  `);
}

function wireConnect() {
  const btn = document.getElementById("join-btn") as HTMLButtonElement;
  btn?.addEventListener("click", async () => {
    const url = (document.getElementById("join-url") as HTMLInputElement).value.trim();
    const code = (document.getElementById("join-code") as HTMLInputElement).value.trim();
    const name = (document.getElementById("join-name") as HTMLInputElement).value.trim();
    const err = document.getElementById("join-err")!;
    err.textContent = "";
    btn.disabled = true;
    try {
      // join is part of save_settings' first-run path — the Rust core owns it.
      await invoke("save_settings", {
        settings: { server_url: url, invite_code: code, player_name: name },
      });
    } catch (e) {
      err.textContent = String(e);
      btn.disabled = false;
    }
  });
}

// ===========================================================================
// Setup wizard (admin, before the server is configured)
// ===========================================================================

function renderSetupForm(): string {
  return h(`
    <form id="setup-form" class="setup-form">
      <label>Describe the modpack you want
        <textarea id="su-desc" rows="5"
          placeholder="e.g. A cozy tech + exploration pack for ~6 friends: Create, Applied Energistics, some world-gen and a minimap. Keep it stable, no super-heavy shaders."></textarea>
      </label>
      <label>Claude Code token
        <input id="su-token" type="password" placeholder="paste your token" autocomplete="off" />
        <span class="hint">Run <code>claude setup-token</code> on any machine with Claude Code and paste the result. This is the only token you need to paste.</span>
      </label>

      <button type="button" id="su-adv-toggle" class="disclosure">
        ${advancedOpen ? "▾" : "▸"} Advanced
      </button>
      <div class="advanced ${advancedOpen ? "" : "hidden"}">
        <div class="grid-2">
          <label>Server memory (MB)
            <input id="su-mem" type="number" value="9216" min="1024" step="512" />
          </label>
          <label>Server port
            <input id="su-port" type="number" value="25565" min="1" max="65535" />
          </label>
        </div>
        <label>Server directory
          <input id="su-dir" type="text" value="/opt/minecraft" />
        </label>
        <label class="row"><input id="su-cf" type="checkbox" /> Allow CurseForge mods</label>
        <label>CurseForge API key <span class="muted small">(only if enabling CurseForge)</span>
          <input id="su-cfkey" type="text" placeholder="optional" autocomplete="off" />
        </label>
      </div>

      <div class="setup-actions">
        <button type="submit" id="su-submit" class="primary" ${setupSubmitting ? "disabled" : ""}>
          ${setupSubmitting ? "Starting…" : "Create my server"}
        </button>
      </div>
      <p id="su-err" class="error-text">${setupError ? esc(setupError) : ""}</p>
    </form>
  `);
}

function setupFailed(st: SetupStatus): boolean {
  return !st.running && !st.configured && (!!st.error || st.steps.some((s) => s.error));
}

function stepIcon(s: { done: boolean; error: string | null }): string {
  if (s.error) return `<span class="step-icon err">✕</span>`;
  if (s.done) return `<span class="step-icon ok">✓</span>`;
  return `<span class="step-icon spin"></span>`;
}

function renderSetupProgress(st: SetupStatus): string {
  const steps =
    st.steps.length > 0
      ? st.steps
          .map(
            (s) => h(`
        <li class="step ${s.error ? "err" : s.done ? "done" : "active"}">
          ${stepIcon(s)}
          <div class="step-body">
            <div class="step-phase">${esc(s.phase)}</div>
            ${s.message ? `<div class="muted small">${esc(s.message)}</div>` : ""}
            ${s.error ? `<div class="error-text">${esc(s.error)}</div>` : ""}
          </div>
          ${s.at ? `<span class="muted small">${relTime(s.at)}</span>` : ""}
        </li>`),
          )
          .join("")
      : `<li class="muted">Warming up…</li>`;

  const failed = setupFailed(st);
  const err = st.error ? `<div class="notice error-notice">${esc(st.error)}</div>` : "";
  const phase = st.phase ? `<div class="muted">Current phase: <strong>${esc(st.phase)}</strong></div>` : "";
  const head = failed
    ? `<h2>Setup didn't finish</h2>`
    : `<span class="step-icon spin"></span><h2>Building your server…</h2>`;
  const blurb = failed
    ? `<p class="muted">Something went wrong during setup. Review the steps below, then try again.</p>`
    : `<p class="muted">The AI is picking mods, assembling the pack, and booting the server. This can take
        several minutes. You can leave this window open.</p>`;
  const retry = failed
    ? `<div class="setup-actions"><button id="su-back" class="secondary">Back to form</button></div>`
    : "";
  return h(`
    <div class="setup-progress">
      <div class="progress-head">${head}</div>
      ${blurb}
      ${phase}
      ${err}
      <ul class="steps">${steps}</ul>
      ${retry}
    </div>
  `);
}

function renderSetup(s: UiState): string {
  // Show live progress once a setup run exists (running, has steps, or errored);
  // otherwise show the form.
  const showProgress =
    setupStarted ||
    (!!setup && (setup.running || setup.steps.length > 0 || !!setup.error));
  const inner = showProgress ? renderSetupProgress(setup!) : renderSetupForm();
  return h(`
    <div class="screen-center wide">
      <section class="setup">
        <div class="brand"><span class="emerald"></span>
          <div>
            <h1>Set up ${esc(s.pack_name || "your server")}</h1>
            <p class="muted small">One-time setup — driven entirely from here.</p>
          </div>
        </div>
        ${inner}
      </section>
    </div>
  `);
}

function wireSetup() {
  document.getElementById("su-adv-toggle")?.addEventListener("click", () => {
    advancedOpen = !advancedOpen;
    render();
  });
  document.getElementById("su-back")?.addEventListener("click", () => {
    // Return to the form after a failed run so the admin can adjust + retry.
    stopSetupPoll();
    setupStarted = false;
    setupSubmitting = false;
    setup = null;
    setupError = null;
    render();
  });
  const form = document.getElementById("setup-form") as HTMLFormElement | null;
  form?.addEventListener("submit", async (e) => {
    e.preventDefault();
    const desc = (document.getElementById("su-desc") as HTMLTextAreaElement).value.trim();
    const token = (document.getElementById("su-token") as HTMLInputElement).value.trim();
    const err = document.getElementById("su-err")!;
    err.textContent = "";
    if (!desc) {
      err.textContent = "Please describe the modpack you want.";
      return;
    }
    if (!token) {
      err.textContent = "A Claude Code token is required.";
      return;
    }
    const payload: StartSetupPayload = { description: desc, claude_token: token };
    const mem = parseInt((document.getElementById("su-mem") as HTMLInputElement)?.value ?? "", 10);
    if (!Number.isNaN(mem)) payload.memory_mb = mem;
    const port = parseInt((document.getElementById("su-port") as HTMLInputElement)?.value ?? "", 10);
    if (!Number.isNaN(port)) payload.server_port = port;
    const dir = (document.getElementById("su-dir") as HTMLInputElement)?.value.trim();
    if (dir) payload.server_dir = dir;
    const cf = (document.getElementById("su-cf") as HTMLInputElement)?.checked ?? false;
    payload.allow_curseforge = cf;
    const cfkey = (document.getElementById("su-cfkey") as HTMLInputElement)?.value.trim();
    if (cf && cfkey) payload.curseforge_key = cfkey;

    setupSubmitting = true;
    setupError = null;
    render();
    try {
      await invoke("start_setup", { payload });
      setupStarted = true;
      // Kick an immediate status fetch; the poller then takes over.
      await pollSetupOnce();
    } catch (ex) {
      setupError = String(ex);
      setupSubmitting = false;
      render();
    }
  });
}

async function pollSetupOnce() {
  try {
    const st = await invoke<SetupStatus>("get_setup_status");
    setup = st;
    setupSubmitting = st.running;
    if (st.invite_code) inviteCode = st.invite_code;
    if (st.configured) {
      // Server is ready — force a reconcile so the app advances to the dashboard.
      stopSetupPoll();
      await invoke("refresh");
    }
    render();
  } catch (e) {
    // Transient errors are fine; the next tick retries.
    setupError = String(e);
    render();
  }
}

function startSetupPoll() {
  if (setupPollTimer !== null) return;
  setupPollTimer = window.setInterval(() => void pollSetupOnce(), 2000);
}

function stopSetupPoll() {
  if (setupPollTimer !== null) {
    window.clearInterval(setupPollTimer);
    setupPollTimer = null;
  }
}

/// Drive the wizard's polling: one initial status fetch on entry, then keep a 2s
/// poll running only while a setup run is active (not while showing the form and
/// not after a failure — where the admin drives via the retry button).
function manageSetupPoll() {
  if (!setupChecked) {
    setupChecked = true;
    void pollSetupOnce();
  }
  const failed = setup ? setupFailed(setup) : false;
  const active =
    !failed &&
    (setupStarted || (setup?.running ?? false) || (setup?.steps.length ?? 0) > 0);
  if (active) startSetupPoll();
  else stopSetupPoll();
}

// ===========================================================================
// Dashboard shell (connected + configured, or any player)
// ===========================================================================

function navItem(id: Tab, label: string): string {
  return h(`<button class="nav-item ${tab === id ? "active" : ""}" data-tab="${id}">${label}</button>`);
}

function renderShell(s: UiState): string {
  const version = s.applied_version != null ? `v${s.applied_version}` : s.current_version != null ? `v${s.current_version}` : "";
  return h(`
    <div class="topbar">
      <div class="title">
        <span class="emerald small"></span>
        <span class="pack">${esc(s.pack_name || "JarJar")}</span>
        ${s.loader_summary ? `<span class="muted small">${esc(s.loader_summary)}</span>` : ""}
        ${version ? `<span class="badge">${version}</span>` : ""}
      </div>
      <div class="header-right">
        ${connectionDot()}
        <span class="muted small">${s.role === "admin" ? "admin" : "player"}</span>
      </div>
    </div>
    <div class="shell">
      <nav class="nav">
        ${navItem("dashboard", "Dashboard")}
        ${navItem("mods", "Mods")}
        ${navItem("versions", "Versions")}
        ${navItem("settings", "Settings")}
        <div class="nav-spacer"></div>
        <button id="nav-start" class="nav-item start">▶ Start Minecraft</button>
      </nav>
      <main class="content">${renderTab(s)}</main>
    </div>
  `);
}

function renderTab(s: UiState): string {
  switch (tab) {
    case "mods":
      return renderMods();
    case "versions":
      return renderVersions(s);
    case "settings":
      return renderSettings(s);
    case "dashboard":
    default:
      return renderDashboard(s);
  }
}

// --- Dashboard tab -----------------------------------------------------------

function renderUpdateCard(s: UiState): string {
  if (s.phase === "UpdateAvailable") {
    return h(`
      <div class="card update">
        <div class="card-title">v${s.current_version} available</div>
        <div class="summary">${esc(s.update_summary ?? "")}</div>
        <button id="apply-btn" class="primary" ${s.apply_enabled ? "" : "disabled"}>Sync &amp; apply update</button>
      </div>
    `);
  }
  if (s.phase === "Applying") {
    const p = progress;
    const pct = p && p.bytes_total > 0 ? Math.round((p.bytes_done / p.bytes_total) * 100) : 0;
    const detail = p
      ? `${fmtBytes(p.bytes_done)} / ${fmtBytes(p.bytes_total)} · ${p.files_done}/${p.files_total} files`
      : "Preparing…";
    return h(`
      <div class="card update">
        <div class="card-title">Applying v${s.current_version}…</div>
        <div class="progress"><div class="bar" style="width:${pct}%"></div></div>
        <div class="muted small">${esc(p?.message ?? detail)}</div>
      </div>
    `);
  }
  if (s.phase === "ApplyError") {
    return h(`
      <div class="card update error">
        <div class="card-title">Update failed</div>
        <div class="error-text">${esc(s.error ?? "Unknown error")}</div>
        <button id="apply-btn" class="primary">Retry</button>
      </div>
    `);
  }
  if (s.phase === "UpToDate") {
    return h(`
      <div class="card">
        <div class="card-title">Your pack is up to date</div>
        <div class="muted small">${s.applied_version != null ? `Synced to v${s.applied_version}.` : "Nothing to sync yet."}</div>
      </div>
    `);
  }
  return "";
}

function renderRequestBox(s: UiState): string {
  const blocked = s.in_flight_request !== null;
  const tip = blocked
    ? `${esc(s.in_flight_request!.player_name)} is running: “${esc(s.in_flight_request!.text)}”`
    : "";
  return h(`
    <div class="card">
      <div class="card-title">Ask the AI for a change</div>
      <div class="requestbox">
        <textarea id="req-input" rows="2" placeholder="e.g. 'add a minimap mod' or 'make mobs less aggressive'"
          ${blocked ? "disabled" : ""}></textarea>
        <button id="send-btn" class="primary" ${blocked ? `disabled title="${tip}"` : ""}>Send</button>
        ${blocked ? `<div class="muted small">${tip}</div>` : ""}
        <div id="req-err" class="error-text"></div>
      </div>
    </div>
  `);
}

function renderFeedItem(it: FeedItem): string {
  if (it.kind === "version") {
    return h(`
      <li class="feed-item version">
        <div class="feed-head"><span class="badge">v${it.number}</span><span class="muted small">${relTime(it.created_at)}</span></div>
        <div class="feed-body">${esc(it.summary)}</div>
      </li>
    `);
  }
  const chipClass =
    it.status === "failed" || it.status === "rejected" || it.status === "infeasible"
      ? "chip-error"
      : it.status === "published"
        ? "chip-ok"
        : "chip-wait";
  const chipLabel =
    it.status === "awaiting_clarification"
      ? "waiting for answer"
      : it.status === "planning"
        ? "planning…"
        : it.status === "failed"
          ? `failed${it.error ? ": " + it.error : ""}`
          : it.status;
  const answer =
    it.awaiting_answer && it.question
      ? h(`
        <div class="answerbox">
          <div class="question">${esc(it.question)}</div>
          <input id="ans-input-${it.id}" type="text" placeholder="Your answer…" />
          <button class="secondary answer-btn" data-req="${it.id}">Answer</button>
        </div>`)
      : it.question
        ? h(`<div class="question muted">${esc(it.question)}</div>`)
        : "";
  return h(`
    <li class="feed-item request">
      <div class="feed-head">
        <span class="muted small">${esc(it.player_name)}</span>
        <span class="chip ${chipClass}">${esc(chipLabel)}</span>
        <span class="muted small">${relTime(it.created_at)}</span>
        ${it.version != null ? `<span class="badge">v${it.version}</span>` : ""}
      </div>
      <div class="feed-body">${esc(it.text)}</div>
      ${answer}
    </li>
  `);
}

function renderDashboard(s: UiState): string {
  const notice = s.min_client_notice
    ? h(`<div class="notice">${esc(s.min_client_notice)}</div>`)
    : "";
  const invite = isAdmin(s) ? renderInviteCard() : "";
  return h(`
    <div class="col">
      ${notice}
      ${renderUpdateCard(s)}
      ${renderRequestBox(s)}
      ${invite}
      <h3 class="section-title">Activity</h3>
      <ul class="feed">${feed.map(renderFeedItem).join("") || `<li class="muted">No activity yet.</li>`}</ul>
    </div>
  `);
}

function renderInviteCard(): string {
  return h(`
    <div class="card">
      <div class="card-title">Invite friends</div>
      <div class="muted small">Share a player invite code so friends can join with the JarJar app.</div>
      <div class="invite-row">
        ${inviteCode ? `<code class="invite-code" id="invite-code">${esc(inviteCode)}</code>` : `<span class="muted small">No code yet.</span>`}
        <button id="invite-btn" class="secondary">${inviteCode ? "New code" : "Create invite"}</button>
        ${inviteCode ? `<button id="invite-copy" class="secondary">Copy</button>` : ""}
      </div>
    </div>
  `);
}

// --- Mods tab ----------------------------------------------------------------

function sideBadge(side: string): string {
  const cls = side === "server" ? "chip-wait" : side === "both" ? "chip-ok" : "";
  return `<span class="chip ${cls}">${esc(side || "client")}</span>`;
}

function renderMods(): string {
  if (modsLoading && mods === null) {
    return `<div class="col"><div class="muted">Loading mods…</div></div>`;
  }
  if (modsError) {
    return h(`<div class="col"><div class="notice error-notice">${esc(modsError)}</div>
      <button id="mods-retry" class="secondary">Retry</button></div>`);
  }
  const all = mods ?? [];
  const q = modSearch.trim().toLowerCase();
  const filtered = q ? all.filter((m) => m.name.toLowerCase().includes(q)) : all;
  const rows = filtered
    .map(
      (m) => h(`
      <li class="mod-row">
        <div class="mod-main">
          <span class="mod-name">${esc(m.name)}</span>
          ${m.version ? `<span class="muted small">${esc(m.version)}</span>` : ""}
        </div>
        <div class="mod-meta">
          ${m.platform ? `<span class="muted small">${esc(m.platform)}</span>` : ""}
          ${sideBadge(m.side)}
        </div>
      </li>`),
    )
    .join("");
  return h(`
    <div class="col">
      <div class="mods-head">
        <h3 class="section-title">Mods <span class="muted small">(${filtered.length}${q ? ` of ${all.length}` : ""})</span></h3>
        <input id="mod-search" class="search" type="search" placeholder="Search mods…" value="${esc(modSearch)}" />
      </div>
      <ul class="mod-list">${rows || `<li class="muted">${all.length === 0 ? "No mods in this pack yet." : "No matches."}</li>`}</ul>
    </div>
  `);
}

// --- Versions tab ------------------------------------------------------------

function renderVersions(s: UiState): string {
  if (versionsLoading && versions === null) {
    return `<div class="col"><div class="muted">Loading versions…</div></div>`;
  }
  if (versionsError) {
    return h(`<div class="col"><div class="notice error-notice">${esc(versionsError)}</div>
      <button id="versions-retry" class="secondary">Retry</button></div>`);
  }
  const list = versions ?? [];
  const admin = isAdmin(s);
  const applied = s.applied_version;
  const rows = list
    .map((v) => {
      const isCurrent = applied != null && v.number === applied;
      const canRollback = admin && !isCurrent;
      return h(`
        <li class="ver-row">
          <div class="ver-head">
            <span class="badge">v${v.number}</span>
            ${v.status ? `<span class="chip ${v.status === "published" ? "chip-ok" : "chip-wait"}">${esc(v.status)}</span>` : ""}
            ${isCurrent ? `<span class="chip chip-ok">current</span>` : ""}
            <span class="muted small">${relTime(v.created_at)}</span>
            ${canRollback ? `<button class="secondary rollback-btn" data-ver="${v.number}">Roll back to v${v.number}</button>` : ""}
          </div>
          <div class="feed-body">${esc(v.summary)}</div>
        </li>`);
    })
    .join("");
  return h(`
    <div class="col">
      <h3 class="section-title">Versions</h3>
      <ul class="feed">${rows || `<li class="muted">No versions yet.</li>`}</ul>
    </div>
  `);
}

// --- Settings tab ------------------------------------------------------------

function renderSettings(s: UiState): string {
  return h(`
    <div class="col">
      <section class="settings">
        <h2>Settings</h2>
        <label>Server URL<input id="set-url" type="url" value="${esc(s.settings.server_url)}" /></label>
        <label>Your name<input type="text" value="${esc(s.settings.player_name ?? "")}" readonly /></label>
        <label>Minecraft folder override<input id="set-mc" type="text" value="${esc(s.settings.minecraft_dir ?? "")}" placeholder="(platform default)" /></label>
        <label class="row"><input id="set-autostart" type="checkbox" ${s.settings.autostart ? "checked" : ""}/> Launch at login</label>
        <div class="settings-actions">
          <button id="save-settings" class="primary">Save</button>
          <button id="reverify" class="secondary">Re-verify all files</button>
        </div>
      </section>
      ${isAdmin(s) ? renderInviteCard() : ""}
    </div>
  `);
}

// ===========================================================================
// Wiring (after each render of the shell)
// ===========================================================================

function wireShell() {
  document.querySelectorAll<HTMLButtonElement>(".nav-item[data-tab]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const next = btn.dataset.tab as Tab;
      if (next === tab) return;
      tab = next;
      render();
    });
  });
  document.getElementById("nav-start")?.addEventListener("click", () => invoke("start_minecraft"));

  wireDashboard();
  wireMods();
  wireVersions();
  wireSettings();
}

function wireDashboard() {
  document.getElementById("apply-btn")?.addEventListener("click", () => invoke("apply_update"));

  const sendBtn = document.getElementById("send-btn") as HTMLButtonElement | null;
  sendBtn?.addEventListener("click", async () => {
    const input = document.getElementById("req-input") as HTMLTextAreaElement;
    const text = input.value.trim();
    const err = document.getElementById("req-err")!;
    err.textContent = "";
    if (!text) return;
    sendBtn.disabled = true;
    try {
      await invoke("send_request", { text });
      input.value = "";
    } catch (e) {
      err.textContent = String(e);
    } finally {
      sendBtn.disabled = false;
    }
  });

  document.querySelectorAll<HTMLButtonElement>(".answer-btn").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const reqId = btn.dataset.req!;
      const input = document.getElementById(`ans-input-${reqId}`) as HTMLInputElement;
      const text = input.value.trim();
      if (!text) return;
      btn.disabled = true;
      try {
        await invoke("answer_question", { requestId: reqId, text });
        input.value = "";
      } catch (e) {
        alert(String(e));
        btn.disabled = false;
      }
    });
  });

  wireInvite();
}

function wireInvite() {
  document.getElementById("invite-btn")?.addEventListener("click", async () => {
    try {
      inviteCode = await invoke<string>("create_invite");
      render();
    } catch (e) {
      alert(String(e));
    }
  });
  document.getElementById("invite-copy")?.addEventListener("click", () => {
    if (inviteCode) void navigator.clipboard?.writeText(inviteCode);
  });
}

function wireMods() {
  document.getElementById("mods-retry")?.addEventListener("click", () => {
    modsError = null;
    mods = null;
    loadMods();
  });
  const search = document.getElementById("mod-search") as HTMLInputElement | null;
  search?.addEventListener("input", () => {
    modSearch = search.value;
    // Re-render list only; keep focus/caret by re-querying after render.
    render();
    const again = document.getElementById("mod-search") as HTMLInputElement | null;
    if (again) {
      again.focus();
      const v = again.value.length;
      again.setSelectionRange(v, v);
    }
  });
}

function wireVersions() {
  document.getElementById("versions-retry")?.addEventListener("click", () => {
    versionsError = null;
    versions = null;
    loadVersions();
  });
  document.querySelectorAll<HTMLButtonElement>(".rollback-btn").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const to = parseInt(btn.dataset.ver!, 10);
      if (Number.isNaN(to)) return;
      if (!confirm(`Roll the server back to v${to}? This creates a new version.`)) return;
      btn.disabled = true;
      try {
        await invoke("rollback", { toVersion: to });
        versions = null;
        loadVersions();
      } catch (e) {
        alert(String(e));
        btn.disabled = false;
      }
    });
  });
}

function wireSettings() {
  document.getElementById("save-settings")?.addEventListener("click", async () => {
    const url = (document.getElementById("set-url") as HTMLInputElement).value.trim();
    const mc = (document.getElementById("set-mc") as HTMLInputElement).value.trim();
    const autostart = (document.getElementById("set-autostart") as HTMLInputElement).checked;
    await invoke("save_settings", {
      settings: { server_url: url, minecraft_dir: mc || null, autostart },
    });
    render();
  });
  document.getElementById("reverify")?.addEventListener("click", async () => {
    await invoke("save_settings", { settings: { reverify: true } });
    render();
  });
  wireInvite();
}

// ===========================================================================
// On-demand data loaders
// ===========================================================================

async function loadMods() {
  if (modsLoading) return;
  modsLoading = true;
  render();
  try {
    mods = await invoke<ModEntry[]>("get_mods");
    modsError = null;
  } catch (e) {
    modsError = String(e);
  } finally {
    modsLoading = false;
    render();
  }
}

async function loadVersions() {
  if (versionsLoading) return;
  versionsLoading = true;
  render();
  try {
    versions = await invoke<VersionEntry[]>("list_versions");
    versionsError = null;
  } catch (e) {
    versionsError = String(e);
  } finally {
    versionsLoading = false;
    render();
  }
}

// ===========================================================================
// Top-level render + side-effect management
// ===========================================================================

function render() {
  if (!state) {
    stopSetupPoll();
    app.innerHTML = `<div class="screen-center"><div class="loading">Connecting…</div></div>`;
    return;
  }

  // 1. Not connected yet → connect screen.
  if (state.phase === "FirstRun") {
    stopSetupPoll();
    app.className = "connect";
    app.innerHTML = renderConnect();
    wireConnect();
    return;
  }

  // 2. Admin, server not configured → setup wizard (with live polling).
  if (isAdmin(state) && !state.configured) {
    app.className = "wizard";
    app.innerHTML = renderSetup(state);
    wireSetup();
    manageSetupPoll();
    return;
  }

  // 3. Connected + configured (or a player) → dashboard shell.
  stopSetupPoll();
  app.className = "shell-root";
  app.innerHTML = renderShell(state);
  wireShell();

  // Lazy-load the data for the active tab.
  if (tab === "mods" && mods === null && !modsLoading && !modsError) loadMods();
  if (tab === "versions" && versions === null && !versionsLoading && !versionsError) loadVersions();
}

// ===========================================================================
// Event subscriptions
// ===========================================================================

async function boot() {
  await listen<UiState>("state_changed", (e) => {
    const prev = state;
    state = e.payload;
    // When the pack version changes, invalidate cached lists so they refetch.
    if (prev && prev.current_version !== state.current_version) {
      mods = null;
      modsError = null;
      versions = null;
      versionsError = null;
    }
    render();
  });
  await listen<Progress>("progress", (e) => {
    progress = e.payload;
    if (state?.phase === "Applying" && tab === "dashboard") render();
  });
  await listen<FeedItem[]>("feed_updated", (e) => {
    feed = e.payload;
    if (tab === "dashboard") render();
  });

  // The Rust core re-emits `state_changed` + `feed_updated` shortly after the
  // window is created (see main.rs open_window), so a freshly-loaded webview
  // receives the current snapshot without needing an extra command.
  render();

  void getCurrentWindow;
}

boot();
