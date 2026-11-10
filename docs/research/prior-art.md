# casca: prior art and design notes

Researched 2026-11-09 from primary sources. UNVERIFIED = not confirmed in a source I read. "Analysis" = my inference from the cited sources.

## 1. Wire protocol and flow control

| Project | Frames | Backpressure | Keepalive / reconnect |
|---|---|---|---|
| ttyd | Binary; first byte is the command. Server: `'0'` OUTPUT, `'1'` SET_WINDOW_TITLE, `'2'` SET_PREFERENCES. Client: `'0'` INPUT, `'1'` RESIZE_TERMINAL (+JSON `{columns,rows}`), `'2'` PAUSE, `'3'` RESUME. First client message is JSON `{AuthToken,columns,rows}`. `socket.binaryType='arraybuffer'`. | `flowControl = {limit: 100000, highWater: 10, lowWater: 4}`: every 100000 bytes the client attaches an `xterm.write` callback; more than 10 pending sends PAUSE, fewer than 4 sends RESUME; the server calls `pty_pause`/`pty_resume` (stops reading the PTY). | `--ping-interval` default 5 s. Close code other than 1000 gives "Reconnecting..." and a token refresh; otherwise "Press ⏎ to Reconnect". |
| gotty (sorenisanerd) | Text; type char + base64 output. Buffer 1024, so 765 raw bytes per frame. | None; only a write mutex. | Client ping answered with a pong byte; `--reconnect` (default false), `--reconnect-time` 10 s. |
| wetty | socket.io events | Byte ACKs: client commits every `ackBytes = 262144`; server pauses the PTY at 2^21 unacked bytes and resumes at 524288. | socket.io built-ins |
| sshwifty | Custom protocol, traffic encrypted with time-derived keys (needs WebCrypto) | Not documented; `ReadDelay`/`WriteDelay` 10 ms | `HeartbeatTimeout` 10 s, `ReadTimeout`/`WriteTimeout` 120 s (example config values) |

