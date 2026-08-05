import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { getCurrentWindow } from "@tauri-apps/api/window";
import type { UiState, Progress, FeedItem } from "./types";

// ---------------------------------------------------------------------------
// The webview is a pure view. All state lives in Rust and arrives via events;
// user actions are forwarded verbatim through the four commands. We keep only a
// tiny amount of local view state (the latest snapshot + whether Settings is open).
// ---------------------------------------------------------------------------

let state: UiState | null = null;
let progress: Progress | null = null;
let feed: FeedItem[] = [];
let settingsOpen = false;

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

function connectionDot(): string {
  const c = state?.connection ?? "connecting";
  const title = c === "ok" ? "Connected" : c === "connecting" ? "Connecting…" : "Connection error";
  return h(`<span class="dot dot-${c}" title="${title}"></span>`);
}

// --- First-run join screen ---------------------------------------------------

function renderFirstRun(): string {
  return h(`
    <section class="join">
      <div class="brand"><span class="emerald"></span><h1>JarJar</h1></div>
      <p class="muted">Join your pack to get started.</p>
      <label>Server URL
        <input id="join-url" type="url" placeholder="https://pack.example.com" />
      </label>
      <label>Invite code
        <input id="join-code" type="text" placeholder="AB2C3D4E" autocomplete="off" />
      </label>
      <label>Player name
        <input id="join-name" type="text" placeholder="alice" autocomplete="off" />
      </label>
      <button id="join-btn" class="primary">Join</button>
      <p id="join-err" class="error-text"></p>
    </section>
  `);
}

function wireFirstRun() {
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

// --- Main screen -------------------------------------------------------------

function renderUpdateCard(s: UiState): string {
  if (s.phase === "UpdateAvailable") {
    return h(`
      <div class="card update">
        <div class="card-title">v${s.current_version} available</div>
        <div class="summary">${esc(s.update_summary ?? "")}</div>
        <button id="apply-btn" class="primary" ${s.apply_enabled ? "" : "disabled"}>Apply update</button>
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
  return "";
}

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function renderRequestBox(s: UiState): string {
  const blocked = s.in_flight_request !== null;
  const tip = blocked
    ? `${esc(s.in_flight_request!.player_name)} is running: “${esc(s.in_flight_request!.text)}”`
    : "";
  return h(`
    <div class="requestbox">
      <textarea id="req-input" rows="2" placeholder="Ask for a change… e.g. 'add a minimap mod'"
        ${blocked ? "disabled" : ""}></textarea>
      <button id="send-btn" class="primary" ${blocked ? `disabled title="${tip}"` : ""}>Send</button>
      ${blocked ? `<div class="muted small">${tip}</div>` : ""}
      <div id="req-err" class="error-text"></div>
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
      </div>
      <div class="feed-body">${esc(it.text)}</div>
      ${answer}
    </li>
  `);
}

function renderSettings(s: UiState): string {
  return h(`
    <section class="settings">
      <div class="settings-head"><h2>Settings</h2><button id="close-settings" class="icon-btn">✕</button></div>
      <label>Server URL<input id="set-url" type="url" value="${esc(s.settings.server_url)}" /></label>
      <label>Player name<input type="text" value="${esc(s.settings.player_name ?? "")}" readonly /></label>
      <label>Minecraft folder override<input id="set-mc" type="text" value="${esc(s.settings.minecraft_dir ?? "")}" placeholder="(platform default)" /></label>
      <label class="row"><input id="set-autostart" type="checkbox" ${s.settings.autostart ? "checked" : ""}/> Launch at login</label>
      <div class="settings-actions">
        <button id="save-settings" class="primary">Save</button>
        <button id="reverify" class="secondary">Re-verify all files</button>
      </div>
    </section>
  `);
}

function renderMain(s: UiState): string {
  if (settingsOpen) return renderSettings(s);
  const notice = s.min_client_notice
    ? h(`<div class="notice">${esc(s.min_client_notice)}</div>`)
    : "";
  return h(`
    <header>
      <div class="title">
        <span class="emerald small"></span>
        <span>${esc(s.pack_name || "JarJar")}${s.applied_version != null ? ` · v${s.applied_version}` : ""}</span>
      </div>
      <div class="header-right">${connectionDot()}<button id="open-settings" class="icon-btn" title="Settings">⚙</button></div>
    </header>
    ${notice}
    ${renderUpdateCard(s)}
    ${renderRequestBox(s)}
    <ul class="feed">${feed.map(renderFeedItem).join("")}</ul>
  `);
}

// --- Wiring after each render ------------------------------------------------

function wireMain() {
  document.getElementById("apply-btn")?.addEventListener("click", () => invoke("apply_update"));
  document.getElementById("open-settings")?.addEventListener("click", () => {
    settingsOpen = true;
    render();
  });
  document.getElementById("close-settings")?.addEventListener("click", () => {
    settingsOpen = false;
    render();
  });

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

  // Settings save / re-verify
  document.getElementById("save-settings")?.addEventListener("click", async () => {
    const url = (document.getElementById("set-url") as HTMLInputElement).value.trim();
    const mc = (document.getElementById("set-mc") as HTMLInputElement).value.trim();
    const autostart = (document.getElementById("set-autostart") as HTMLInputElement).checked;
    await invoke("save_settings", {
      settings: { server_url: url, minecraft_dir: mc || null, autostart },
    });
    settingsOpen = false;
    render();
  });
  document.getElementById("reverify")?.addEventListener("click", async () => {
    await invoke("save_settings", { settings: { reverify: true } });
    settingsOpen = false;
    render();
  });
}

function render() {
  if (!state) {
    app.innerHTML = `<div class="loading">Connecting…</div>`;
    return;
  }
  if (state.phase === "FirstRun") {
    app.innerHTML = renderFirstRun();
    wireFirstRun();
    return;
  }
  app.innerHTML = renderMain(state);
  wireMain();
}

// --- Event subscriptions -----------------------------------------------------

async function boot() {
  await listen<UiState>("state_changed", (e) => {
    state = e.payload;
    render();
  });
  await listen<Progress>("progress", (e) => {
    progress = e.payload;
    // Only the progress bar needs updating; re-render the card cheaply.
    if (state?.phase === "Applying") render();
  });
  await listen<FeedItem[]>("feed_updated", (e) => {
    feed = e.payload;
    render();
  });

  // The Rust core re-emits `state_changed` + `feed_updated` shortly after the
  // window is created (see main.rs open_window), so a freshly-loaded webview
  // receives the current snapshot without needing an extra command. We keep the
  // command surface to exactly the five contract commands.
  render();

  // getCurrentWindow is imported for potential future use (e.g. an in-page close
  // button); the tray/close-request handling lives in Rust.
  void getCurrentWindow;
}

boot();
