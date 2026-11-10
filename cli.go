package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// findHost accepts a host index or a (case-insensitive) name.
func findHost(c Config, arg string) (int, Host, bool) {
	if i, err := strconv.Atoi(arg); err == nil && i >= 0 && i < len(c.Hosts) {
		return i, c.Hosts[i], true
	}
	for i, h := range c.Hosts {
		if strings.EqualFold(h.Name, arg) || strings.EqualFold(h.Target, arg) {
			return i, h, true
		}
	}
	return 0, Host{}, false
}

// runCLI serves the one-shot commands other front ends (the GTK app, scripts)
// build on, so the ssh/tmux/reconnect logic lives in one place.
func runCLI(sub string, c Config, args []string, agent, attach, name, dir, cmd string) int {
	out := json.NewEncoder(os.Stdout)
	if sub == "status" {
		res := make([]string, len(c.Hosts))
		var wg sync.WaitGroup
		for i, h := range c.Hosts {
			wg.Add(1)
			go func(i int, h Host) { defer wg.Done(); res[i] = reachable(h) }(i, h)
		}
		wg.Wait()
		out.Encode(res)
		return 0
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "falta o host (índice ou nome)")
		return 2
	}
	idx, h, ok := findHost(c, args[0])
	if !ok {
		fmt.Fprintln(os.Stderr, "host desconhecido:", args[0])
		return 2
	}
	switch sub {
	case "ls":
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		list, err := listRemote(ctx, h)
		if err != nil {
			out.Encode(map[string]any{"error": "sem acesso ao host", "sessions": []RemoteSession{}})
			return 1
		}
		out.Encode(map[string]any{"sessions": list})
	case "kill":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "falta o nome da sessão")
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := killRemote(ctx, h, args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "não consegui encerrar:", err)
			return 1
		}
	case "connect":
		m := inMsg{Host: idx, Agent: -1, Name: name, Dir: dir, Cmd: cmd, Tmux: true}
		switch {
		case attach != "":
			m.Attach, m.Name = true, attach
		case cmd != "":
		default:
			if agent == "" {
				agent = c.Agents[0].Name
			}
			m.Agent = -2
			for i, a := range c.Agents {
				if strings.EqualFold(a.Name, agent) {
					m.Agent = i
				}
			}
			if m.Agent == -2 {
				fmt.Fprintln(os.Stderr, "agente desconhecido:", agent)
				return 2
			}
		}
		_, _, _, sp, err := planOpen(c, m)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		sig := make(chan os.Signal, 4)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		connectLoop(h, sp, sig)
	}
	return 0
}
