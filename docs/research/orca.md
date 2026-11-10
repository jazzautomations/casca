# Orca (stablyai/orca) — mechanics worth porting to casca

Source read at `e96ee25` from `/tmp/orca` (sparse clone). **[S]** = verified in source, **[D]** = docs only (`docs/site/content/docs/*.mdx`), **[I]** = my inference.

## 1. Remote runtime

- **Three processes [S]**: UI client; `orcad` (headless Node runtime, `src/main/orcad/`), which serves a WebSocket RPC; and a detached **terminal daemon** (`src/main/daemon/`) that owns the PTYs over a unix socket and must outlive orcad (`orcad-daemon-supervision.ts`: "Detached terminal daemons must outlive orcad").
- **Paired Remote Orca Server [D+S]**: server owns repos, worktrees, PTYs, agent state. Client dials `ws://host:6768` (default port per docs). Pairing link is `orca://pair?code=<base64url JSON offer>` (`src/shared/remote-runtime/pairing/pairing.ts`); the offer carries endpoint, per-device token and a public key, scope `mobile|runtime`, invite TTL max 10 min. Each client gets its own revocable token. Phone traffic is E2EE inside the socket (`runtime/rpc/e2ee-channel.ts`, states `awaiting_hello → awaiting_auth → ready`, closes after 5 consecutive decrypt failures).
- **SSH target [S]**: no different protocol. Orca uploads orcad plus a pinned Node over SSH (`src/main/managed-server/install/`), starts it bound to loopback, SSH-forwards a local port, and rewrites the pairing endpoint to `127.0.0.1:<localPort>` (`managed-server/lifecycle/orcad-tunneled-pairing.ts`, which rejects non-loopback endpoints). The older `relay.js --detached` path is compat-only (`src/main/ssh/legacy-relay/README.md`).
- **Difference [D]**: SSH target = the laptop owns the catalog and one laptop drives the host; paired server = host owns everything and many clients share it.
- Server heartbeat reaps a socket only after **3 consecutive missed pings** (`remote-runtime-server-heartbeat.ts`: "Cellular/Tailscale loss may swallow one pong").

## 2. Terminal sessions

