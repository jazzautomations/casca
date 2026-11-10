I now have a comprehensive picture from official docs, changelogs, blog posts, and user reviews. Here's the full report.

---

# Termius Deep-Research Report — for "casca"

Sources: docs.termius.com (including its `llms-full.txt` full-doc dump), termius.com/blog, Termius changelogs (desktop 9.x / iOS 6.x / Android 7.x), App Store / Play Store reviews, HN threads, LowEndTalk, comparison articles.

## 1. Core UX model

**Data hierarchy** (docs.termius.com — "What is Termius"):
- **Vaults** — top-level E2E-encrypted containers holding Hosts, Groups, Keys, Snippets, Port-forwarding rules, Known Hosts. Personal vault (private) + auto-created Team vault + custom vaults with per-member permissions ("Can edit" / "Can view").
- **Groups** — *nested* collections of hosts. Explicitly "not folders": the Hosts screen shows all hosts in the current group *and its subgroups*. Groups can define defaults for protocols, credentials, **startup snippets, host chaining, proxy, env vars, terminal themes** — all inherited via config-merging unless overridden per-host. Right-click a group → "Quick Connect" opens a tab per host.
- **Tags** — orthogonal, non-hierarchical. A host has one parent group but many tags; tag filter = show hosts having *all* selected tags. Groups = structure, tags = filtering.
- **Host** — one machine, *multiple protocols* (SSH, Mosh, SFTP, Telnet, Serial), each with own port/creds. Fields: address, label, tags, credentials/identity, host chaining, proxy, env vars, startup command, agent forwarding, theme. Desktop auto-saves edits (no Save button). OS is auto-detected and reflected in the host icon after first connect.

**Navigation** (desktop 9.0+, "Termius X" redesign): horizontal tabs across the top — Vault, SFTP, and each connection is a tab. Mobile has a Connections screen listing all active sessions (SSH, SFTP, port-forwarding) as tabs. New Tab screen (`Cmd/Ctrl+T`) lets you just start typing to fuzzy-search hosts — no palette invocation needed; also shows Recent connections and lets you multi-select recents → "Create a workspace".

**Workspaces** (desktop only): groups of ≥2 sessions with two modes — **Focus mode** (one terminal full-screen, others as a side list with working-dir/active-command status and a notify dot when background commands finish) and **Split view** (up to 16 panes). `Cmd+Shift+M` toggles; `Cmd+Opt+arrows` cycles panes; `Cmd+S` saves a **workspace template** capturing connections, tab names, layout, *working directories and running commands*. **Broadcast input** sends keystrokes/snippets to all panes.

**Connection flow**: double-click/tap host → first connect shows server fingerprint → "Add and continue" (newer builds store SHA256; some docs note silent storing). Password prompts are *detected* in the terminal → autocomplete suggests saved Identities. **Host chaining** = multi-hop jump list per host or per group (equivalent to `ProxyJump`); HTTP/SOCKS5 proxy also per host/group. `ProxyCommand` is *not* supported/imported. Quick connect: type `ssh user@host` in the search bar without saving a host.

## 2. Snippets

- Labeled shell scripts in the vault; organized into **snippet packages**.
- Run modes: (a) side panel in terminal (`Ctrl/Cmd+.`) → hover → **PASTE** (inserts without Enter) or **RUN**; double-click runs; (b) **Autocomplete** — typing snippet name or any script text surfaces it; Tab can paste suggestions; (c) **multi-host execution** from the Snippets screen — select targets → Termius opens a *workspace* of new connections and runs it everywhere; savable default target set ("Save targets"); "Run in all tabs" from side panel runs in existing sessions; (d) **startup snippet** per host/group (`Startup Command` field) — the docs literally suggest `tmux attach -t <name>`; (e) AI-generate a snippet from a description.
- Mobile: tap = run immediately; long-press = preview → Paste / Edit. Command history panel lets you re-run/paste or *promote* a history entry into a snippet.

## 3. Terminal features

- **Autocomplete**: two engines — Hydrogen (stable: history, paths, snippets, *password-prompt detection → suggests identities*) and Helium (new UX, "latency-free input"). Powered by **shell integration**: Termius opens extra SSH `exec` channels to read cwd, history, prompt state — needs OpenSSH 7.2+ and bash remotely. Also detects sudo/password prompts.
- **AI**: press Down-Arrow → describe in English → generates command (account-required). Mobile AI widget above keyboard: AI mode + "Paste mode" voice dictation with per-session prompt history; select terminal text → "AI" to feed as context.
- **Paste/links**: OSC 52 supported for clipboard; terminal links require `Cmd/Ctrl`+click (added after accidental-click complaints). Paste on mobile = 3-finger tap or long-press menu. **Paste a file/image → auto-uploads via SFTP to `/tmp` and inserts the resulting path** — designed for agent prompts.
- Themes/fonts: large built-in theme library (Dracula, Nord, Flexoki, Kanagawa, Everforest…), per-host theme setting, group-inherited themes (red theme for prod = classic use). Nerd Font v3 bundled set (FiraCode, JetBrains Mono, Meslo…). Terminal type configurable (xterm/Linux/VT100). PQC KEX + ML-DSA key support recently added.
- Key forwarding: built-in SSH agent (not OS agent), per-host "SSH Agent Forwarding" toggle.
- Mobile keyboard extras — see §6.

