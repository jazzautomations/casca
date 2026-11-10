package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The terminal UI is the same idea as the web one with nothing between the
// keyboard and ssh: pick a host, pick an agent or a live session, and casca
// runs ssh+tmux in the foreground, reconnecting when the link drops. Detach
// from tmux (prefix d) to come back to the menu; the agent keeps running.

type tui struct {
	fd    int
	saved *unix.Termios
	raw   bool
}

func (t *tui) enterRaw() {
	if t.raw {
		return
	}
	st, err := unix.IoctlGetTermios(t.fd, unix.TCGETS)
	if err != nil {
		return
	}
	t.saved = st
	r := *st
	r.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	r.Iflag &^= unix.IXON | unix.ICRNL
	// Reads return after 100 ms with nothing, so the menu can repaint when a
	// background probe finishes without a reader goroutine that would steal
	// keystrokes from ssh later.
	r.Cc[unix.VMIN], r.Cc[unix.VTIME] = 0, 1
	unix.IoctlSetTermios(t.fd, unix.TCSETS, &r)
	t.raw = true
	fmt.Print("\x1b[?25l")
}

func (t *tui) leaveRaw() {
	if !t.raw {
		return
	}
	unix.IoctlSetTermios(t.fd, unix.TCSETS, t.saved)
	t.raw = false
	fmt.Print("\x1b[?25h")
}

// key reads one key, or "" on timeout.
func (t *tui) key() string {
	b := make([]byte, 16)
	n, _ := unix.Read(t.fd, b)
	if n <= 0 {
		return ""
	}
	s := string(b[:n])
	switch s {
	case "\x1b[A", "\x1bOA", "k":
		return "up"
	case "\x1b[B", "\x1bOB", "j":
		return "down"
	case "\r", "\n", "\x1b[C", "\x1bOC", "l":
		return "enter"
	case "\x1b", "\x1b[D", "\x1bOD", "h", "\x7f":
		return "back"
	case "\x03", "q":
		return "quit"
	}
	return s
}