- **Spawn [S]**: `node-pty` inside the daemon (`pty-subprocess/native-pty-spawn.ts`). Daemon RPC is NDJSON with two sockets per client, `role: 'control' | 'stream'`, token in `hello` (`protocol/daemon-hello-protocol.ts`). Requests: `createOrAttach`, `write`, `resize`, `pausePty`/`resumePty`, `detach`, `getSnapshot`, `takePendingOutput`, `kill` (`protocol/daemon-rpc-contract.ts`). Session states `created|spawning|running|exiting|exited`.
- **Reattach replays a serialized screen, not raw bytes [S]**. Every session feeds a headless xterm (`@xterm/headless` + `SerializeAddon`, `emulation/headless-emulator.ts`). `createOrAttach` returns `TerminalSnapshot` (`protocol/terminal-snapshot.ts`): `snapshotAnsi`, `scrollbackAnsi` (normal buffer kept separately when the alt screen is active), `rehydrateSequences`, `cols/rows`, `cwd`, `lastTitle`, `outputSequence`. Mode rehydration (`terminal-mode-rehydrate-sequences.ts`): `ESC[?1049h` (after an SGR reset), `?2004h`, `?1h`, `ESC[>4;Nm`, mouse `?9h/?1000h/?1002h/?1003h`, encoding `?1016h/?1006h`.
- Live grid keeps **1000 scrollback rows** (`sessions/daemon-session-scrollback-window.ts`); deeper history is a disk log capped at 5 MiB with incremental checkpoints (`history/storage/terminal-history-file-limits.ts`).
- **Resume without a snapshot [S]**: the WS layer keeps a per-PTY ring (`RING_MAX_UNITS = 64*1024`) and a seq ledger; a reconnecting client presents its last seq and gets only the gap, else a fresh snapshot (`rpc/methods/terminal/terminal-stream-resume-ledger.ts`, `terminal-subscribe-binary-resume.ts`).
- **Wire frame [S]** (`src/shared/terminal/stream/terminal-stream-protocol.ts`): 16-byte header `0x74, ver=1, opcode, 0, streamId u32 LE, seq u64 LE`, then payload. Opcodes: Output 1, SnapshotStart/Chunk/End 2-4, Resized 5, Input 7, Resize 8, Subscribe 9, Unsubscribe 10, Ack 13, ClaimViewport 14, SetOutputPaused 16, InputAck 18.
- **Multi-client resize [S]**: no "smallest wins". A per-PTY driver is `idle | desktop | mobile{clientId}` (`runtime/terminal/driver/runtime-terminal-driver-controller.ts`); the most recent actor's viewport is applied and the desktop size is restored when the phone unsubscribes or toggles display mode (`runtime/terminal/layout/terminal-display-mode.ts`).
- **Flow-control numbers [S]**:
  - Daemon batcher (`transport/stream/daemon-stream-data-batcher.ts`): 2 ms flush, 64 KiB slices, hold writes while socket `writableLength ≥ 128 KiB`, sessions with < 4 KiB queued bypass the hold.
  - Backpressure (`daemon-stream-backpressure.ts`): global high/low 4 MiB / 1 MiB; per-session 64 KiB / 32 KiB → pause the PTY producer.
  - Failsafes (`sessions/session-producer-pause.ts`): a client pause auto-releases after 5 s; a stalled stream is released after 60 s.
  - Background sessions (`daemon-stream-keep-tail-drop.ts`): drop oldest, keep a 512 KiB tail (min 64 KiB, 2 MiB shared), emit a `dataGap` event, and salvage terminal queries from the dropped bytes (≤ 4096 chars) so a hidden TUI still gets its replies.
  - WS credit (`terminal-multiplex-flow-control.ts`): 48 KiB chunks, per-stream ack window 512 KiB → 2 MiB, total 2 → 8 MiB, client acks every 192 KiB or 4 ms.

## 3. SSH reconnect

- `ssh-connection-timing.ts` [S]: `RECONNECT_BACKOFF_MS = [1000, 2000, 5000, 5000, 10000, 10000, 10000, 30000, 30000]`, `CONNECT_TIMEOUT_MS = 30_000`; first connect retries 5 × 2 s on transient errors.
- Ladder (`reconnect/ssh-reconnect-ladder.ts`) [S]: the delay index advances on every retry, but only **failed handshakes** count toward give-up (9). A connection that held ≥ 60 s resets the delay index. While no handshake has failed (a flap), delay is capped at 5 s so the retry lands inside the 60 s minimum relay grace.
- Classification (`ssh-reconnect-error-classification.ts`) [S]: **give up** on host-key denial, auth (`permission denied (publickey…)`, `all configured authentication methods failed`, `too many authentication failures`) and passphrase errors → `auth-failed`. **Retry** on `ETIMEDOUT/ECONNREFUSED/ECONNRESET/EHOSTUNREACH/ENETUNREACH/EAI_AGAIN` plus OpenSSH prose: `no route to host`, `network is unreachable`, `could not resolve hostname`, `connection timed out`, `connection refused`, `connection reset`, `broken pipe`, `kex_exchange_identification`, `connection closed by remote`. Anything else → `error`, no retry.
- **Network-return restart** (`ssh-connection-reconnect.ts`) [S]: on wake/online, a spent or sleeping ladder is reset and redialed immediately; attempts older than 5 s are treated as stale; generation counters stop a superseded attempt from publishing.
- Keepalive [S]: ssh2 `keepaliveInterval: 15_000`; system ssh `ServerAliveInterval=15`, `ServerAliveCountMax=3`, `ControlMaster=auto`, `ControlPersist=300`. App-level frame keepalive every 5 s, dead only when **no inbound for 20 s AND oldest unacked > 20 s**; a timer gap > 15 s is read as sleep and rebases the clocks (`protocol/ssh-channel-multiplexer.ts`).
- Survival [S+D]: PTYs live in the host daemon; nothing is killed on drop. Default grace is "until reset" (`relayGracePeriodSeconds` 0, max 7 days).

