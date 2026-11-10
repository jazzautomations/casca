/* casca — site. Idioma, copiar, e a imitação da janela: sessões que continuam
   trabalhando sozinhas e um botão que derruba a rede pra mostrar que nada se perde. */
(function () {
  "use strict";
  var d = document.documentElement;
  var $ = function (s, r) { return (r || document).querySelector(s); };
  function lang() { return d.getAttribute("data-lang") === "en" ? "en" : "pt"; }
  function T(pt, en) { return lang() === "en" ? en : pt; }

  /* idioma */
  var TITLE = { pt: "casca · um notebook de 2005 pilotando agentes de IA", en: "casca · a 2005 laptop flying AI agents" };
  function setLang(l, save) {
    d.setAttribute("data-lang", l); d.setAttribute("lang", l === "pt" ? "pt-BR" : "en");
    document.title = TITLE[l];
    if (save) { try { localStorage.setItem("casca-lang", l); } catch (e) {} }
  }
  (function () {
    var l = null;
    try { l = new URLSearchParams(location.search).get("lang"); } catch (e) {}
    if (l !== "pt" && l !== "en") { try { l = localStorage.getItem("casca-lang"); } catch (e) {} }
    if (l !== "pt" && l !== "en") l = /^pt\b/i.test(navigator.language || "pt") ? "pt" : "en";
    setLang(l, false);
  })();
  var lb = $("#lang");
  if (lb) lb.addEventListener("click", function () { setLang(lang() === "pt" ? "en" : "pt", true); paintAll(); });

  /* copiar */
  var cp = $("#copy");
  if (cp) cp.addEventListener("click", function () {
    var txt = $("#cmds").getAttribute("data-copy");
    if (navigator.clipboard) navigator.clipboard.writeText(txt).then(function () {
      var old = cp.textContent; cp.textContent = T("copiado", "copied");
      setTimeout(function () { cp.textContent = old; }, 1500);
    });
  });

  /* ---------- a janela ---------- */
  var win = $("#win");
  if (!win) return;
  var SPIN = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏";
  // cada sessão repete um roteiro: [classe, texto, espera em ms antes da próxima linha]
  var S = [
    { id: "c1", host: 0, agent: "claude", mark: "✻", name: "claude", cmd: "claude",
      title: ["consertando o parser", "fixing the parser"],
      script: [["b", "› conserte o teste que falha no parser", 900], ["", "● Bash(go test ./...)", 1400],
        ["r", "  ⎿  FAIL TestParseDuration: got \"90s\", want \"1m30s\"", 1100], ["", "● Read(duration.go)", 900],
        ["d", "  ⎿  64 linhas", 700], ["", "● Edit(duration.go)", 1500], ["d", "  ⎿  4 adições, 1 remoção", 800],
        ["", "● Bash(go test ./...)", 1700], ["ok", "  ⎿  ok   parser   0.412s", 900],
        ["", "● Corrigi formatDuration: acima de 60 s agora sai 1m30s.", 24000]] },
    { id: "x1", host: 1, agent: "codex", mark: "⬡", name: "codex", cmd: "codex",
      title: ["migração do banco", "database migration"],
      script: [["b", "› roda a migração em modo de teste e me diz o que muda", 1200], ["", "• lendo migrations/0042_orders.sql", 1300],
        ["", "• dry-run em staging", 2600], ["d", "  3 tabelas alteradas, 0 removidas", 900], ["w", "  aviso: índice orders_created_at vai ser recriado (~40 s)", 1500],
        ["", "• nada destrutivo. Posso aplicar?", 31000]] },
    { id: "d1", host: 1, agent: "devin", mark: "◆", name: "devin", cmd: "devin",
      title: ["PR #212: paginação", "PR #212: pagination"],
      script: [["b", "› abre um PR com paginação na listagem", 1000], ["", "▸ plano: 4 passos", 1100], ["d", "  1/4 endpoint aceita ?page e ?size", 2200],
        ["d", "  2/4 testes de borda (página vazia, size=0)", 2400], ["d", "  3/4 lint", 1300], ["ok", "  4/4 PR #212 aberto", 1000],
        ["", "▸ pronto. Esperando revisão.", 27000]] },
    { id: "c2", host: 2, agent: "claude", mark: "✻", name: "claude", cmd: "python",
      title: ["treino noturno", "overnight training"],
      script: [["b", "› treina 3 épocas e me avisa se a loss parar de cair", 1100], ["", "● Bash(python train.py --epochs 3)", 1600],
        ["d", "  época 1/3   loss 0.82", 2600], ["d", "  época 2/3   loss 0.51", 2600], ["d", "  época 3/3   loss 0.47", 2600],
        ["", "● A loss caiu nas três. Checkpoint salvo em runs/0611.", 35000]] }
  ];
  var HOSTS = ["desktop", "servidor", "gpu"];
  var cur = "c1", down = false, frame = 0, missed = 0;
  var reduce = window.matchMedia && matchMedia("(prefers-reduced-motion: reduce)").matches;
  S.forEach(function (s) { s.buf = []; s.i = 0; s.state = "idle"; s.last = Date.now(); });

  function byId(id) { for (var k = 0; k < S.length; k++) if (S[k].id === id) return S[k]; }
  function title(s) { return s.title[lang() === "en" ? 1 : 0]; }

  function step(s) {
    var ln = s.script[s.i];
    s.buf.push(ln); if (s.buf.length > 40) s.buf.shift();
    s.last = Date.now();
    s.i = (s.i + 1) % s.script.length;
    var endOfTurn = s.i === 0;
    if (endOfTurn) { if (s.buf.length) s.buf.push(["d", "", 0]); s.state = (s.id === cur && !down) ? "idle" : "news"; }
    else s.state = "work";
    if (s.id === cur) { if (down) missed++; else paintTerm(); }
    paintSide();
    s.timer = setTimeout(function () { step(s); }, ln[2]);
  }

  function paintSide() {
    HOSTS.forEach(function (h, hi) {
      var n = 0;
      S.forEach(function (s) { if (s.host === hi) n++; });
      $("#h" + hi + " .n").textContent = n;
    });
    S.forEach(function (s) {
      var b = $("#s-" + s.id);
      b.className = "sess" + (s.id === cur ? " on" : "") + (s.state === "work" ? " work" : "") + (s.state === "news" ? " news" : "");
      $(".st", b).textContent = s.state === "work" ? SPIN[frame] : (s.state === "news" ? "●" : "○");
      $(".ti", b).textContent = title(s);
      var sec = Math.floor((Date.now() - s.last) / 1000);
      $(".age", b).textContent = sec < 60 ? T("agora", "now") : Math.floor(sec / 60) + "m";
    });
    var c = byId(cur), tab = $("#tab");
    tab.className = "tab" + (c.state === "work" && !down ? " work" : "");
    $(".st", tab).textContent = c.state === "work" && !down ? SPIN[frame] : "●";
    $(".mk", tab).className = "mk " + c.agent; $(".mk", tab).textContent = c.mark;
    $(".ti", tab).textContent = HOSTS[c.host] + " · " + title(c);
    $("#crumb").textContent = HOSTS[c.host] + " · " + c.name;
    $("#foot-state").textContent = down ? T("sem rede", "offline") : HOSTS[c.host] + " · " + c.name + "   " +
      (c.state === "work" ? T("trabalhando", "working") : T("parado", "idle"));
    $("#net-label").textContent = down ? T("sem rede", "offline") : T("rede", "online");
  }

  var term = $("#term");
  function line(cls, text) { var e = document.createElement("div"); if (cls) e.className = cls; e.textContent = text || " "; term.appendChild(e); }
  function paintTerm(extra) {
    term.textContent = "";
    byId(cur).buf.slice(-18).forEach(function (l) { line(l[0], l[1]); });
    (extra || []).forEach(function (l) { line(l[0], l[1]); });
  }
  function paintAll() { paintSide(); if (!down) paintTerm(); }

  S.forEach(function (s) {
    $("#s-" + s.id).addEventListener("click", function () {
      if (down) return;
      cur = s.id; if (s.state === "news") s.state = "idle";
      paintAll();
    });
  });

  /* derrubar a rede: a aba para de receber, as sessões seguem; depois reanexa e mostra o que rolou */
  var kill = $("#kill"), killLabel = $("#kill-label");
  function outage() {
    if (down) return;
    down = true; missed = 0; win.classList.add("down"); kill.disabled = true;
    killLabel.textContent = T("reconectando…", "reconnecting…");
    var msgs = [["r", T("⚠ ssh caiu (255) — a sessão continua viva no tmux do host", "⚠ ssh died (255) — the session is still alive in tmux on the host")]];
    // se o agente da aba estava parado, ele pega serviço agora: a queda tem que pegar alguém trabalhando
    var c0 = byId(cur);
    if (!reduce && c0.state !== "work") { clearTimeout(c0.timer); c0.timer = setTimeout(function () { step(c0); }, 700); }
    paintSide(); paintTerm(msgs);
    var waits = [1, 2, 4], k = 0;
    (function attempt() {
      if (k < waits.length) {
        msgs.push(["w", T("  sem rede. Tentando de novo em ", "  no network. Retrying in ") + waits[k] + " s…"]);
        paintTerm(msgs);
        setTimeout(attempt, waits[k++] * 1000);
        return;
      }
      down = false; win.classList.remove("down");
      var c = byId(cur), n = missed;
      c.buf.push(["gap", T("✓ reanexado em c-" + c.name + ". " + (n ? n + " linha(s) saíram enquanto você estava fora — estão aí em cima." : "Nada saiu enquanto você estava fora."),
                          "✓ reattached to c-" + c.name + ". " + (n ? n + " line(s) were printed while you were away — they are right above." : "Nothing was printed while you were away.")), 0]);
      paintTerm();
      kill.disabled = false; killLabel.textContent = T("Derrubar o WiFi de novo", "Kill the WiFi again");
      paintSide();
    })();
  }
  kill.addEventListener("click", function () { outage(); win.scrollIntoView({ behavior: reduce ? "auto" : "smooth", block: "center" }); });

  // começa já com história na tela, depois cada sessão segue no seu ritmo
  S.forEach(function (s, k) { for (var j = 0; j < 3 + k; j++) { s.buf.push(s.script[s.i]); s.i = (s.i + 1) % s.script.length; } s.state = "work"; });
  paintAll();
  if (!reduce) {
    S.forEach(function (s, k) { s.timer = setTimeout(function () { step(s); }, 900 + k * 700); });
    setInterval(function () { frame = (frame + 1) % SPIN.length; paintSide(); }, 130);
  }
})();