Sources: [ttyd xterm/index.ts](https://github.com/tsl0922/ttyd/blob/main/html/src/components/terminal/xterm/index.ts), [ttyd app.tsx](https://github.com/tsl0922/ttyd/blob/main/html/src/components/app.tsx), [ttyd protocol.c](https://github.com/tsl0922/ttyd/blob/main/src/protocol.c), [ttyd README](https://github.com/tsl0922/ttyd/blob/main/README.md), [gotty webtty.go](https://github.com/sorenisanerd/gotty/blob/master/webtty/webtty.go), [gotty README](https://github.com/sorenisanerd/gotty/blob/master/README.md), [wetty client flowcontrol.ts](https://github.com/butlerx/wetty/blob/main/src/client/wetty/flowcontrol.ts), [wetty server flowcontrol.ts](https://github.com/butlerx/wetty/blob/main/src/server/flowcontrol.ts), [sshwifty README](https://github.com/nirui/sshwifty/blob/master/README.md).

Binary frames remove the UTF-8 problem entirely: ttyd passes raw PTY bytes to `term.write(Uint8Array)`, and xterm.js decodes across chunk boundaries.

For casca's multiplexed socket the wetty model fits better than ttyd's: PAUSE/RESUME is per connection, while byte ACKs can be counted per channel.

## 2. xterm.js on weak CPUs and phones

- **Flow control guide** ([xtermjs.org](https://xtermjs.org/docs/guides/flowcontrol/)): the write buffer is capped at 50 MB and "any data beyond that limit gets discarded"; throughput is 5-35 MB/s on normal hardware. Byte watermarks `HIGH = 100000`, `LOW = 10000`, and HIGH should stay under 500K to keep keystrokes snappy. The cheaper variant is `CALLBACK_BYTE_LIMIT = 100000`, `HIGH = 5`, `LOW = 2` pending callbacks. Over a websocket the socket is left out of the accounting: the client sends an ACK from the write callback and the server pauses the PTY.
- **Renderers**: 6.0.0 removed the canvas addon ("we recommend using either the DOM renderer or WebGL", [release notes](https://github.com/xtermjs/xterm.js/releases/tag/6.0.0)). The WebGL addon is a "WebGL2-based renderer" and contexts can be lost, handled with `onContextLoss` ([addon README](https://github.com/xtermjs/xterm.js/blob/master/addons/addon-webgl/README.md)). Analysis: the D610's GPU cannot do WebGL2, so DOM is the only renderer there (UNVERIFIED on the machine itself).
- **Phones**: the maintainers say mobile support "is non-existent"; touch needs "the default DOM renderer and/or `screenReaderMode`", and "you basically need to implement your own keyboard or at least some bar with all the relevant characters", which is "out of scope for this library". Touch scrolling regressed in 6.0.0 ([#5377](https://github.com/xtermjs/xterm.js/issues/5377)). So use DOM on phones too.
- **Scrollback**: default 1000 ([ITerminalOptions](https://xtermjs.org/docs/api/terminal/interfaces/iterminaloptions/)). tmux draws on the alternate screen, so xterm.js scrollback stays nearly empty and history lives in tmux ([tmux FAQ](https://github.com/tmux/tmux/wiki/FAQ)).
- **6.0 also adds** synchronized output (DEC mode 2026), which cuts flicker from agent TUIs (release notes).
- **Soft keyboard and IME**: Gboard predictive text buffers input before xterm.js sees it; `autocorrect="off"` and `spellcheck="false"` are already set and do not stop it, and the `type=password` trick was rejected ([#2403](https://github.com/xtermjs/xterm.js/issues/2403)). wetty sets `contenteditable`, `spellcheck`, `autocorrect`, `autocomplete`, `autocapitalize` on `.xterm-screen` ([mobile.ts](https://github.com/butlerx/wetty/blob/main/src/client/wetty/mobile.ts)). iOS composition still drops some input ([PR #5614](https://github.com/xtermjs/xterm.js/pull/5614), open).
- **Keyboard and viewport**: on iOS the layout viewport keeps its size and only `window.visualViewport` shrinks ([MDN](https://developer.mozilla.org/en-US/docs/Web/API/VisualViewport)). Size the terminal container from `visualViewport.height`, debounce, then `fit()`. One project reports that refitting while the keyboard is open blurs the hidden textarea and iOS will not reopen the keyboard without a user gesture (UNVERIFIED, third-party PR).
- **Extra-keys bytes**: Esc `\x1b`, Tab `\t`, Shift+Tab `\x1b[Z`, arrows `\x1b[A` `\x1b[B` `\x1b[C` `\x1b[D` (`\x1bOA`... in application cursor mode), Ctrl+letter = `code & 0x1f` (Ctrl-C `\x03`). A sticky Ctrl/Alt toggle like Termux's is the usual design (UNVERIFIED, the Termux wiki blocked the fetch).

## 3. Reattach with tmux

- **`new-session -A` ignores the command when the session exists.** It returns through `cmd_attach_session` before the command arguments are read ([cmd-new-session.c](https://github.com/tmux/tmux/blob/master/cmd-new-session.c)). The reverse case matters more: if the agent exited while the link was down, the session is gone and `-A` silently starts a new agent.
- **Raw replay is unsafe.** A 256 KB ring can start mid-sequence and loses modes set earlier. Analysis: it is correct only when the same xterm.js instance resumes from an exact byte offset.
- **A resize forces a clean redraw.** `tty_resize` always calls `tty_invalidate`, which resets cached cursor, region and SGR state and sets `tty->mode = ALL_MODES`, so mouse and other modes are sent again ([tty.c](https://github.com/tmux/tmux/blob/master/tty.c)). The kernel only raises SIGWINCH when the size changes, so jiggle rows-1 then rows. The redraw does not re-enter the alternate screen, so a fresh terminal needs `\x1b[?1049h\x1b[H\x1b[2J` first (UNVERIFIED in practice). Restarting the ssh/tmux client is the certain alternative. `tmux refresh-client -t <client>` also redraws but needs a second ssh.
- **Several clients**: `window-size` defaults to `latest`, "the size of the most recently used client". `aggressive-resize` only matters with `smallest` ([options-table.c](https://github.com/tmux/tmux/blob/master/options-table.c)).
- **Defaults** (same file; Debian 13 ships tmux 3.5a, [packages.debian.org](https://packages.debian.org/trixie/tmux)): `escape-time` 10 ms, `history-limit` 2000, `set-clipboard` external, `allow-passthrough` off, `set-titles` off, `focus-events` off, `extended-keys` off, `destroy-unattached` off, `remain-on-exit` off, `monitor-bell` on.
- **OSC 52**: `external` already forwards tmux's own copies; apps inside tmux setting the clipboard need `set-clipboard on`, and the outer TERM needs the `Ms` capability, added automatically only for `xterm*` ([Clipboard wiki](https://github.com/tmux/tmux/wiki/Clipboard)). xterm.js needs `@xterm/addon-clipboard` to honour it.
- **Setting options without `~/.tmux.conf`**: commands chain with `\;` ([tmux(1)](https://man.openbsd.org/tmux.1)). `escape-time`, `set-clipboard`, `focus-events`, `extended-keys` are server-wide, so setting them changes the user's other sessions. A dedicated server avoids that: `tmux -L casca -f /dev/null ...` (`-L` and `-f` per tmux(1)).

```
tmux -L casca -f /dev/null new-session -A -s c-NAME 'CMD' \; \
  set -s escape-time 50 \; set -s set-clipboard on \; set -s focus-events on \; \
  set -s extended-keys on \; set -as terminal-features 'xterm*:extkeys' \; \
  set -t c-NAME history-limit 20000 \; set -t c-NAME mouse on \; \
  set -t c-NAME set-titles on \; set -t c-NAME set-titles-string '#{pane_title}' \; \
  set -t c-NAME status off \; set -w -t c-NAME allow-passthrough on \; \
  set -w -t c-NAME remain-on-exit on
```

Caveats: `history-limit` only applies to panes created afterwards, so it does nothing for the first pane on this line (from tmux(1) as I recall it; UNVERIFIED this session). `escape-time` 50 rather than 10 because the FAQ says a slower network needs a larger value. The Claude Code tmux lines (`allow-passthrough`, `focus-events`, `extended-keys`, `xterm*:extkeys`) come from [its terminal docs](https://code.claude.com/docs/en/terminal-config).

## 4. Surviving the flaky link

- **ssh options** ([ssh_config(5)](https://man.openbsd.org/ssh_config)): `ServerAliveInterval` defaults to 0 (off), `ServerAliveCountMax` 3, `ConnectionAttempts` 1, `TCPKeepAlive` on; `ConnectTimeout` has no stated default. Use `-o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o ConnectTimeout=10 -o TCPKeepAlive=no -o BatchMode=yes`, which detects a dead link in about 15 s. `ExitOnForwardFailure` is irrelevant without forwards.
- **ControlMaster** (analysis): one handshake serves every tab, which matters on a Pentium M, but every session dies together and a hung master blocks new connections until ServerAlive kills it. Leave it off at first. If added: `ControlPersist=60` and `ssh -O exit` before each retry.
- **Respawn**: autossh's model is `AUTOSSH_GATETIME` 30 s, progressively longer waits after quick failures, and `-M 0` with ServerAlive preferred over its monitor port ([autossh(1)](https://linux.die.net/man/1/autossh)). For 3-5 minute drops use 1, 2, 4, 8, 15 s capped at 15 s with jitter, reset after 30 s connected.
- **Exit codes**: "ssh exits with the exit status of the remote command or with 255 if an error occurred" ([ssh(1)](https://man.openbsd.org/ssh)). The tmux client exits 0 on detach and on `[exited]`, 1 on lost tty or server exit ([client.c](https://github.com/tmux/tmux/blob/master/client.c)). So 255 means retry; anything else means ask tmux whether the session and its pane are still alive (`has-session`, or `#{pane_dead}` when `remain-on-exit` is on). With `BatchMode=yes`, auth and host-key failures are also 255: stop after repeated sub-2-second failures and show stderr.
- **mosh**: Debian 13 has mosh 1.4.0-1+b2 for i386 ([packages.debian.org](https://packages.debian.org/trixie/mosh)). It roams and survives drops, but needs `mosh-server` on every remote and UDP 60000-61000, and "synchronizes only the visible state of the terminal" ([mosh.org](https://mosh.org/)). It runs its own terminal emulator, so OSC passthrough is doubtful (UNVERIFIED). tmux already gives persistence, so mosh is an optional per-host transport at most.

## 5. Detecting agent state

- **BEL** is the only signal that crosses tmux unaided: tmux writes BEL to each attached client when `monitor-bell` is on, `bell-action` applies, and `visual-bell` is off or both ([alerts.c](https://github.com/tmux/tmux/blob/master/alerts.c)). Catch it with `term.onBell`.
- **OSC handled by tmux**: 0, 2, 4, 7, 8, 9 (only `9;4` progress and `9;9`), 10-12, 52, 104, 110-112, 133. Everything else, including OSC 9 notifications, 99 and 777, is dropped ([input.c](https://github.com/tmux/tmux/blob/master/input.c)). The only way through is `\033Ptmux;` + payload with each ESC doubled + `\033\\`, with `allow-passthrough on` ([FAQ](https://github.com/tmux/tmux/wiki/FAQ)).
- **Titles**: OSC 0/2 sets the pane title; it reaches the browser only with `set-titles on` (`term.onTitleChange`). Claude Code prefixes the title with a status glyph in terminals without OSC 7501. Which glyphs: UNVERIFIED.
- **OSC 7501 is real.** Program Status Protocol, revision 0.4, spec at [superlogical.com](https://www.superlogical.com/rex/docs/build/program-status). Format `ESC ] 7501 ; key=value:key=value ST`, with ST = `ESC \` or BEL. `state` = `idle|working|done|blocked|error|clear`; `kind` = `permission|question|auth`; `progress` 0-100; `title` and `msg` base64; 4096-byte limit. Query `ESC ] 7501 ; ? ST`. xterm.js ships `@xterm/addon-program-status` ([addon](https://github.com/xtermjs/xterm.js/tree/master/addons/addon-program-status)). Claude Code v2.1.295+ emits it, but "inside tmux, Claude Code checks tmux for support instead of your terminal, and `allow-passthrough` has no effect on that" ([docs](https://code.claude.com/docs/en/terminal-config)). So it does not work in casca's tmux design today. Codex and Devin emitting it: UNVERIFIED (forum claim only).
- **Claude Code**: `"preferredNotifChannel": "terminal_bell"` in `~/.claude/settings.json` sends BEL when it finishes or waits for permission. Hooks are the precise channel: `Stop`, and `Notification` with matchers `permission_prompt`, `idle_prompt`, `elicitation_dialog`, `agent_needs_input`, `agent_completed` ([hooks](https://code.claude.com/docs/en/hooks)).
- **Codex**: `tui.notifications` (bool or list, e.g. `agent-turn-complete`, `approval-requested`), `tui.notification_method = auto|osc9|bel` (auto prefers OSC 9, which tmux drops, so set `bel`), `tui.notification_condition = unfocused|always`, and `notify = [...]`, which runs a program with JSON for `agent-turn-complete` only ([reference](https://learn.chatgpt.com/docs/config-file/config-reference), [advanced](https://learn.chatgpt.com/docs/config-file/config-advanced)).

## 6. Security on the tailnet

- **Origin**: gorilla's default `CheckOrigin` fails when Origin is present and its host differs from Host ([pkg.go.dev](https://pkg.go.dev/github.com/gorilla/websocket)). Keep it; never return true. Also allowlist the Host header against the tailnet name and 100.x address, which blocks DNS rebinding (analysis). ttyd makes the same check optional (`-O`); gotty rejects cross-origin by default.
- **Token and cookie**: keep the token out of the URL. Use a cookie with `HttpOnly; SameSite=Strict; Path=/`, plus `Secure` when served over HTTPS, or send it as the first websocket message as ttyd does.
- **Binding**: listen on the address from `tailscale ip -4` or on 127.0.0.1, never 0.0.0.0.
- **Identity, simplest**: bind 127.0.0.1 and run `tailscale serve --bg 8022`. Serve adds `Tailscale-User-Login`, `Tailscale-User-Name`, `Tailscale-User-Profile-Pic` (absent for tagged devices) and provides HTTPS; "it's best practice to only have the service listen on localhost" ([Serve](https://tailscale.com/kb/1312/serve), [CLI](https://tailscale.com/kb/1242/tailscale-serve)).
- **Identity without serve**: GET `/localapi/v0/whois?addr=IP:port` on the tailscaled unix socket with host `local-tailscaled.sock`; 404 means not a peer ([local.go](https://github.com/tailscale/tailscale/blob/main/client/local/local.go)). Plain `net/http` over the socket avoids the dependency. Socket path `/var/run/tailscale/tailscaled.sock` and non-root access: UNVERIFIED.
- **gorilla**: one concurrent writer per connection, so one writer goroutine. Leave `EnableCompression` off ("experimental and may result in decreased performance").

## Do this in casca

1. Send output as binary frames, `[1-byte type][2-byte channel][raw PTY bytes]`, with `binaryType='arraybuffer'` and `term.write(Uint8Array)`.
2. Replace dropping with per-channel ACK flow control: client ACKs every 100000 bytes from the `term.write` callback; server stops reading that PTY above 5 unacked windows and resumes below 2.
3. Give each channel a byte offset. On websocket reconnect the client sends its last offset; replay the delta if it is still in the ring, otherwise do item 4.
4. For a new terminal, send no scrollback: `term.reset()`, write `\x1b[?1049h\x1b[H\x1b[2J`, then jiggle the PTY (rows-1, then rows). Fall back to restarting the tmux client if the test shows artefacts.
5. Create with `new-session -A` once only; every respawn uses `tmux -L casca attach-session -t =c-NAME`, with `remain-on-exit on`, so a finished agent is never relaunched.
6. Run ssh with `-o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o ConnectTimeout=10 -o TCPKeepAlive=no -o BatchMode=yes`.
7. Respawn on exit 255 with backoff 1, 2, 4, 8, 15 s (cap 15 s, jitter, reset after 30 s up). On any other code run `tmux -L casca list-panes -t =c-NAME -F '#{pane_dead} #{pane_dead_status}'`: an error means the session is gone, `1` means the agent exited (pane kept by `remain-on-exit`), `0` means a plain detach.
8. Use a dedicated tmux server (`-L casca -f /dev/null`) with the option line from section 3.
9. Use the DOM renderer everywhere, `scrollback: 1000`, no WebGL addon. History comes from tmux (`mouse on`, or `capture-pane -p -e -S -2000` on demand).
10. Websocket ping every 5 s from the server with a 15 s read deadline; the client reconnects with the same backoff as item 7.
11. Mobile: size the terminal from `visualViewport.height` (debounced 100 ms) and add a keys row with Esc, Tab, sticky Ctrl, sticky Alt, arrows, `|`, `/`, `-`, `~`, sending the bytes in section 2.
12. Agent state: set `preferredNotifChannel: "terminal_bell"` for Claude and `tui.notification_method = "bel"` for Codex, mark the tab on `term.onBell`, and show `onTitleChange`.
13. Later, for exact state: Claude `Stop`/`Notification` hooks and Codex `notify` POST to casca over the tailnet. Do not rely on OSC 7501 through tmux.
14. Keep gorilla's default `CheckOrigin`, add a Host allowlist, and use a `HttpOnly; SameSite=Strict` cookie instead of a URL token.
15. Bind 127.0.0.1 behind `tailscale serve --bg <port>` and read `Tailscale-User-Login`, or bind the tailscale IP and call the whois local API.
