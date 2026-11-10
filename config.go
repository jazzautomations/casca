package main

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Host is one machine reachable by ssh from where casca runs.
type Host struct {
	Name   string `json:"name"`            // display name
	Target string `json:"target"`          // user@host, or "local" for this machine
	Group  string `json:"group,omitempty"` // sidebar section
	Args   string `json:"args,omitempty"`  // extra ssh args, e.g. "-p 2222 -J bastion"
	Dir    string `json:"dir,omitempty"`   // default working directory for new sessions
}

// Agent is something launchable on a host: a shell or a coding agent CLI.
type Agent struct {
	Name string `json:"name"` // display name, also the tmux session suffix
	Cmd  string `json:"cmd"`  // typed into the new session; empty = plain shell
	Tmux bool   `json:"tmux"` // run inside a persistent remote tmux session
}

type Snippet struct {
	Label string `json:"label"`
	Cmd   string `json:"cmd"` // sent to the terminal; a trailing "\n" runs it
}

type Config struct {
	Token     string    `json:"token"`      // empty = no auth
	Listen    string    `json:"listen"`     // comma list; "tailscale:PORT" binds the tailscale0 address
	TmuxMouse *bool     `json:"tmux_mouse"` // mouse/touch scrolling inside tmux (default true)
	Hosts     []Host    `json:"hosts"`
	Agents    []Agent   `json:"agents"`
	Snippets  []Snippet `json:"snippets"`
}

const defaultListen = "127.0.0.1:7681,tailscale:7681"

func (c Config) tmuxMouse() bool { return c.TmuxMouse == nil || *c.TmuxMouse }

var (
	cfgMu   sync.RWMutex
	cfg     Config
	cfgPath string
	cfgMod  time.Time
)

func defaultConfigPath() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "casca", "config.json")
	}
	return "config.json"
}

func normalize(c *Config) {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if len(c.Agents) == 0 {
		c.Agents = defaultConfig().Agents
	}
}

func loadConfig(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	normalize(&c)
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if st, err := os.Stat(path); err == nil {
		cfgMod = st.ModTime()
	}
	cfg = c
	cfgPath = path
	return nil
}

// getConfig returns the current config, hot-reloading when the file changed.
// A broken edit keeps the last good config instead of taking the server down.
func getConfig() Config {
	cfgMu.RLock()
	path, mod := cfgPath, cfgMod
	cfgMu.RUnlock()
	if path != "" {
		if st, err := os.Stat(path); err == nil && st.ModTime().After(mod) {
			if err := loadConfig(path); err != nil {
				log.Printf("config: reload ignored: %v", err)
				cfgMu.Lock()
				cfgMod = st.ModTime()
				cfgMu.Unlock()
			} else {
				log.Printf("config: reloaded")
			}
		}
	}
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

// updateConfig applies fn to the config and writes it back atomically.
func updateConfig(fn func(*Config) error) error {
	getConfig() // pick up any manual edit first
	cfgMu.Lock()
	defer cfgMu.Unlock()
	c := cfg
	c.Hosts = append([]Host(nil), cfg.Hosts...)
	if err := fn(&c); err != nil {
		return err
	}
	if err := writeConfig(cfgPath, c); err != nil {
		return err
	}
	cfg = c
	if st, err := os.Stat(cfgPath); err == nil {
		cfgMod = st.ModTime()
	}
	return nil
}

func writeConfig(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func defaultConfig() Config {
	return Config{
		Token:  newToken(),
		Listen: defaultListen,
		Hosts: []Host{
			{Name: "esta máquina", Target: "local", Group: "local"},
		},
		Agents: []Agent{
			{Name: "shell", Cmd: "", Tmux: true},
			{Name: "claude", Cmd: "claude", Tmux: true},
			{Name: "codex", Cmd: "codex", Tmux: true},
			{Name: "devin", Cmd: "devin", Tmux: true},
			{Name: "jazz", Cmd: "jazz", Tmux: true},
		},
		Snippets: []Snippet{
			{Label: "tmux ls", Cmd: "tmux ls\n"},
			{Label: "git status", Cmd: "git status\n"},
			{Label: "htop", Cmd: "htop\n"},
		},
	}
}