## 4. SFTP & port forwarding

- **SFTP**: desktop = dual-pane (local fs default left, pick host per pane), drag-and-drop, file editing with change tracking + re-upload; one-click switch between a host's terminal and its SFTP tab. Mobile = full-screen SFTP tabs, multiple concurrent, "Connect via SFTP" from host context menu. Recent Android: multiple SFTP sessions alongside terminal tabs, listed in Connections.
- **Port forwarding**: all three types (local/remote/dynamic) as *named, saved rules* on one screen; start/stop with double-click; a **setup wizard** explains each field; credentials drawn from the vault so a rule is one-click for non-experts; team-shareable. Rules can auto-start on connect (the dev-server-preview use case).
- **Minimal useful version for casca**: named local-forward rules per host with one-tap toggle (Go side does `ssh -L` easily); SFTP = single remote pane + upload/download/delete/rename is enough; the paste-file→/tmp trick is cheap and high-value.

## 5. Command palette

`Cmd+K`/`Ctrl+K` — fuzzy search over *all open tabs + all saved hosts* → Enter connects or focuses tab. `Cmd+J` cycles open tabs; `Cmd+T` new connection tab; `Cmd+L` local terminal; `Cmd+N` new host. It's primarily a *tab-and-host jumper*, not a general action palette. `Ctrl/Cmd+.` toggles the terminal side panel; `Ctrl/Cmd+D` split.

## 6. Mobile UX specifics (the important part)

- **Keyboard add-on**: a strip above the system keyboard showing the first 3 groups of the **Extended keyboard** — keys in **groups of 4**, reorderable/customizable: Ctrl, Esc, Tab, arrows, F1–F12, symbols, Hide-keyboard. Side panel exposes the full set + customization. Shift+Tab was added explicitly *for Claude Code mode-switching*; paperclip button uploads a file into the terminal.
- **Gestures**: long-press terminal + drag = arrow keys, farther drag = faster (3 "gears"); hold Space + slide = same; double-tap = Tab; pinch = font size; long-press word = select/copy; 3-finger tap = paste; shake/gesture for PgUp/PgDn/Home/End. Reworked in Android 7.7 so no "touch mode" toggle is needed.
- **Volume buttons assignable** to keys (e.g. Shift+Tab or Ctrl) — iOS 6.3.1+ and Android.
- Side panel tabs on mobile: Extended keyboard / Snippets / Command history / Themes. iPhone = panel above keyboard; iPad = right-side panel.
- **Insert password** action types the saved host password into the prompt.
- Keep-alive: iOS Live Activities keep sessions running in background (~3-min iOS cap otherwise); Android docs tell users to disable battery optimization for Termius.
- **Agent-specific**: per-host config to auto-launch Gemini/Claude Code/OpenCode on connect (iOS 6.3.3); new prompt input field with speech-to-text; Android beta does **push notifications when an agent needs input/finishes via OSC 7501** (Program Status Protocol: `idle/working/done/blocked/error`, `kind=permission/question/auth`, `msg` base64).

## 7. What users complain about

