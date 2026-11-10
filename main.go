package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const version = "0.2.0"

//go:embed web
var webFS embed.FS

// ---------- auth ----------

func tokenOK(tok string) bool {
	want := getConfig().Token
	return want == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1
}

func authed(r *http.Request) bool {
	if getConfig().Token == "" {
		return true
	}
	if ck, err := r.Cookie("casca_token"); err == nil && tokenOK(ck.Value) {
		return true
	}
	return false
}

// sameOrigin rejects requests a foreign web page made the browser send.
// Without it any site could open /ws with the visitor's cookie and get a shell.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // not a browser cross-origin request
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func setCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: "casca_token", Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: 60 * 60 * 24 * 365,
	})
}

func guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// ---------- websocket client ----------

const maxQueued = 4 << 20 // bytes of output a client may fall behind before it is cut loose

type frame struct {
	binary bool
	data   []byte
}

// wsClient is one browser connection. All terminals of that browser share it.
type wsClient struct {
	conn   *websocket.Conn
	out    chan frame
	queued atomic.Int64
	sent   atomic.Int64 // terminal bytes pushed
	acked  atomic.Int64 // terminal bytes the browser reports as rendered
	quit   chan struct{}
	once   sync.Once

	mu   sync.Mutex
	subs map[string]*Session
}

var (
	clientsMu sync.Mutex
	clients   = map[*wsClient]struct{}{}
)

func (c *wsClient) lag() int64 { return c.sent.Load() - c.acked.Load() }

func (c *wsClient) shutdown() {
	c.once.Do(func() {
		close(c.quit)
		c.conn.Close()
	})
}

// push never blocks: a PTY reader calls it while holding the session lock. A
// client that cannot keep up is disconnected; it reconnects and repaints,
// which is cheaper and more correct than a terminal with holes in its stream.
func (c *wsClient) push(f frame) {
	if c.queued.Add(int64(len(f.data))) > maxQueued {
		c.shutdown()
		return
	}
	select {
	case c.out <- f:
	case <-c.quit:
	default:
		c.shutdown()
	}
}

// Output frame: 1 byte id length, id, raw terminal bytes. Binary because a
// PTY read can end in the middle of a UTF-8 sequence.
func (c *wsClient) pushData(id string, data []byte) {
	b := make([]byte, 0, 1+len(id)+len(data))
	b = append(b, byte(len(id)))
	b = append(b, id...)
	b = append(b, data...)
	c.sent.Add(int64(len(data)))
	c.push(frame{binary: true, data: b})
}

func (c *wsClient) pushJSON(v any) {
	b, _ := json.Marshal(v)
	c.push(frame{data: b})
}

func (c *wsClient) pushState(s *Session) {
	in := s.Info()
	c.pushJSON(map[string]any{"type": "state", "id": in.ID, "state": in.State, "attempt": in.Attempt})
}

func (c *wsClient) writer() {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case f := <-c.out:
			c.queued.Add(-int64(len(f.data)))
			mt := websocket.TextMessage
			if f.binary {
				mt = websocket.BinaryMessage
			}
			c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := c.conn.WriteMessage(mt, f.data); err != nil {
				c.shutdown()
				return
			}
		case <-ping.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.shutdown()
				return
			}
		case <-c.quit:
			return
		}
	}
}

func (c *wsClient) attach(s *Session) {
	c.mu.Lock()
	if _, ok := c.subs[s.ID]; ok {
		c.mu.Unlock()
		return
	}
	c.subs[s.ID] = s
	c.mu.Unlock()
	replay, redraw := s.Attach(c)
	c.pushState(s)
	if len(replay) > 0 {
		c.pushData(s.ID, replay)
	}
	if redraw {
		s.Redraw()
	}
}

func (c *wsClient) detach(id string) {
	c.mu.Lock()
	s := c.subs[id]
	delete(c.subs, id)
	c.mu.Unlock()
	if s != nil {
		s.Detach(c)
	}
}

// sessionsChanged tells every browser to refresh its tab list, so a session
// opened on the phone shows up on the laptop and the other way round.
func sessionsChanged() {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for c := range clients {
		c.pushJSON(map[string]any{"type": "sessions"})
	}
}

type inMsg struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Host   int    `json:"host"`
	Agent  int    `json:"agent"`  // index into config.Agents, -1 = custom
	Cmd    string `json:"cmd"`    // custom command
	Tmux   bool   `json:"tmux"`   // custom command: keep it in tmux
	Name   string `json:"name"`   // tmux session name (optional)
	Dir    string `json:"dir"`    // working directory (optional)
	Attach bool   `json:"attach"` // attach to existing tmux session Name
	Data   string `json:"data"`
	N      int64  `json:"n"` // ack: bytes rendered since the last ack
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

