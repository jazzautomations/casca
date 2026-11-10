/* casca web UI. One websocket carries every terminal; the server owns the
   sessions, so any browser that connects sees the same tabs. */
"use strict";

const $ = (s) => document.querySelector(s);
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
};
const store = {
  get(k, d) { try { const v = localStorage.getItem(k); return v == null ? d : v; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch {} },
};
const touch = matchMedia("(pointer: coarse)").matches;

const state = {
  cfg: { hosts: [], agents: [], snippets: [] },
  status: [],          // per host: up | down | unknown
  tabs: new Map(),     // id -> tab
  active: null,
  ws: null,
  wsTry: 0,
  broadcast: false,
  split: false,
  font: parseInt(store.get("casca_font", touch ? "12" : "13"), 10),
  ctrl: false,
  alt: false,
  ackN: 0,
};

const newId = () => "t" + Math.random().toString(36).slice(2, 10) + Date.now().toString(36).slice(-4);

/* ---------- boot ---------- */
async function boot() {
  if (new URLSearchParams(location.search).has("bad")) $("#login-bad").classList.remove("hidden");
  let r;
  try { r = await fetch("/api/config"); } catch { setTimeout(boot, 2000); return; }
  if (r.status === 401) { $("#login").classList.remove("hidden"); return; }
  if (!r.ok) { toast("falha ao carregar a config", true); return; }
  state.cfg = await r.json();
  $("#app").classList.remove("hidden");
  renderHosts();
  renderSnippets();
  renderKeys();
  if (store.get("casca_keys", touch ? "1" : "0") === "1") toggleKeys(true);
  connectWS();
  pollStatus();
  setInterval(pollStatus, 30000);
  setInterval(flushAck, 400);
}

async function api(path, opts) {
  const r = await fetch(path, opts);
  if (r.status === 401) { location.reload(); throw new Error("sem sessão"); }
  if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
  return r.json();
}

async function pollStatus() {
  if (document.hidden) return;
  try { state.status = await api("/api/status"); renderHosts(); } catch {}
}

/* ---------- websocket ---------- */
function connectWS() {
  if (state.ws && state.ws.readyState <= 1) return;
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/ws`);
  ws.binaryType = "arraybuffer";
  state.ws = ws;

  ws.onopen = () => {
    state.wsTry = 0;
    state.ackN = 0;
    $("#offline").classList.add("hidden");
    // The server keeps no per-browser state: attach everything again and let
    // it repaint each terminal from scratch.
    for (const t of state.tabs.values()) { t.attached = false; }
    syncSessions();
  };

  ws.onmessage = (ev) => {
    if (ev.data instanceof ArrayBuffer) { onOutput(new Uint8Array(ev.data)); return; }
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    const t = state.tabs.get(m.id);
    switch (m.type) {
      case "state":
        if (t) { t.state = m.state; t.attempt = m.attempt; paintTab(t); }
        break;
      case "sessions":
        syncSessions();
        break;
      case "exists":
        dropTab(m.id);
        syncSessions().then(() => activate(m.existing));
        break;
      case "gone":
        dropTab(m.id);
        break;
      case "error":
        toast(m.error || "erro", true);
        dropTab(m.id);
        break;
    }
  };

  ws.onclose = () => {
    if (state.ws !== ws) return;
    $("#offline").classList.remove("hidden");
    const wait = Math.min(10000, 1000 * Math.pow(2, state.wsTry++));
    setTimeout(connectWS, wait);
  };
}

function wsSend(o) {
  if (state.ws && state.ws.readyState === 1) { state.ws.send(JSON.stringify(o)); return true; }
  return false;
}

// Output frame: [id length][id][terminal bytes].
function onOutput(u8) {
  const n = u8[0];
  let id = "";
  for (let i = 1; i <= n; i++) id += String.fromCharCode(u8[i]);
  const data = u8.subarray(1 + n);
  const t = state.tabs.get(id);
  if (!t) { state.ackN += data.length; return; }
  // Acknowledge only once xterm has parsed the chunk: that is what lets the
  // server pace a fast producer to a slow browser.
  t.term.write(data, () => {
    state.ackN += data.length;
    if (state.ackN >= 65536) flushAck();
  });
}

function flushAck() {
  if (state.ackN > 0 && wsSend({ type: "ack", n: state.ackN })) state.ackN = 0;
}

document.addEventListener("visibilitychange", () => {
  if (document.hidden) return;
  if (!state.ws || state.ws.readyState > 1) { state.wsTry = 0; connectWS(); }
  pollStatus();
  const t = state.tabs.get(state.active);
  if (t) clearBell(t);
});

/* ---------- sessions <-> tabs ---------- */
async function syncSessions() {
  let live;
  try { live = await api("/api/sessions"); } catch { return; }
  const ids = new Set(live.map((s) => s.id));
  for (const t of [...state.tabs.values()]) {
    if (!ids.has(t.id) && !t.pending) dropTab(t.id);
  }
  for (const s of live) {
    let t = state.tabs.get(s.id);
    if (!t) {
      t = makeTab(s.id, s.label, s.host);
      t.attached = false;
    }
    t.label = s.label; t.host = s.host; t.tmux = s.tmux; t.state = s.state; t.attempt = s.attempt;
    if (s.bell && s.id !== state.active) t.bell = true;
    delete t.pending;
    if (!t.attached) attachTab(t);
    paintTab(t);
  }
  if (!state.active || !state.tabs.has(state.active)) {
    const first = state.tabs.keys().next().value;
    if (first) activate(first);
    else showEmpty();
  }
  renderTabBar();
  renderHosts();
}

function attachTab(t) {
  t.term.reset();
  const d = dims(t);
  if (d) wsSend({ type: "resize", id: t.id, cols: d.cols, rows: d.rows });
  t.attached = wsSend({ type: "attach", id: t.id });
}

function dims(t) {
  try { const d = t.fit.proposeDimensions(); if (d && d.cols > 1 && d.rows > 1) return d; } catch {}
  return null;
}

function openSession(req, label) {
  const id = newId();
  const t = makeTab(id, label, req.host);
  t.pending = true;
  t.attached = true;
  t.state = "connecting";
  activate(id);
  const d = dims(t) || { cols: 80, rows: 24 };
  if (!wsSend({ type: "open", id, ...req, cols: d.cols, rows: d.rows })) {
    toast("sem conexão com o casca", true);
    dropTab(id);
  }
}

function makeTab(id, label, host) {
  const wrap = el("div", "term-wrap");
  const box = el("div", "term-box");
  const bar = el("div", "term-bar hidden");
  wrap.append(box, bar);
  $("#terms").appendChild(wrap);
  $("#empty").classList.add("hidden");

  const term = new Terminal({
    fontSize: state.font,
    fontFamily: '"JetBrains Mono", "DejaVu Sans Mono", Menlo, Consolas, monospace',
    theme: { background: "#0b0e13", foreground: "#e6edf3", cursor: "#58a6ff" },
    cursorBlink: !touch,
    scrollback: 1000,
    allowProposedApi: true,
    macOptionIsMeta: true,
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(box);

  const t = { id, label, host, term, fit, wrap, bar, state: "connecting", attempt: 0, bell: false, attached: false };
  state.tabs.set(id, t);

  term.onData((d) => sendInput(t, applyMods(d)));
  term.onResize(({ cols, rows }) => wsSend({ type: "resize", id, cols, rows }));
  term.onBell(() => { if (state.active !== id || document.hidden) { t.bell = true; paintTab(t); updateTitle(); } });
  term.onTitleChange((s) => { t.title = s; if (t.tabEl) t.tabEl.title = s; });
  term.attachCustomKeyEventHandler((e) => {
    if (e.type !== "keydown") return true;
    if (e.altKey && e.key.toLowerCase() === "k") { e.preventDefault(); openPalette(); return false; }
    if (e.ctrlKey && e.shiftKey && e.key.toLowerCase() === "c") {
      const s = term.getSelection();
      if (s && navigator.clipboard) navigator.clipboard.writeText(s).catch(() => {});
      e.preventDefault();
      return false;
    }
    if (e.ctrlKey && e.shiftKey && e.key.toLowerCase() === "v") { e.preventDefault(); pasteInto(t); return false; }
    return true;
  });
  wrap.addEventListener("pointerdown", () => { if (state.active !== id) activate(id); });

  renderTabBar();
  return t;
}

function sendInput(t, data) {
  const targets = state.broadcast ? [...state.tabs.values()] : [t];
  for (const x of targets) wsSend({ type: "stdin", id: x.id, data });
}

function activate(id) {
  const t = state.tabs.get(id);
  if (!t) return;
  state.active = id;
  for (const x of state.tabs.values()) {
    x.wrap.classList.toggle("active", x.id === id || state.split);
    x.wrap.classList.toggle("focused", x.id === id);
  }
  clearBell(t);
  renderTabBar();
  requestAnimationFrame(() => { refit(t); if (!touch) t.term.focus(); });
}

function refit(t) {
  if (!t.wrap.offsetParent) return;
  try { t.fit.fit(); } catch {}
}

function refitVisible() {
  for (const t of state.tabs.values()) if (t.id === state.active || state.split) refit(t);
}

function clearBell(t) {
  if (t.bell) { t.bell = false; paintTab(t); }
  updateTitle();
}

function updateTitle() {
  const n = [...state.tabs.values()].filter((t) => t.bell).length;
  document.title = (n ? `(${n}) ` : "") + "casca";
}

// closeTab ends the local connection only. A tmux session stays on the host.
function closeTab(id) {
  wsSend({ type: "close", id });
  dropTab(id);
}

function dropTab(id) {
  const t = state.tabs.get(id);
  if (!t) return;
  t.term.dispose();
  t.wrap.remove();
  state.tabs.delete(id);
  if (state.active === id) {
    state.active = null;
    const next = [...state.tabs.keys()].pop();
    if (next) activate(next);
  }
  if (!state.tabs.size) showEmpty();
  renderTabBar();
  updateTitle();
}

// With no tab open a phone would show a blank page: bring the host list out.
function showEmpty() {
  $("#empty").classList.remove("hidden");
  if ($("#sheet").classList.contains("hidden")) $("#sidebar").classList.add("open");
}

function paintTab(t) {
  if (t.tabEl) {
    t.tabEl.className = "tab st-" + t.state + (t.id === state.active ? " active" : "") + (t.bell ? " bell" : "");
  }
  const bar = t.bar;
  bar.textContent = "";
  if (t.state === "retry" || t.state === "dead") {
    bar.classList.remove("hidden");
    bar.appendChild(el("span", null, t.state === "retry"
      ? `rede caiu — reconectando (tentativa ${t.attempt}). O agente continua rodando no host.`
      : "sessão encerrada"));
    const again = el("button", null, t.state === "retry" ? "tentar agora" : "reconectar");
    again.onclick = () => wsSend({ type: "retry", id: t.id });
    bar.appendChild(again);
    if (t.state === "dead") {
      const x = el("button", null, "fechar aba");
      x.onclick = () => closeTab(t.id);
      bar.appendChild(x);
    }
  } else {
    bar.classList.add("hidden");
  }
}

function renderTabBar() {
  const bar = $("#tabs");
  bar.textContent = "";
  for (const t of state.tabs.values()) {
    const e = el("div", "tab");
    e.appendChild(el("span", "dot"));
    e.appendChild(el("span", "lbl", t.label));
    const x = el("span", "x", "×");
    x.title = "fechar aba (o tmux continua no host)";
    x.onclick = (ev) => { ev.stopPropagation(); closeTab(t.id); };
    e.appendChild(x);
    e.onclick = () => activate(t.id);
    if (t.title) e.title = t.title;
    t.tabEl = e;
    paintTab(t);
    bar.appendChild(e);
    if (t.id === state.active) e.scrollIntoView({ block: "nearest", inline: "nearest" });
  }
}

/* ---------- hosts ---------- */
function renderHosts() {
  const list = $("#host-list");
  list.textContent = "";
  const filter = ($("#host-filter").value || "").toLowerCase();
  const groups = new Map();
  state.cfg.hosts.forEach((h, i) => {
    if (filter && !(h.name + " " + h.target).toLowerCase().includes(filter)) return;
    const g = h.group || "hosts";
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push({ h, i });
  });
  const open = new Map();
  for (const t of state.tabs.values()) open.set(t.host, (open.get(t.host) || 0) + 1);
  for (const [g, items] of groups) {
    list.appendChild(el("div", "hgroup", g));
    for (const { h, i } of items) {
      const row = el("div", "host");
      row.appendChild(el("span", "hdot " + (state.status[i] || "unknown")));
      const txt = el("div", "htxt");
      txt.appendChild(el("div", "hname", h.name));
      txt.appendChild(el("div", "target", h.target));
      row.appendChild(txt);
      if (open.get(i)) row.appendChild(el("span", "count", String(open.get(i))));
      row.onclick = () => openHostSheet(i);
      list.appendChild(row);
    }
  }
  if (!state.cfg.hosts.length) list.appendChild(el("div", "hgroup", "nenhum host ainda"));
}
$("#host-filter").oninput = renderHosts;
$("#btn-add-host").onclick = () => openHostForm(-1);

/* ---------- sheets (modal panels) ---------- */
function showSheet(build) {
  const s = $("#sheet");
  s.textContent = "";
  build(s);
  s.classList.remove("hidden");
  $("#scrim").classList.remove("hidden");
  $("#sidebar").classList.remove("open");
}
function closeSheet() {
  $("#sheet").classList.add("hidden");
  $("#palette").classList.add("hidden");
  $("#scrim").classList.add("hidden");
  const t = state.tabs.get(state.active);
  if (t && !touch) t.term.focus();
}
$("#scrim").onclick = closeSheet;

function ago(sec) {
  const d = Math.max(0, Date.now() / 1000 - sec);
  if (d < 90) return "agora";
  if (d < 5400) return Math.round(d / 60) + " min";
  if (d < 129600) return Math.round(d / 3600) + " h";
  return Math.round(d / 86400) + " d";
}

function openHostSheet(i) {
  const h = state.cfg.hosts[i];
  showSheet((s) => {
    const head = el("div", "sheet-head");
    head.appendChild(el("h3", null, h.name));
    head.appendChild(el("span", "target", h.target));
    const edit = el("button", "ghost", "editar");
    edit.onclick = () => openHostForm(i);
    head.appendChild(edit);
    s.appendChild(head);

    s.appendChild(el("div", "sec", "abrir"));
    const grid = el("div", "agent-grid");
    const btns = new Map();
    state.cfg.agents.forEach((a, ai) => {
      const b = el("button", "agent");
      b.appendChild(el("b", null, a.name));
      const sub = el("small", null, a.tmux ? "tmux" : "sem tmux");
      b.appendChild(sub);
      b.onclick = () => { closeSheet(); openSession({ host: i, agent: ai }, `${h.name} · ${a.name}`); };
      grid.appendChild(b);
      if (a.tmux) btns.set("c-" + a.name.replace(/[^A-Za-z0-9_-]+/g, "-"), { b, sub });
    });
    s.appendChild(grid);

    const liveSec = el("div", "sec", "rodando no host");
    const live = el("div", "live");
    live.appendChild(el("div", "hint", "consultando…"));
    s.append(liveSec, live);

    s.appendChild(el("div", "sec", "sessão com nome, pasta ou comando próprio"));
    const f = el("form", "adv");
    const cmd = input("comando (vazio = shell)", "");
    const name = input("nome da sessão (ex: api-refactor)", "");
    const dir = input("pasta no host", h.dir || "");
    const go = el("button", null, "abrir");
    go.type = "submit";
    f.append(cmd, name, dir, go);
    f.onsubmit = (ev) => {
      ev.preventDefault();
      const c = cmd.value.trim();
      const n = name.value.trim() || (c ? c.split(/\s+/)[0] : "shell");
      closeSheet();
      if (c) openSession({ host: i, agent: -1, cmd: c, tmux: true, name: n, dir: dir.value.trim() }, `${h.name} · ${n}`);
      else openSession({ host: i, agent: 0, name: n, dir: dir.value.trim() }, `${h.name} · ${n}`);
    };
    s.appendChild(f);

    api("/api/remote?host=" + i).then((r) => {
      live.textContent = "";
      if (r.error) { live.appendChild(el("div", "hint", "não consegui consultar: " + r.error)); return; }
      if (!r.sessions.length) { live.appendChild(el("div", "hint", "nenhuma sessão tmux viva")); return; }
      for (const rs of r.sessions) {
        const known = btns.get(rs.name);
        if (known) { known.b.classList.add("running"); known.sub.textContent = "rodando · " + rs.command; }
        const row = el("div", "live-row");
        row.appendChild(el("span", "hdot up"));
        const txt = el("div", "htxt");
        txt.appendChild(el("div", "hname", rs.name));
        txt.appendChild(el("div", "target", `${rs.command} · atividade ${ago(rs.activity)}` + (rs.attached ? " · aberta" : "")));
        row.appendChild(txt);
        const at = el("button", null, "abrir");
        at.onclick = () => { closeSheet(); openSession({ host: i, attach: true, name: rs.name }, `${h.name} · ${rs.name}`); };
        const kill = el("button", "danger", "encerrar");
        kill.onclick = async () => {
          if (!confirm(`Encerrar a sessão "${rs.name}" em ${h.name}? O que estiver rodando nela morre.`)) return;
          try {
            await api(`/api/kill?host=${i}&name=${encodeURIComponent(rs.name)}`, { method: "POST" });
            row.remove();
          } catch (e) { toast(e.message, true); }
        };
        row.append(at, kill);
        live.appendChild(row);
      }
    }).catch((e) => { live.textContent = ""; live.appendChild(el("div", "hint", e.message)); });
  });
}

function input(ph, val) {
  const i = el("input");
  i.placeholder = ph; i.value = val || ""; i.autocomplete = "off"; i.autocapitalize = "none"; i.spellcheck = false;
  return i;
}

function openHostForm(i) {
  const h = i >= 0 ? state.cfg.hosts[i] : { name: "", target: "", group: "", dir: "", args: "" };
  showSheet((s) => {
    s.appendChild(el("h3", null, i >= 0 ? "editar host" : "novo host"));
    const f = el("form", "adv");
    const target = input("user@host (ou local)", h.target);
    const name = input("nome", h.name);
    const group = input("grupo", h.group);
    const dir = input("pasta padrão", h.dir);
    const args = input("argumentos ssh extras (ex: -p 2222)", h.args);
    const save = el("button", null, "salvar");
    save.type = "submit";
    f.append(target, name, group, dir, args, save);
    if (i >= 0) {
      const del = el("button", "danger", "remover host");
      del.type = "button";
      del.onclick = async () => {
        if (!confirm(`Remover ${h.name} da lista?`)) return;
        try { state.cfg = await api("/api/hosts?i=" + i, { method: "DELETE" }); closeSheet(); renderHosts(); pollStatus(); }
        catch (e) { toast(e.message, true); }
      };
      f.appendChild(del);
    }
    f.onsubmit = async (ev) => {
      ev.preventDefault();
      const body = JSON.stringify({ target: target.value.trim(), name: name.value.trim(), group: group.value.trim(),
        dir: dir.value.trim(), args: args.value.trim() });
      try {
        state.cfg = await api("/api/hosts" + (i >= 0 ? "?i=" + i : ""), { method: "POST", body });
        closeSheet(); renderHosts(); pollStatus();
      } catch (e) { toast(e.message, true); }
    };
    s.appendChild(f);
    target.focus();
  });
}

/* ---------- extra keys (what a phone keyboard lacks) ---------- */
const KEYS = [
  ["esc", "\x1b"], ["tab", "\t"], ["⇧tab", "\x1b[Z"], ["ctrl", "CTRL"], ["alt", "ALT"],
  ["←", "LEFT"], ["↓", "DOWN"], ["↑", "UP"], ["→", "RIGHT"],
  ["^C", "\x03"], ["^D", "\x04"], ["^B", "\x02"], ["^R", "\x12"], ["^Z", "\x1a"],
  ["pgup", "\x1b[5~"], ["pgdn", "\x1b[6~"], ["home", "\x1b[H"], ["end", "\x1b[F"],
  ["/", "/"], ["-", "-"], ["|", "|"], ["~", "~"], ["colar", "PASTE"], ["📎", "FILE"],
];
const ARROW = { UP: "A", DOWN: "B", RIGHT: "C", LEFT: "D" };

function renderKeys() {
  const row = $("#keys");
  row.textContent = "";
  for (const [label, seq] of KEYS) {
    const b = el("button", "key", label);
    b.dataset.seq = seq;
    // pointerdown + preventDefault keeps focus in the terminal, so the
    // on-screen keyboard does not close on every tap.
    b.addEventListener("pointerdown", (ev) => { ev.preventDefault(); pressKey(seq, b); });
    row.appendChild(b);
  }
}

function pressKey(seq, btn) {
  const t = state.tabs.get(state.active);
  if (seq === "CTRL") { state.ctrl = !state.ctrl; btn.classList.toggle("on", state.ctrl); return; }
  if (seq === "ALT") { state.alt = !state.alt; btn.classList.toggle("on", state.alt); return; }
  if (!t) return;
  if (seq === "PASTE") { pasteInto(t); return; }
  if (seq === "FILE") { $("#file-input").click(); return; }
  if (ARROW[seq]) {
    const app = t.term.modes && t.term.modes.applicationCursorKeysMode;
    seq = (app ? "\x1bO" : "\x1b[") + ARROW[seq];
  }
  sendInput(t, applyMods(seq));
}

// applyMods turns the next typed character into Ctrl+x / Alt+x when the
// sticky modifiers are armed.
function applyMods(d) {
  if (!state.ctrl && !state.alt) return d;
  if (d.length === 1) {
    if (state.ctrl) {
      const c = d.toUpperCase().charCodeAt(0);
      if (c >= 63 && c <= 95) d = String.fromCharCode(c === 63 ? 127 : c & 0x1f);
      else if (d === " ") d = "\x00";
    }
    if (state.alt) d = "\x1b" + d;
  }
  state.ctrl = state.alt = false;
  for (const b of document.querySelectorAll("#keys .key.on")) b.classList.remove("on");
  return d;
}

function toggleKeys(on) {
  const k = $("#keys");
  const show = on != null ? on : k.classList.contains("hidden");
  k.classList.toggle("hidden", !show);
  $("#btn-keys").classList.toggle("on", show);
  store.set("casca_keys", show ? "1" : "0");
  requestAnimationFrame(refitVisible);
}
$("#btn-keys").onclick = () => toggleKeys();

async function pasteInto(t) {
  let text = null;
  if (navigator.clipboard && navigator.clipboard.readText) {
    try { text = await navigator.clipboard.readText(); } catch {}
  }
  // Plain http on a tailnet address is not a "secure context", so browsers
  // hide the clipboard API there. Ask instead.
  if (text == null) text = prompt("colar:");
  if (text) t.term.paste(text);
}

/* ---------- file / screenshot upload ---------- */
async function uploadTo(t, file) {
  if (!file) return;
  if (file.size > 25 * 1024 * 1024) { toast("arquivo maior que 25 MB", true); return; }
  toast(`enviando ${file.name || "imagem"}…`);
  try {
    const r = await api(`/api/upload?host=${t.host}&name=${encodeURIComponent(file.name || "paste.png")}`,
      { method: "POST", body: file });
    sendInput(t, r.path + " ");
    toast("enviado: " + r.path);
  } catch (e) { toast(e.message, true); }
}
$("#file-input").onchange = (ev) => {
  const t = state.tabs.get(state.active);
  if (t) uploadTo(t, ev.target.files[0]);
  ev.target.value = "";
};
document.addEventListener("paste", (ev) => {
  const t = state.tabs.get(state.active);
  const f = [...(ev.clipboardData ? ev.clipboardData.files : [])][0];
  if (t && f) { ev.preventDefault(); ev.stopPropagation(); uploadTo(t, f); }
}, true);

/* ---------- snippets ---------- */
function renderSnippets() {
  const bar = $("#snippets");
  bar.textContent = "";
  for (const s of state.cfg.snippets || []) {
    const b = el("button", "snip", s.label);
    b.title = s.cmd;
    b.addEventListener("pointerdown", (ev) => {
      ev.preventDefault();
      const t = state.tabs.get(state.active);
      if (t) sendInput(t, s.cmd);
    });
    bar.appendChild(b);
  }
  $("#snippets").classList.toggle("hidden", !(state.cfg.snippets || []).length);
}

/* ---------- palette ---------- */
function paletteItems(filter) {
  const f = filter.toLowerCase();
  const hit = (s) => !f || s.toLowerCase().includes(f);
  const items = [];
  for (const t of state.tabs.values()) {
    if (hit(t.label)) items.push({ kind: "aba", label: t.label, run: () => activate(t.id) });
  }
  state.cfg.hosts.forEach((h, hi) => {
    state.cfg.agents.forEach((a, ai) => {
      const s = `${h.name} · ${a.name}`;
      if (hit(s)) items.push({ kind: "abrir", label: s, run: () => openSession({ host: hi, agent: ai }, s) });
    });
  });
  for (const s of state.cfg.snippets || []) {
    if (hit(s.label)) items.push({ kind: "snippet", label: s.label, run: () => {
      const t = state.tabs.get(state.active);
      if (t) sendInput(t, s.cmd);
    } });
  }
  return items.slice(0, 30);
}

let palSel = 0;
let palItems = [];
function openPalette() {
  $("#palette").classList.remove("hidden");
  $("#scrim").classList.remove("hidden");
  const inp = $("#palette-input");
  inp.value = "";
  renderPalette("");
  inp.focus();
}
function renderPalette(f) {
  palSel = 0;
  palItems = paletteItems(f);
  const list = $("#palette-list");
  list.textContent = "";
  palItems.forEach((it, i) => {
    const row = el("div", "pal-item" + (i === 0 ? " sel" : ""));
    row.appendChild(el("span", null, it.label));
    row.appendChild(el("span", "pal-kind", it.kind));
    row.onclick = () => { closeSheet(); it.run(); };
    list.appendChild(row);
  });
}
$("#palette-input").oninput = (e) => renderPalette(e.target.value);
$("#palette-input").onkeydown = (e) => {
  const rows = [...$("#palette-list").children];
  if (e.key === "ArrowDown" || e.key === "ArrowUp") {
    e.preventDefault();
    if (!rows.length) return;
    palSel = (palSel + (e.key === "ArrowDown" ? 1 : -1) + rows.length) % rows.length;
    rows.forEach((r, i) => r.classList.toggle("sel", i === palSel));
    rows[palSel].scrollIntoView({ block: "nearest" });
  } else if (e.key === "Enter" && palItems[palSel]) {
    const it = palItems[palSel];
    closeSheet();
    it.run();
  } else if (e.key === "Escape") closeSheet();
};
document.addEventListener("keydown", (e) => {
  if (e.altKey && e.key.toLowerCase() === "k") { e.preventDefault(); openPalette(); }
  else if (e.key === "Escape" && !$("#scrim").classList.contains("hidden")) closeSheet();
});

/* ---------- topbar ---------- */
$("#btn-palette").onclick = openPalette;
$("#hamburger").onclick = () => $("#sidebar").classList.toggle("open");

$("#btn-split").onclick = () => {
  state.split = !state.split;
  $("#terms").classList.toggle("split", state.split);
  $("#btn-split").classList.toggle("on", state.split);
  if (state.active) activate(state.active);
  requestAnimationFrame(refitVisible);
};

$("#btn-broadcast").onclick = () => {
  state.broadcast = !state.broadcast;
  $("#btn-broadcast").classList.toggle("on", state.broadcast);
  toast(state.broadcast ? "o que você digitar vai pra TODAS as abas" : "digitação só na aba ativa");
};

function setFont(delta) {
  state.font = Math.max(8, Math.min(24, state.font + delta));
  store.set("casca_font", String(state.font));
  for (const t of state.tabs.values()) t.term.options.fontSize = state.font;
  requestAnimationFrame(refitVisible);
}
$("#btn-font-dn").onclick = () => setFont(-1);
$("#btn-font-up").onclick = () => setFont(1);

/* ---------- layout ---------- */
let fitTimer;
function scheduleFit() {
  clearTimeout(fitTimer);
  fitTimer = setTimeout(refitVisible, 100);
}
window.addEventListener("resize", scheduleFit);
// When a phone keyboard opens, only the visual viewport shrinks. Size the app
// to it so the prompt stays above the keyboard.
if (window.visualViewport) {
  const vv = window.visualViewport;
  const apply = () => {
    document.documentElement.style.setProperty("--vh", vv.height + "px");
    window.scrollTo(0, 0);
    scheduleFit();
  };
  vv.addEventListener("resize", apply);
  apply();
}

let toastTimer;
function toast(msg, err) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.toggle("err", !!err);
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), err ? 5000 : 2500);
}

boot();