## 4. Agent launch and status

- Launch [S]: per-agent catalog (`src/shared/agent-launch/catalog/tui-agent-config.ts`) with `detectCmd`, `promptInjectionMode: 'argv' | 'stdin-after-start'`, `draftPromptFlag` (`claude --prefill`), `argvPromptSeparator` (`devin -- <prompt>`), paste-ready signals. `buildAgentStartupPlan` (`startup/tui-agent-startup.ts`) yields `launchCommand`, `expectedProcess`, `followupPrompt`, `env`. PTY env carries `ORCA_PANE_KEY`, `ORCA_TAB_ID`, `ORCA_WORKTREE_ID`, `ORCA_AGENT_LAUNCH_TOKEN`, `ORCA_AGENT_HOOK_PORT/TOKEN/ENDPOINT`.
- **Status = three layers, in precedence order** (`src/main/agent-hooks/README.md`) [S]:
  1. **Hooks.** Orca writes a `#!/bin/sh` script into `~/.claude/settings.json` for `SessionStart, UserPromptSubmit, Stop, StopFailure, SubagentStart/Stop, PreToolUse, PostToolUse, PostToolUseFailure, PermissionRequest, PostCompact` (`src/main/claude/claude-managed-hook-events.ts`). It prints `{}` first (permission hooks fail closed on empty stdout), then `curl -sS -X POST http://127.0.0.1:$ORCA_AGENT_HOOK_PORT/hook/claude --connect-timeout 0.5 --max-time 1.5 -H "X-Orca-Agent-Hook-Token: …" --data-binary @-`, and spools to disk on failure. Mapping (`src/shared/agent-hook-listener/providers/claude-events.ts`): `UserPromptSubmit | PreToolUse | PostToolUse(Failure)` → **working**; `PermissionRequest | Elicitation | PreToolUse(AskUserQuestion)` → **waiting**; `Stop | StopFailure` → **done**. States: `working | blocked | waiting | done`.
  2. **OSC 7501 Program Status** (`src/shared/program-status/protocol.ts`): program emits `ESC ] 7501 ; state=<idle|working|done|blocked|error>:kind=<permission|question|auth>:progress=N:app=<id>:msg=<base64>:id=<child/path> ESC \`. Many agents first probe `ESC ] 7501 ; ? ESC \`; the terminal must answer `ESC ] 7501 ; ?:states=idle,working,done,blocked,error:kinds=permission,question,auth ESC \`. Supported by Claude Code ≥ 2.1.295, Codex, Grok, pi. Used when hooks are absent, or silent for 2 min on a state the program left (`server/program-status/hook-precedence.ts`).
  3. **Title glyphs** (`src/shared/agent-status/title/`, `runtime/agent-state-rules/claude.json`): Claude idle title starts with `✳` (U+2733) or ASCII `* `; working shows braille `U+2800–28FF` or quarter-circle `U+25D0–25D3` spinners. Gemini: `✦` working, `◇` idle, `✋` permission. Shell title after idle = agent exited.
- Caveat [S]: once the terminal answers the 7501 probe, Claude stops painting the title glyph.

## 5. Notifications and mobile

- Phone = Expo app over the same WS RPC + E2EE, or Orca's cloud relay; APNs/FCM pushes go through a push gateway (`runtime/push/`) [S]. Payload: `kind: alert|dismiss, hostFingerprint, notificationId, notificationSeq, notificationEpoch, paneKey, worktreeId, agentState` (`mobile/src/notifications/push-payload.ts`).
- Triggers [S]: transitions `turn-finished | errored | needs-input` (`agent-transitions.ts`). A finish waits **1.5 s quiet** (`AGENT_DONE_QUIET_MS`) and is cancelled if work resumes; 5 s per-key cooldown (`notification-burst-cooldown.ts`).
- Missed alerts [S]: 256-entry replay buffer with `seq` + random `epoch`; client sends its watermark, an epoch mismatch replays everything (`mobile-notification-replay.ts`). Desktop presence is reported every 30 s with a 90 s TTL (`desktop-client-presence.ts`).
- Unread bell, mark-unread and dock badge are [D] only; I did not read the renderer.

## 6. Data model

- One JSON document, `orca-data.json`, written atomically [S]. `PersistedState` (`src/shared/persisted-state-types.ts`): `schemaVersion, repos, projects, worktreeMeta, settings, ui, workspaceSession, sshTargets[], sshRemotePtyLeases[], …`.
- `SshTarget` (`src/shared/ssh-types.ts`): `id, label, configHost, host, port, username, identityFile, proxyCommand, jumpHost, source: 'ssh-config'|'manual', relayGracePeriodSeconds, reconnectOnLaunch, portForwards, generation`.
- Agent status is persisted separately, at most every 3 min; rows older than 30 min are stale (`agent-hooks/server/server-constants.ts`, `agent-status-freshness.ts`).

## Steal this for casca

1. **Agent status via hooks + OSC 7501.** Launch with `CASCA_SESSION`, `CASCA_HOOK_URL`, `CASCA_HOOK_TOKEN` in the env; install the curl hook (print `{}` first, 0.5 s connect / 1.5 s max, always `exit 0`) with the event mapping in §4. The hook runs on the remote and the D610 is not reachable as `127.0.0.1`, so post to the tailnet address or append to a spool file casca tails over the session. Independently, scan PTY output for `ESC]7501;` and answer the `?` probe. Needs `set -g allow-passthrough on` in tmux, and that is untested here.
2. **Title-glyph fallback** for agents without either: OSC 0/2 title starting `✳` or `* ` = idle; braille or `◐◓◑◒` = working. Needs tmux `set-titles on`.
3. **Reconnect ladder**: `[1,2,5,5,10,10,10,30,30]` s, 30 s connect timeout, reset after 60 s stable, count only failed handshakes, cap flap delay at 5 s, retry/give-up lists from §3. **Change for casca**: 9 failures is about 103 s of delays plus connect timeouts, shorter than a 3–5 min WiFi drop. Never give up on transient errors (hold at 30 s) and reset the ladder on link-up (netlink or tailscale status).
4. **Two-condition liveness**: declare the link dead only when nothing arrived for 20 s *and* the oldest unacked frame is > 20 s old; treat a tick gap > 3× interval as suspend and rebase. Use `ServerAliveInterval=15`, `ServerAliveCountMax=3`; reap a browser WS only after 3 missed pings.
5. **Seq-numbered output + ring resume**: stamp each PTY byte offset, keep a 64 KiB ring per session, browser reconnects with `lastSeq`, send the gap or fall back to a full repaint.
6. **Repaint on attach**: Orca needs a headless xterm; casca already has tmux. Use `tmux capture-pane -e -p -S -1000` plus the mode sequences in §2, or simply let a fresh `tmux attach` redraw.
7. **Flow control**: 2 ms coalescing, 48–64 KiB frames, per-session 64/32 KiB watermarks that stop reading the PTY fd, global 4/1 MiB, 5 s auto-resume failsafe, client acks every 192 KiB.
8. **Background-tab thinning**: for sessions not visible, keep only a 512 KiB tail and send a `gap` marker so the client repaints later. This is the biggest saving on a phone link.
9. **Resize = last actor wins**, with the desktop size remembered and restored when the phone leaves (tmux `window-size latest` gives this for free).
10. **Notification hygiene**: 1.5 s quiet window before "done", 5 s cooldown per session, `seq + epoch` watermark with a 256-entry replay for a phone that was offline.

**Bad fit for a 2 GB Pentium M**: a headless xterm emulator per session (JS heap per terminal; tmux already is the emulator); Electron/Node orcad uploaded to each host (~60 MB, glibc ≥ 2.28, x64/arm64 only, so no i386); 32 MiB held-write and 8 MiB ack windows (scale down ~10×); E2EE + cloud relay + APNs/FCM gateway (Tailscale already encrypts; use Web Push or ntfy); 5 MiB-per-session history logs and 200 MB checkpoints.