// planOpen turns an open request into what to run.
func planOpen(c Config, m inMsg) (h Host, agent, label string, sp Spec, err error) {
	if m.Host < 0 || m.Host >= len(c.Hosts) {
		return h, "", "", sp, errors.New("host inválido")
	}
	h = c.Hosts[m.Host]
	sp = Spec{Dir: m.Dir, Mouse: c.tmuxMouse()}
	if sp.Dir == "" {
		sp.Dir = h.Dir
	}
	if m.Attach {
		if m.Name == "" {
			return h, "", "", sp, errors.New("sessão sem nome")
		}
		sp = Spec{Tmux: m.Name, Attach: true}
		return h, "tmux", h.Name + " · " + m.Name, sp, nil
	}
	a := Agent{Name: "custom", Cmd: m.Cmd, Tmux: m.Tmux}
	if m.Agent >= 0 && m.Agent < len(c.Agents) {
		a = c.Agents[m.Agent]
	} else if strings.TrimSpace(m.Cmd) == "" {
		return h, "", "", sp, errors.New("comando vazio")
	}
	sp.Cmd = a.Cmd
	short := a.Name
	if a.Tmux {
		sp.Tmux = tmuxName(m.Name)
		if sp.Tmux == "" {
			sp.Tmux = "c-" + tmuxName(a.Name)
		}
		short = sp.Tmux
	}
	return h, a.Name, h.Name + " · " + short, sp, nil
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{CheckOrigin: sameOrigin, ReadBufferSize: 4096, WriteBufferSize: 32 * 1024}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsClient{conn: conn, out: make(chan frame, 1024), quit: make(chan struct{}), subs: map[string]*Session{}}
	clientsMu.Lock()
	clients[c] = struct{}{}
	clientsMu.Unlock()
	go c.writer()
	defer func() {
		c.shutdown()
		clientsMu.Lock()
		delete(clients, c)
		clientsMu.Unlock()
		c.mu.Lock()
		subs := c.subs
		c.subs = map[string]*Session{}
		c.mu.Unlock()
		for _, s := range subs {
			s.Detach(c)
		}
	}()

	conn.SetReadLimit(1 << 20)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		var m inMsg
		if err := conn.ReadJSON(&m); err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		switch m.Type {
		case "open":
			h, agent, label, sp, err := planOpen(getConfig(), m)
			if err != nil {
				c.pushJSON(map[string]any{"type": "error", "id": m.ID, "error": err.Error()})
				continue
			}
			// One tab per remote tmux session: a second attach would only mirror it.
			if sp.Tmux != "" {
				if dup := findTmux(m.Host, sp.Tmux); dup != "" {
					c.pushJSON(map[string]any{"type": "exists", "id": m.ID, "existing": dup})
					continue
				}
			}
			s, err := mgr.Open(m.ID, m.Host, h, agent, label, sp, m.Cols, m.Rows)
			if err != nil {
				c.pushJSON(map[string]any{"type": "error", "id": m.ID, "error": err.Error()})
				continue
			}
			c.attach(s)
			sessionsChanged()

		case "attach":
			if s := mgr.Get(m.ID); s != nil {
				c.attach(s)
			} else {
				c.pushJSON(map[string]any{"type": "gone", "id": m.ID})
			}

		case "stdin":
			if s := mgr.Get(m.ID); s != nil {
				s.Write([]byte(m.Data))
			}

		case "resize":
			if s := mgr.Get(m.ID); s != nil {
				s.Resize(m.Cols, m.Rows)
			}

		case "retry":
			if s := mgr.Get(m.ID); s != nil {
				s.Retry()
			}

		case "close":
			c.detach(m.ID)
			mgr.Close(m.ID)
			sessionsChanged()

		case "ack":
			if m.N > 0 {
				c.acked.Add(m.N)
			}

		case "ping":
			c.pushJSON(map[string]any{"type": "pong"})
		}
	}
}

func findTmux(host int, name string) string {
	for _, s := range mgr.List() {
		in := s.Info()
		if in.Host == host && in.Tmux == name && in.State != stDead {
			return in.ID
		}
	}
	return ""
}

// ---------- http api ----------

func hostParam(r *http.Request) (int, Host, error) {
	c := getConfig()
	i, err := strconv.Atoi(r.URL.Query().Get("host"))
	if err != nil || i < 0 || i >= len(c.Hosts) {
		return 0, Host{}, errors.New("host inválido")
	}
	return i, c.Hosts[i], nil
}

func apiConfig(w http.ResponseWriter, r *http.Request) {
	c := getConfig()
	writeJSON(w, map[string]any{"hosts": c.Hosts, "agents": c.Agents, "snippets": c.Snippets, "version": version})
}

func apiSessions(w http.ResponseWriter, r *http.Request) {
	out := []Info{}
	for _, s := range mgr.List() {
		out = append(out, s.Info())
	}
	writeJSON(w, out)
}