func (t *tui) size() (cols, rows int) {
	ws, err := unix.IoctlGetWinsize(t.fd, unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
}

func clip(s string, n int) string {
	r := []rune(s)
	if n < 1 {
		return ""
	}
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

type row struct {
	mark string // "", "up", "down", "unknown": a status dot in front
	text string
	dim  string
}

func (t *tui) draw(title, help string, rows []row, sel int) {
	cols, lines := t.size()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	fmt.Fprintf(&b, "\x1b[1;36m casca\x1b[0m \x1b[2m%s\x1b[0m\r\n\r\n", clip(title, cols-8))
	max := lines - 5
	start := 0
	if sel >= max {
		start = sel - max + 1
	}
	for i := start; i < len(rows) && i < start+max; i++ {
		lead := "  "
		if rows[i].mark != "" {
			lead = dot(rows[i].mark) + " "
		}
		line := clip(rows[i].text, cols-6)
		dim := clip(rows[i].dim, cols-7-len([]rune(line)))
		if i == sel {
			fmt.Fprintf(&b, " %s\x1b[1;7m %s \x1b[0m \x1b[2m%s\x1b[0m\r\n", lead, line, dim)
		} else {
			fmt.Fprintf(&b, " %s %s  \x1b[2m%s\x1b[0m\r\n", lead, line, dim)
		}
	}
	fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2m %s\x1b[0m", lines, clip(help, cols-2))
	os.Stdout.WriteString(b.String())
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func dot(state string) string {
	switch state {
	case "up":
		return "\x1b[32m●\x1b[0m"
	case "down":
		return "\x1b[31m●\x1b[0m"
	}
	return "\x1b[2m○\x1b[0m"
}

func runTUI() int {
	fd := int(os.Stdin.Fd())
	if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err != nil {
		fmt.Fprintln(os.Stderr, "casca tui precisa de um terminal")
		return 1
	}
	t := &tui{fd: fd}
	defer t.leaveRaw()
	// Ctrl-C reaches casca only while it waits to reconnect; ssh owns the tty otherwise.
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	sel := 0
	status := map[string]string{}
	probed := make(chan [2]string, 64)
	probe := func() {
		for _, h := range getConfig().Hosts {
			go func(h Host) { probed <- [2]string{h.Target, reachable(h)} }(h)
		}
	}
	probe()
	last := time.Now()
	for {
		hosts := getConfig().Hosts
		if sel >= len(hosts) {
			sel = maxInt(0, len(hosts)-1)
		}
		rows := make([]row, len(hosts))
		for i, h := range hosts {
			st := status[h.Target]
			if st == "" {
				st = "unknown"
			}
			rows[i] = row{mark: st, text: fmt.Sprintf("%-22s", h.Name), dim: h.Target}
		}
		t.enterRaw()
		t.draw("hosts", "↑↓ escolher · enter abrir · r atualizar · a novo host · q sair", rows, sel)
	wait:
		for {
			select {
			case p := <-probed:
				status[p[0]] = p[1]
				break wait
			case <-sig:
				return 0
			default:
			}
			if time.Since(last) > 20*time.Second {
				last = time.Now()
				probe()
			}
			switch k := t.key(); k {
			case "":
				continue
			case "up":
				if sel > 0 {
					sel--
				}
			case "down":
				if sel < len(hosts)-1 {
					sel++
				}
			case "quit":
				t.leaveRaw()
				fmt.Print("\x1b[H\x1b[2J")
				return 0
			case "r":
				probe()
			case "a":
				t.leaveRaw()
				addHostPrompt()
				probe()
			case "enter":
				if len(hosts) > 0 {
					t.hostMenu(sel, hosts[sel], sig)
					probe()
				}
			default:
				if len(k) == 1 && k[0] >= '1' && k[0] <= '9' && int(k[0]-'1') < len(hosts) {
					sel = int(k[0] - '1')
				}
			}
			break
		}
	}
}

func addHostPrompt() {
	in := bufio.NewReader(os.Stdin)
	ask := func(q string) string {
		fmt.Print(q)
		s, _ := in.ReadString('\n')
		return strings.TrimSpace(s)
	}
	fmt.Print("\x1b[H\x1b[2J\x1b[1mnovo host\x1b[0m (enter vazio cancela)\n\n")
	target := ask("  user@host: ")
	if target == "" || strings.HasPrefix(target, "-") || strings.ContainsAny(target, " \t") {
		return
	}
	name := ask("  nome [" + target + "]: ")
	if name == "" {
		name = target
	}
	dir := ask("  diretório padrão (opcional): ")
	if err := updateConfig(func(c *Config) error {
		c.Hosts = append(c.Hosts, Host{Name: name, Target: target, Group: "hosts", Dir: dir})
		return nil
	}); err != nil {
		fmt.Println("erro:", err)
		time.Sleep(2 * time.Second)
	}
}

type choice struct {
	row  row
	spec Spec
	live bool
}

func (t *tui) hostMenu(hostIdx int, h Host, sig chan os.Signal) {
	sel := 0
	var live []RemoteSession
	liveErr := ""
	loading := true
	type result struct {
		list []RemoteSession
		err  error
	}
	got := make(chan result, 4)
	fetch := func() {
		loading = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			l, err := listRemote(ctx, h)
			got <- result{l, err}
		}()
	}
	fetch()
	for {
		c := getConfig()
		var items []choice
		running := map[string]RemoteSession{}
		if !loading {
			for _, r := range live {
				running[r.Name] = r
			}
		}
		for _, a := range c.Agents {
			sp := Spec{Cmd: a.Cmd, Dir: h.Dir, Mouse: c.tmuxMouse()}
			dim := "novo"
			if a.Tmux {
				sp.Tmux = "c-" + tmuxName(a.Name)
				if r, ok := running[sp.Tmux]; ok {
					dim = "● rodando (" + r.Command + ") — reabrir"
					delete(running, sp.Tmux)
				}
			}
			items = append(items, choice{row: row{text: fmt.Sprintf("%-14s", a.Name), dim: dim}, spec: sp})
		}
		if !loading {
			for _, r := range live {
				if _, ok := running[r.Name]; !ok {
					continue
				}
				items = append(items, choice{
					row:  row{text: fmt.Sprintf("%-14s", r.Name), dim: "● tmux vivo (" + r.Command + ")"},
					spec: Spec{Tmux: r.Name, Attach: true}, live: true,
				})
			}
		}
		title := h.Name + " — " + h.Target
		if loading {
			title += "  (vendo o que está rodando…)"
		} else if liveErr != "" {
			title += "  (" + liveErr + ")"
		}
		if sel >= len(items) {
			sel = maxInt(0, len(items)-1)
		}
		rows := make([]row, len(items))
		for i := range items {
			rows[i] = items[i].row
		}
		t.enterRaw()
		t.draw(title, "enter conectar · n nome/pasta · x encerrar sessão · r atualizar · ← voltar", rows, sel)
	wait:
		for {
			select {
			case r := <-got:
				loading = false
				live, liveErr = r.list, ""
				if r.err != nil {
					liveErr = "não consegui listar as sessões do host"
				}
				break wait
			case <-sig:
				return
			default:
			}
			switch k := t.key(); k {
			case "":
				continue
			case "up":
				if sel > 0 {
					sel--
				}
			case "down":
				if sel < len(items)-1 {
					sel++
				}
			case "back", "quit":
				return
			case "r":
				fetch()
			case "x":
				// Killing a session kills the agent in it: ask first.
				if sel < len(items) && items[sel].spec.Tmux != "" {
					name := items[sel].spec.Tmux
					t.draw(title, "encerrar \""+name+"\" e o que estiver rodando nela? s = sim, outra tecla = não", rows, sel)
					k := ""
					for k == "" {
						k = t.key()
					}
					if k == "s" || k == "y" {
						ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
						killRemote(ctx, h, name)
						cancel()
						fetch()
					}
				}
			case "n":
				if sel < len(items) && !items[sel].live {
					t.leaveRaw()
					sp := items[sel].spec
					in := bufio.NewReader(os.Stdin)
					fmt.Print("\x1b[H\x1b[2J  nome da sessão [" + sp.Tmux + "]: ")
					if s, _ := in.ReadString('\n'); tmuxName(s) != "" {
						sp.Tmux = tmuxName(s)
					}
					fmt.Print("  pasta [" + sp.Dir + "]: ")
					if s, _ := in.ReadString('\n'); strings.TrimSpace(s) != "" {
						sp.Dir = strings.TrimSpace(s)
					}
					connectLoop(h, sp, sig)
					fetch()
				}
			case "enter":
				if sel < len(items) {
					t.leaveRaw()
					connectLoop(h, items[sel].spec, sig)
					fetch()
				}
			}
			break
		}
	}
}

// capWriter keeps the first bytes written through it.
type capWriter struct {
	w   io.Writer
	buf bytes.Buffer
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.buf.Len() < 2048 {
		c.buf.Write(p)
	}
	return c.w.Write(p)
}

// connectLoop runs the session in the foreground and respawns ssh while the
// failure looks like the network rather than the far side ending the session.
func connectLoop(h Host, sp Spec, sig chan os.Signal) {
	fmt.Print("\x1b[H\x1b[2J")
	failures := 0
	created := sp.Attach
	patient := false
	for {
		for len(sig) > 0 {
			<-sig
		}
		started := time.Now()
		// Respawns only attach: see Session.runOnce.
		use := sp
		if sp.Tmux != "" && created {
			use.Attach = true
		}
		flag := connFlag()
		cmd := command(h, use, flag, patient)
		cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
		errw := &capWriter{w: os.Stderr}
		cmd.Stderr = errw
		cmd.Env = cleanEnv()
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if code < 0 {
				code = 255
			}
		} else if err != nil {
			fmt.Printf("\r\n[casca] não consegui iniciar: %v\r\n", err)
			time.Sleep(2 * time.Second)
			return
		}
		if h.isLocal() || sp.Tmux == "" || code != 255 {
			os.Remove(flag)
			if code != 0 {
				fmt.Printf("\r\n[casca] encerrou com status %d — enter pra voltar", code)
				bufio.NewReader(os.Stdin).ReadString('\n')
			}
			return
		}
		in := connectedFor(flag)
		os.Remove(flag)
		if in >= 3*time.Second {
			created = true
		}
		patient = in < 0 && needsApproval(errw.buf.Bytes())
		if patient && code == 255 {
			fmt.Print("\r\n\x1b[33m[casca] o host pede aprovação: vou esperar até 10 min no próximo link\x1b[0m\r\n")
		}
		if time.Since(started) >= stableAfter {
			failures = 0
		} else if reason := fatalSSH(errw.buf.Bytes()); reason != "" {
			fmt.Printf("\r\n[casca] ssh recusou (%s) — enter pra voltar", reason)
			bufio.NewReader(os.Stdin).ReadString('\n')
			return
		}
		failures++
		wait := backoff(failures)
		fmt.Printf("\r\n\x1b[33m[casca] conexão caiu — reconectando em %s (tentativa %d, Ctrl-C volta ao menu)\x1b[0m\r\n", wait, failures)
		select {
		case <-time.After(wait):
		case <-sig:
			return
		}
	}
}
