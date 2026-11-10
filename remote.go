package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sshBin can be overridden in tests.
var sshBin = envOr("CASCA_SSH", "ssh")

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// cleanEnv is the environment for child processes. TMUX is dropped so that a
// casca started from inside tmux can still attach sessions on this machine.
func cleanEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func shellquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var nameRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// tmuxName turns free text into a name that is safe as a tmux target and in a
// shell word. tmux itself rewrites "." and ":" in session names, so drop them.
func tmuxName(s string) string {
	s = strings.Trim(nameRe.ReplaceAllString(s, "-"), "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func (h Host) isLocal() bool { return h.Target == "" || h.Target == "local" }

// Spec says what a session runs. Exactly one mode applies:
// Tmux != "" && Attach: attach to an existing remote tmux session;
// Tmux != "": create-or-attach a tmux session and type Cmd into a new one;
// otherwise: run Cmd (or a login shell) with nothing keeping it alive.
type Spec struct {
	Tmux   string
	Attach bool
	Cmd    string
	Dir    string
	Mouse  bool
}

// script builds the POSIX sh program run on the target.
//
// A new session starts the user's normal interactive shell and the agent
// command is typed into it. That gives the agent the same PATH and rc files
// the user gets by hand (nvm, ~/.local/bin), and quitting the agent leaves a
// shell instead of destroying the session.
func (sp Spec) script() string {
	if sp.Tmux == "" {
		var b strings.Builder
		if sp.Dir != "" {
			fmt.Fprintf(&b, "cd %s && ", shellquote(sp.Dir))
		}
		if sp.Cmd == "" {
			b.WriteString(`exec "${SHELL:-/bin/sh}" -l`)
		} else {
			fmt.Fprintf(&b, `exec "${SHELL:-/bin/sh}" -lic %s`, shellquote(sp.Cmd))
		}
		return b.String()
	}
	n := shellquote(sp.Tmux)
	exact := shellquote("=" + sp.Tmux)
	// Let the pane's title (agents put their current task there) reach the
	// outer terminal. A per-session option: the user's other sessions and
	// tmux.conf are untouched.
	pane := shellquote("=" + sp.Tmux + ":")
	titles := fmt.Sprintf("tmux set-option -t %s set-titles on 2>/dev/null; tmux set-option -t %s set-titles-string '#{pane_title}' 2>/dev/null; ", pane, pane)
	// casca's own sessions (c-NAME) get a quiet status line; sessions the user
	// made by hand keep whatever look they have.
	if strings.HasPrefix(sp.Tmux, "c-") {
		titles += fmt.Sprintf("tmux set-option -t %s status-style 'bg=#1c1c1c,fg=#8a857c' 2>/dev/null; ", pane)
	}
	if sp.Attach {
		return titles + "exec tmux attach-session -t " + exact
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tmux has-session -t %s 2>/dev/null || { tmux new-session -d -s %s", exact, n)
	if sp.Dir != "" {
		fmt.Fprintf(&b, " -c %s", shellquote(sp.Dir))
	}
	// set-option and send-keys take a pane target, where exact matching
	// needs the "=name:" form; "=name" alone only works for session targets.
	if sp.Mouse {
		fmt.Fprintf(&b, " && tmux set-option -t %s mouse on", pane)
	}
	if sp.Cmd != "" {
		fmt.Fprintf(&b, " && tmux send-keys -t %s -l %s && tmux send-keys -t %s Enter",
			pane, shellquote(sp.Cmd), pane)
	}
	fmt.Fprintf(&b, "; }; %sexec tmux attach-session -t %s", titles, exact)
	return b.String()
}

// sshOpts are tuned for a link that drops for minutes at a time: notice a
// dead peer in 30 s instead of hanging, and never wait long to connect.
var sshOpts = []string{
	"-o", "ServerAliveInterval=10",
	"-o", "ServerAliveCountMax=3",
	"-o", "ConnectTimeout=10",
	"-o", "TCPKeepAlive=yes",
	"-o", "StrictHostKeyChecking=accept-new",
}

// wrapLogin runs a script through a login sh so /etc/profile and ~/.profile
// set PATH (ssh command mode starts a non-login shell with a bare PATH). The
// outer line stays valid in non-POSIX login shells such as fish.
func wrapLogin(script string) string {
	return "exec sh -lc " + shellquote(script)
}

// needsApproval reports that the host is waiting for a person: Tailscale SSH
// in "check" mode prints a link and holds the connection until it is opened.
func needsApproval(out []byte) bool {
	return bytes.Contains(out, []byte("requires an additional check"))
}

// connFlag is a file ssh touches once the connection is authenticated. It is
// how casca tells "waited at the door" from "got in": only after getting in
// can the remote session have been created.
func connFlag() string {
	f, err := os.CreateTemp("", "casca-conn-")
	if err != nil {
		return ""
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return name
}

// connectedFor is how long ago ssh authenticated, or -1 if it never did.
func connectedFor(flag string) time.Duration {
	if flag == "" {
		return -1
	}
	st, err := os.Stat(flag)
	if err != nil {
		return -1
	}
	return time.Since(st.ModTime())
}

// command returns the process to start for spec on host, with a tty.
// patient gives the far side ten minutes to let us in instead of ten seconds,
// for hosts that hold the connection while a person approves it.
func command(h Host, sp Spec, flag string, patient bool) *exec.Cmd {
	script := sp.script()
	if h.isLocal() {
		return exec.Command("sh", "-lc", script)
	}
	args := append([]string{"-tt"}, sshOpts...)
	if patient {
		args = append(args, "-o", "ConnectTimeout=600")
	}
	if flag != "" {
		args = append(args, "-o", "PermitLocalCommand=yes", "-o", "LocalCommand=touch "+flag)
	}
	args = append(args, strings.Fields(h.Args)...)
	args = append(args, "--", h.Target, wrapLogin(script))
	return exec.Command(sshBin, args...)
}

// batch runs a non-interactive command on host and returns its stdout.
func batch(ctx context.Context, h Host, script string, stdin io.Reader) ([]byte, error) {
	var cmd *exec.Cmd
	if h.isLocal() {
		cmd = exec.CommandContext(ctx, "sh", "-lc", script)
	} else {
		args := []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=6",
			"-o", "StrictHostKeyChecking=accept-new"}
		args = append(args, strings.Fields(h.Args)...)
		args = append(args, "--", h.Target, wrapLogin(script))
		cmd = exec.CommandContext(ctx, sshBin, args...)
	}
	cmd.Stdin = stdin
	return cmd.Output()
}

// RemoteSession is one tmux session found on a host.
type RemoteSession struct {
	Name     string `json:"name"`
	Attached int    `json:"attached"`
	Activity int64  `json:"activity"` // unix seconds of last output
	Command  string `json:"command"`  // foreground command of the active pane
}

const tmuxListFmt = `#{session_name}|#{session_attached}|#{session_activity}|#{pane_current_command}`

func parseTmuxList(out string) []RemoteSession {
	res := []RemoteSession{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 4)
		if len(f) != 4 || f[0] == "" {
			continue
		}
		att, _ := strconv.Atoi(f[1])
		act, _ := strconv.ParseInt(f[2], 10, 64)
		res = append(res, RemoteSession{Name: f[0], Attached: att, Activity: act, Command: f[3]})
	}
	return res
}

// listRemote returns the tmux sessions alive on host. No tmux server running
// is an empty list, not an error.
func listRemote(ctx context.Context, h Host) ([]RemoteSession, error) {
	out, err := batch(ctx, h, "tmux list-sessions -F "+shellquote(tmuxListFmt)+" 2>/dev/null || true", nil)
	if err != nil {
		return nil, err
	}
	return parseTmuxList(string(out)), nil
}

func killRemote(ctx context.Context, h Host, name string) error {
	_, err := batch(ctx, h, "tmux kill-session -t "+shellquote("="+name), nil)
	return err
}

// dialAddr is the host:port to probe for reachability.
func (h Host) dialAddr() string {
	host := h.Target
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	port := "22"
	f := strings.Fields(h.Args)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "-p" {
			port = f[i+1]
		}
	}
	return net.JoinHostPort(host, port)
}

// reachable reports whether the host's ssh port answers. "unknown" covers ssh
// config aliases and jump hosts, where a direct dial says nothing.
func reachable(h Host) string {
	if h.isLocal() {
		return "up"
	}
	if strings.Contains(h.Args, "-J") || strings.Contains(h.Args, "ProxyJump") || strings.Contains(h.Args, "ProxyCommand") {
		return "unknown"
	}
	c, err := net.DialTimeout("tcp", h.dialAddr(), 2500*time.Millisecond)
	if err != nil {
		if _, ok := err.(*net.OpError); ok && strings.Contains(err.Error(), "no such host") {
			return "unknown"
		}
		return "down"
	}
	c.Close()
	return "up"
}