// apiStatus probes every host's ssh port in parallel.
func apiStatus(w http.ResponseWriter, r *http.Request) {
	hosts := getConfig().Hosts
	res := make([]string, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h Host) {
			defer wg.Done()
			res[i] = reachable(h)
		}(i, h)
	}
	wg.Wait()
	writeJSON(w, res)
}

func apiRemote(w http.ResponseWriter, r *http.Request) {
	_, h, err := hostParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	list, err := listRemote(ctx, h)
	if err != nil {
		msg := "sem acesso"
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		writeJSON(w, map[string]any{"error": msg, "sessions": []RemoteSession{}})
		return
	}
	writeJSON(w, map[string]any{"sessions": list})
}

func apiKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	i, h, err := hostParam(r)
	name := r.URL.Query().Get("name")
	if err != nil || name == "" {
		http.Error(w, "host/name inválido", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if err := killRemote(ctx, h, name); err != nil {
		http.Error(w, "não consegui encerrar: "+err.Error(), http.StatusBadGateway)
		return
	}
	if id := findTmux(i, name); id != "" {
		mgr.Close(id)
	}
	sessionsChanged()
	writeJSON(w, map[string]any{"ok": true})
}

func apiHosts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost: // add, or replace when ?i= is given
		var h Host
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&h); err != nil {
			http.Error(w, "json inválido", http.StatusBadRequest)
			return
		}
		h.Name, h.Target = strings.TrimSpace(h.Name), strings.TrimSpace(h.Target)
		if h.Target == "" || strings.HasPrefix(h.Target, "-") || strings.ContainsAny(h.Target, " \t\n") {
			http.Error(w, "target deve ser user@host", http.StatusBadRequest)
			return
		}
		if h.Name == "" {
			h.Name = h.Target
		}
		err := updateConfig(func(c *Config) error {
			if q := r.URL.Query().Get("i"); q != "" {
				i, err := strconv.Atoi(q)
				if err != nil || i < 0 || i >= len(c.Hosts) {
					return errors.New("índice inválido")
				}
				c.Hosts[i] = h
				return nil
			}
			c.Hosts = append(c.Hosts, h)
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	case http.MethodDelete:
		i, err := strconv.Atoi(r.URL.Query().Get("i"))
		if err != nil {
			http.Error(w, "índice inválido", http.StatusBadRequest)
			return
		}
		// Open sessions refer to hosts by index, so a host in use stays.
		for _, s := range mgr.List() {
			if s.Info().Host >= i {
				http.Error(w, "feche as abas abertas antes de remover um host", http.StatusConflict)
				return
			}
		}
		err = updateConfig(func(c *Config) error {
			if i < 0 || i >= len(c.Hosts) {
				return errors.New("índice inválido")
			}
			c.Hosts = append(c.Hosts[:i], c.Hosts[i+1:]...)
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "método", http.StatusMethodNotAllowed)
		return
	}
	apiConfig(w, r)
}

// apiUpload stores the request body as a file in /tmp on the host and returns
// its path, so a screenshot pasted in the browser can be handed to an agent.
func apiUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	_, h, err := hostParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ext := nameRe.ReplaceAllString(filepath.Ext(r.URL.Query().Get("name")), "")
	if ext != "" {
		ext = "." + ext
	}
	rb := make([]byte, 4)
	rand.Read(rb)
	path := "/tmp/casca-" + time.Now().Format("0102-150405") + "-" + hex.EncodeToString(rb) + ext
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, 25<<20)
	if _, err := batch(ctx, h, "umask 077; cat > "+shellquote(path), body); err != nil {
		http.Error(w, "upload falhou: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"path": path})
}

// ---------- listening ----------

// tailscaleIP finds this machine's tailnet IPv4 without assuming it exists yet:
// at boot casca can start before tailscaled has brought the interface up.
func tailscaleIP() (string, error) {
	if ifc, err := net.InterfaceByName("tailscale0"); err == nil {
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
					return n.IP.String(), nil
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output()
	if err == nil {
		if ip := strings.TrimSpace(strings.Split(string(out), "\n")[0]); net.ParseIP(ip) != nil {
			return ip, nil
		}
	}
	return "", errors.New("tailscale ainda sem IP")
}

func resolveListen(spec string) (string, error) {
	if port, ok := strings.CutPrefix(spec, "tailscale:"); ok {
		ip, err := tailscaleIP()
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(ip, port), nil
	}
	return spec, nil
}

// serveOn keeps one listen address alive, retrying while it cannot bind.
func serveOn(spec string, h http.Handler) {
	logged := false
	for {
		addr, err := resolveListen(spec)
		var ln net.Listener
		if err == nil {
			ln, err = net.Listen("tcp", addr)
		}
		if err != nil {
			if !logged {
				log.Printf("listen %s: %v (tentando de novo a cada 5s)", spec, err)
				logged = true
			}
			time.Sleep(5 * time.Second)
			continue
		}
		logged = false
		log.Printf("casca %s em http://%s", version, addr)
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		err = srv.Serve(ln)
		log.Printf("listen %s: %v", addr, err)
		time.Sleep(2 * time.Second)
	}
}

func newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !sameOrigin(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		tok := strings.TrimSpace(r.FormValue("token"))
		if getConfig().Token != "" && tokenOK(tok) {
			setCookie(w, tok)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		time.Sleep(time.Second) // slow down guessing
		http.Redirect(w, r, "/?bad=1", http.StatusSeeOther)
	})
	mux.HandleFunc("/api/config", guard(apiConfig))
	mux.HandleFunc("/api/sessions", guard(apiSessions))
	mux.HandleFunc("/api/status", guard(apiStatus))
	mux.HandleFunc("/api/remote", guard(apiRemote))
	mux.HandleFunc("/api/kill", guard(apiKill))
	mux.HandleFunc("/api/hosts", guard(apiHosts))
	mux.HandleFunc("/api/upload", guard(apiUpload))
	mux.HandleFunc("/ws", guard(wsHandler))

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	static := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A link carrying the token logs the device in once and drops it from the URL.
		if tok := r.URL.Query().Get("token"); tok != "" && r.URL.Path == "/" {
			if getConfig().Token != "" && tokenOK(tok) {
				setCookie(w, tok)
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		static.ServeHTTP(w, r)
	})
	return mux
}

// ---------- entry ----------

func usage() {
	fmt.Fprint(os.Stderr, `casca `+version+` — abre shells e agentes de código em outros hosts

uso:
  casca [serve]     sobe a interface web (padrão)
  casca tui         menu no terminal: escolhe host e agente, conecta por ssh+tmux
  casca init        cria a config com um token novo
  casca url         mostra os endereços de acesso com o token
  casca connect [-agent A | -attach S | -cmd C] [-name N] [-dir D] HOST
                    conecta em primeiro plano, reconectando se a rede cair
  casca ls HOST     sessões tmux vivas no host (JSON)
  casca status      quais hosts respondem (JSON)
  casca kill HOST SESSAO
  casca version

opções:
  -conf caminho     arquivo de config (padrão: ~/.config/casca/config.json)
  -listen addr      sobrepõe o listen da config (ex: 127.0.0.1:7681,tailscale:7681)
`)
}

func main() {
	args := os.Args[1:]
	sub := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("casca", flag.ExitOnError)
	fs.Usage = usage
	conf := fs.String("conf", defaultConfigPath(), "config file")
	listen := fs.String("listen", "", "listen addresses")
	cAgent := fs.String("agent", "", "connect: agent name")
	cAttach := fs.String("attach", "", "connect: attach to this existing tmux session")
	cName := fs.String("name", "", "connect: tmux session name")
	cDir := fs.String("dir", "", "connect: working directory on the host")
	cCmd := fs.String("cmd", "", "connect: command to type into a new session")
	fs.Parse(args)

	if sub == "version" {
		fmt.Println(version)
		return
	}
	if sub == "help" {
		usage()
		return
	}

	created := false
	if _, err := os.Stat(*conf); errors.Is(err, os.ErrNotExist) {
		if err := writeConfig(*conf, defaultConfig()); err != nil {
			log.Fatalf("config: %v", err)
		}
		created = true
	} else if sub == "init" {
		fmt.Println("config já existe:", *conf)
		return
	}
	if err := loadConfig(*conf); err != nil {
		log.Fatalf("config: %v", err)
	}
	c := getConfig()
	if created || sub == "init" {
		fmt.Println("config criada:", *conf)
	}
	spec := c.Listen
	if *listen != "" {
		spec = *listen
	}

	switch sub {
	case "init", "url":
		for _, a := range strings.Split(spec, ",") {
			addr, err := resolveListen(strings.TrimSpace(a))
			if err != nil {
				fmt.Printf("  %s: %v\n", a, err)
				continue
			}
			fmt.Printf("  http://%s/?token=%s\n", addr, c.Token)
		}
	case "tui":
		os.Exit(runTUI())
	case "connect", "ls", "status", "kill":
		os.Exit(runCLI(sub, c, fs.Args(), *cAgent, *cAttach, *cName, *cDir, *cCmd))
	case "serve":
		h := newMux()
		parts := strings.Split(spec, ",")
		for _, a := range parts[1:] {
			go serveOn(strings.TrimSpace(a), h)
		}
		serveOn(strings.TrimSpace(parts[0]), h)
	default:
		usage()
		os.Exit(2)
	}
}