- **Price/subscription**: $10/mo Pro, $20–30/user/mo Team/Business — "more than MS Office for a terminal"; constant refrain. Free tier requires an **account** and can't save host login details on some versions — resented.
- **Credential sync surprise**: keys/passwords synced to Termius cloud by default once logged in; HN user rotated all keys after discovering it. Sync is E2E but *on by default*; can disable only for credentials, not other data.
- **Sync corruption/data loss**: reports of hosts/keychain wiped or corrupted by sync; logout deletes local data.
- **Stability regressions**: versions where groups/search broke, crashes per connection tap; Windows client "slow and buggy"; keyboard lag growing until lockup on some iPads; support unresponsive.
- **Flaky-network handling**: older complaint — switching Wi-Fi→LTE killed connections until app restart (auto-reconnect added later to address this).
- **iOS background limit**: sessions die ~3 min after backgrounding (OS limit, not Termius's fault — but tmux/mosh is the answer).
- **Gaps**: no export-all-hosts, no clone-host (historical), no ProxyCommand, session logs *not viewable on mobile*, Mosh sessions not logged, autocomplete bash-only remotely.

## 8. Mosh, persistence, reconnect

- **Auto-reconnect**: on network drop, Termius re-establishes the SSH session *in the same tab with scrollback intact* — headline feature for VPN/network switching.
- **Mosh**: clean-room, "mosh-compatible" implementation (the Mosh devs made them stop calling it "Mosh") — a per-host toggle; works with standard `mosh-server`; UDP roaming + local echo prediction. Caveats: session logs not recorded for Mosh.
- Official mobile-agent recipe (blog): tmux on the server + `tmux attach -t x` as **Startup Command** + Mosh + iOS Live Activities / Android battery whitelist.
- **For casca the architecture is inherently better**: the SSH session lives on the Go server, not the phone — the browser is a dumb renderer. Survive phone suspend, network hops, browser reloads for free; just re-attach the websocket and repaint.

## 9. Keychain, credentials, collaboration

- **Identities** = named (username + password + key/cert/FIDO2) tuples linkable to many hosts/groups — edit once, applies everywhere. Keygen in-app (incl. Secure Enclave keys on iOS, FaceID/TouchID confirmation, FIDO2/YubiKey).
- Vault encryption: SRP6a auth (password never sent), Argon2id + hybrid encryption; private key encrypted with master password; lose password = lose vault. Local encrypted copy → offline-first. Credentials sync can be disabled per-account.
- **Sharing**: team vaults with per-member edit/view; hosts can be shared *without* credentials (each member uses personal creds) — nice pattern.
- **Multiplayer**: share a link → WebRTC P2P session; viewers watch live, **one typist at a time**, control passed explicitly; only signaling touches Termius servers. (tmux attach gets casca ~80% of this for free.)
- **Session logs**: auto-recorded per host, one rotating log each; bookmark + annotate; team-visible — but *not viewable on mobile*.

---

## Prioritized "steal this" list for casca

1. **Extra-keys strip above the mobile keyboard** — sticky Ctrl/Esc/Tab + arrows + a handful of symbols, in small customizable groups; include **Shift+Tab** (agent mode toggle) and make it trivial to send Ctrl-C/Ctrl-D/Ctrl-Z/Ctrl-B. This is *the* feature that makes phone SSH usable. (xterm.js `attachCustomKeyEventHandler` + a DOM row — cheap.)
2. **Per-host "agent launcher" / startup command** — Termius's `Startup Command` + startup snippets maps exactly to casca's core value: per host config like `tmux attach -t claude || tmux new -s claude 'claude'`, plus first-class host fields "launch claude/codex/devin on connect". One-tap tab → dropped into a running agent.
3. **Saved hosts → one-tap terminal tabs** with a hosts screen showing *already-open sessions* (Termius's Connections screen). Big touch targets, group/tag filter optional — a flat searchable list is enough at tailnet scale.
4. **Snippets with Paste-vs-Run + long-press preview** — tap = run, long-press = paste/edit. Add "run in all open sessions" only if trivially cheap; skip multi-host workspaces entirely.
5. **Silent auto-reconnect of the *web* layer** — websocket drops shouldn't nuke the terminal; keep xterm state server-side (tmux/pty buffer) and repaint on reconnect. Beat Termius here: they can only save scrollback; casca can keep the *live* session.
6. **Touch gestures on the terminal surface** — double-tap = Tab, long-press+drag = arrows with speed gears, pinch = font size, native text selection for copy, 3-finger/long-press paste. The drag-arrows gesture is beloved and rarely copied.
7. **Agent-status tab badges + notification** — watch for OSC 7501 (now emitted by Claude Code) or fall back to prompt-detection; show per-tab "working / waiting-on-you / done" dots (Termius Focus-mode notify dot) + Web Push / favicon badge. This is the feature Termius only just shipped in beta and is *the* killer for agent-monitoring from a phone.
8. **Paste-image/file → upload to `/tmp` + insert path** — for feeding screenshots to agents. Do it over the existing SSH channel (no SFTP pane needed): read clipboard blob → stream to remote `/tmp/xxx` → type the path.
9. **Command history panel + "save as snippet"** — local per-host history list, tap to re-run, promote to snippet. Very high perceived-value-to-cost ratio.
10. **One-tap named port forwards** — per-host saved `-L` rules with a toggle (dev-server preview on the phone browser). Skip remote/dynamic until needed.

**Deliberately skip**: vaults/teams/encryption-key management (self-hosted + Tailscale auth replaces it), accounts/subscriptions, sync (the server *is* the store — export/import as JSON instead, avoiding Termius's no-export complaint), cloud AI autocomplete, workspaces/broadcast, native-app gestures Termius can't do in a browser anyway (volume buttons, Live Activities).

**Anti-patterns to actively avoid**: requiring login before basic use, silently syncing credentials anywhere, dropping sessions on network switches, hiding session logs on mobile, and gating the extra-keys row or snippets behind tiers — those are exactly the complaints that send Termius users looking for alternatives.

**Key sources**: docs.termius.com (vaults/groups/tags/snippets/workspaces/mobile-terminal/port-forwarding/session-logs/encryption pages + changelogs), termius.com/blog (termius-x, workspaces, broadcast-input, new-touch-terminal-on-ios, stay-connected-with-auto-reconnect, 8-tips-for-using-ai-agents-on-mobile, multiplayer), mitchellh.com/writing/program-status-osc7501, App Store reviews, news.ycombinator.com (items 20119938, 29571019, 44937651), lowendtalk.com threads, spawnd.dev/termius-alternatives, roamtty.com mobile-SSH comparison.