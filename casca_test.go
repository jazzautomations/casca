package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTmuxName(t *testing.T) {
	for in, want := range map[string]string{
		"api refactor": "api-refactor", "c-claude": "c-claude", "a.b:c": "a-b-c",
		"$(rm -rf /)": "rm--rf", "  ": "", "'; reboot #": "reboot",
	} {
		if got := tmuxName(in); got != want {
			t.Errorf("tmuxName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{1, 1, 2, 4, 8, 15, 15, 15}
	for n, w := range want {
		if got := backoff(n); got != w*time.Second {
			t.Errorf("backoff(%d) = %v, want %v", n, got, w*time.Second)
		}
	}
}

func TestFatalSSH(t *testing.T) {
	if fatalSSH([]byte("dev@x: Permission denied (publickey).")) == "" {
		t.Error("auth failure must be fatal")
	}
	for _, transient := range []string{
		"ssh: connect to host laptop port 22: Connection timed out",
		"ssh: Could not resolve hostname desktop: Temporary failure in name resolution",
		"client_loop: send disconnect: Broken pipe",
	} {
		if r := fatalSSH([]byte(transient)); r != "" {
			t.Errorf("%q treated as fatal (%s): the link dropping must be retried", transient, r)
		}
	}
}

func TestParseTmuxList(t *testing.T) {
	got := parseTmuxList("c-claude|1|1794272350|node\nwork|0|1794270000|bash\n\ngarbage\n")
	if len(got) != 2 || got[0].Name != "c-claude" || got[0].Attached != 1 || got[1].Command != "bash" {
		t.Fatalf("unexpected parse: %+v", got)
	}
}

func TestDialAddr(t *testing.T) {
	if a := (Host{Target: "dev@100.64.0.5"}).dialAddr(); a != "100.64.0.5:22" {
		t.Error(a)
	}
	if a := (Host{Target: "u@h", Args: "-p 2222"}).dialAddr(); a != "h:2222" {
		t.Error(a)
	}
}

func TestPlanOpen(t *testing.T) {
	c := defaultConfig()
	c.Hosts = append(c.Hosts, Host{Name: "server", Target: "dev@server", Dir: "/srv/app"})
	_, _, label, sp, err := planOpen(c, inMsg{Host: 1, Agent: 1})
	if err != nil || sp.Tmux != "c-claude" || sp.Cmd != "claude" || sp.Dir != "/srv/app" || label != "server · c-claude" {
		t.Fatalf("agent open: %+v %q %v", sp, label, err)
	}
	_, _, _, sp, _ = planOpen(c, inMsg{Host: 1, Agent: 1, Name: "api refactor", Dir: "/x"})
	if sp.Tmux != "api-refactor" || sp.Dir != "/x" {
		t.Fatalf("named open: %+v", sp)
	}
	_, _, _, sp, _ = planOpen(c, inMsg{Host: 1, Attach: true, Name: "my work"})
	if !sp.Attach || sp.Tmux != "my work" || sp.Cmd != "" {
		t.Fatalf("attach: %+v", sp)
	}
	if _, _, _, _, err := planOpen(c, inMsg{Host: 9}); err == nil {
		t.Error("bad host index accepted")
	}
	if _, _, _, _, err := planOpen(c, inMsg{Host: 1, Agent: -1, Cmd: " "}); err == nil {
		t.Error("empty custom command accepted")
	}
}

// isolatedTmux points tmux at a private server so tests never touch the
// developer's own sessions.
func isolatedTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	t.Cleanup(func() { exec.Command("tmux", "kill-server").Run() })
}

// The create script must type the agent command into a fresh session exactly
// once, and a second run must attach without typing it again.
func TestScriptCreatesOnceThenAttaches(t *testing.T) {
	isolatedTmux(t)
	marker := filepath.Join(t.TempDir(), "ran")
	sp := Spec{Tmux: "c-test", Cmd: "echo x >> " + marker, Dir: "/tmp", Mouse: true}
	create := strings.TrimSuffix(sp.script(), "exec tmux attach-session -t '=c-test'")
	if create == sp.script() {
		t.Fatalf("script shape changed: %s", sp.script())
	}
	for i := 0; i < 2; i++ {
		if out, err := exec.Command("sh", "-c", create).CombinedOutput(); err != nil {
			t.Fatalf("run %d: %v\n%s", i, err, out)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(marker)
		if strings.Count(string(b), "x") == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent command ran %d times, want exactly 1", strings.Count(string(b), "x"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	list, err := listRemote(context.Background(), Host{Target: "local"})
	if err != nil || len(list) != 1 || list[0].Name != "c-test" {
		t.Fatalf("listRemote: %+v %v", list, err)
	}
	out, _ := exec.Command("tmux", "show-options", "-t", "=c-test:", "mouse").Output()
	if !strings.Contains(string(out), "on") {
		t.Errorf("mouse not enabled on the session: %q", out)
	}
	out, _ = exec.Command("tmux", "show-options", "-t", "=c-test:", "set-titles").Output()
	if !strings.Contains(string(out), "on") {
		t.Errorf("pane titles are not forwarded: %q", out)
	}
	if err := killRemote(context.Background(), Host{Target: "local"}, "c-test"); err != nil {
		t.Fatal(err)
	}
	if list, _ := listRemote(context.Background(), Host{Target: "local"}); len(list) != 0 {
		t.Fatalf("session survived kill: %+v", list)
	}
}

// ---- test sink ----

type testSink struct {
	data   chan []byte
	states chan string
}

func newTestSink() *testSink {
	return &testSink{data: make(chan []byte, 4096), states: make(chan string, 64)}
}
func (k *testSink) pushData(id string, d []byte) { k.data <- append([]byte(nil), d...) }
func (k *testSink) pushState(s *Session)         { k.states <- s.Info().State }
func (k *testSink) lag() int64                   { return 0 }
func (k *testSink) shutdown()                    {}

func (k *testSink) waitState(t *testing.T, want string, d time.Duration) {
	t.Helper()
	to := time.After(d)
	for {
		select {
		case st := <-k.states:
			if st == want {
				return
			}
		case <-to:
			t.Fatalf("never reached state %q", want)
		}
	}
}

func (k *testSink) waitOutput(t *testing.T, want string, d time.Duration) string {
	t.Helper()
	var all bytes.Buffer
	to := time.After(d)
	for {
		select {
		case b := <-k.data:
			all.Write(b)
			if strings.Contains(all.String(), want) {
				return all.String()
			}
		case <-to:
			t.Fatalf("output never contained %q; got %q", want, all.String())
		}
	}
}

// fakeSSH installs a script as the ssh binary. It counts invocations in $dir/n
// and records each command line in $dir/args.
func fakeSSH(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nD=" + dir + "\nn=$(cat $D/n 2>/dev/null || echo 0); n=$((n+1)); echo $n > $D/n\necho \"$*\" >> $D/args\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := sshBin
	sshBin = path
	t.Cleanup(func() { sshBin = old })
	return dir
}

// Waiting at the door is not being inside: a host that held the connection
// for an approval and then timed out must still get the session CREATED on the
// next attempt, and that attempt must be given time.
func TestApprovalWaitDoesNotCountAsConnected(t *testing.T) {
	dir := fakeSSH(t, `if [ $n -eq 1 ]; then echo "# Tailscale SSH requires an additional check."; sleep 4; exit 255; fi
for a in "$@"; do case "$a" in LocalCommand=*) sh -c "${a#LocalCommand=}";; esac; done
echo inside; sleep 30`)
	m := &SessionManager{sessions: map[string]*Session{}}
	s, _ := m.Open("t1", 0, Host{Name: "far", Target: "u@far"}, "claude", "x", Spec{Tmux: "c-claude", Cmd: "claude"}, 80, 24)
	defer m.Close("t1")
	k := newTestSink()
	s.Attach(k)
	k.waitOutput(t, "inside", 12*time.Second)
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "new-session") {
		t.Fatalf("second attempt must still create the session:\n%s", args)
	}
	if !strings.Contains(lines[1], "ConnectTimeout=600") {
		t.Errorf("second attempt was not given time for the approval: %s", lines[1])
	}
	if strings.Contains(lines[0], "ConnectTimeout=600") {
		t.Errorf("first attempt should use the short timeout: %s", lines[0])
	}
}

// A dropped link (ssh exit 255) must be retried, and the respawn must only
// attach: creating again could launch a second agent.
func TestReconnectAfterLinkDrop(t *testing.T) {
	dir := fakeSSH(t, `for a in "$@"; do case "$a" in LocalCommand=*) sh -c "${a#LocalCommand=}";; esac; done
if [ $n -eq 1 ]; then echo first-up; sleep 4; echo "client_loop: send disconnect: Broken pipe" >&2; exit 255; fi
echo back-again; sleep 30`)
	m := &SessionManager{sessions: map[string]*Session{}}
	s, err := m.Open("t1", 0, Host{Name: "far", Target: "u@far"}, "claude", "far · c-claude",
		Spec{Tmux: "c-claude", Cmd: "claude"}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close("t1")
	k := newTestSink()
	s.Attach(k)
	k.waitOutput(t, "first-up", 5*time.Second)
	k.waitState(t, stRetry, 8*time.Second)
	k.waitState(t, stUp, 8*time.Second)
	k.waitOutput(t, "back-again", 5*time.Second)

	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 2 {
		t.Fatalf("ssh ran %d times, want 2:\n%s", len(lines), args)
	}
	if !strings.Contains(lines[0], "new-session") {
		t.Errorf("first start should create the session: %s", lines[0])
	}
	if strings.Contains(lines[1], "new-session") || strings.Contains(lines[1], "send-keys") {
		t.Errorf("respawn must only attach, got: %s", lines[1])
	}
}

func TestAuthFailureIsNotRetried(t *testing.T) {
	dir := fakeSSH(t, `echo "u@far: Permission denied (publickey)." >&2; exit 255`)
	m := &SessionManager{sessions: map[string]*Session{}}
	s, _ := m.Open("t1", 0, Host{Name: "far", Target: "u@far"}, "shell", "x", Spec{Tmux: "c-shell"}, 80, 24)
	defer m.Close("t1")
	k := newTestSink()
	s.Attach(k)
	k.waitState(t, stDead, 5*time.Second)
	time.Sleep(1500 * time.Millisecond)
	n, _ := os.ReadFile(filepath.Join(dir, "n"))
	if strings.TrimSpace(string(n)) != "1" {
		t.Fatalf("ssh ran %s times after an auth failure, want 1", strings.TrimSpace(string(n)))
	}
}

// A normal end of the remote command (detach, agent quit) is not a link drop.
func TestCleanExitIsNotRetried(t *testing.T) {
	dir := fakeSSH(t, `echo "[detached (from session c-shell)]"; exit 0`)
	m := &SessionManager{sessions: map[string]*Session{}}
	s, _ := m.Open("t1", 0, Host{Name: "far", Target: "u@far"}, "shell", "x", Spec{Tmux: "c-shell"}, 80, 24)
	defer m.Close("t1")
	k := newTestSink()
	s.Attach(k)
	k.waitState(t, stDead, 5*time.Second)
	time.Sleep(1200 * time.Millisecond)
	if n, _ := os.ReadFile(filepath.Join(dir, "n")); strings.TrimSpace(string(n)) != "1" {
		t.Fatalf("ssh ran %s times after a clean exit", strings.TrimSpace(string(n)))
	}
	// A dead session can be restarted on request.
	s.Retry()
	k.waitState(t, stDead, 5*time.Second)
	if n, _ := os.ReadFile(filepath.Join(dir, "n")); strings.TrimSpace(string(n)) != "2" {
		t.Fatalf("retry did not restart the session (runs=%s)", strings.TrimSpace(string(n)))
	}
}

// ---- end to end over http + websocket ----

func testServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	c := defaultConfig()
	c.Token = "s3cret"
	if err := writeConfig(path, c); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(path); err != nil {
		t.Fatal(err)
	}
	mgr = &SessionManager{sessions: map[string]*Session{}}
	srv := httptest.NewServer(newMux())
	t.Cleanup(func() {
		for _, s := range mgr.List() {
			mgr.Close(s.ID)
		}
		srv.Close()
	})
	return srv, "casca_token=s3cret"
}

func TestAuthAndOrigin(t *testing.T) {
	srv, cookie := testServer(t)
	get := func(path, ck string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if ck != "" {
			req.Header.Set("Cookie", ck)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	for _, p := range []string{"/api/config", "/api/sessions", "/api/status", "/api/remote?host=0", "/ws"} {
		if c := get(p, ""); c != 401 {
			t.Errorf("%s without cookie: %d, want 401", p, c)
		}
		if c := get(p, "casca_token=wrong"); c != 401 {
			t.Errorf("%s with wrong token: %d, want 401", p, c)
		}
	}
	if c := get("/api/config", cookie); c != 200 {
		t.Errorf("/api/config with cookie: %d", c)
	}
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	// A page on another site holding the visitor's cookie must not get a shell.
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Cookie": {cookie}, "Origin": {"http://evil.example"}})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("cross-origin websocket was not refused: err=%v resp=%v", err, resp)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/kill?host=0&name=x", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "http://evil.example")
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 403 {
		t.Errorf("cross-origin POST: %d, want 403", r.StatusCode)
	}
}

type wsTest struct {
	t    *testing.T
	c    *websocket.Conn
	out  map[string]*bytes.Buffer
	last map[string]string
}

func (w *wsTest) send(v any) {
	if err := w.c.WriteJSON(v); err != nil {
		w.t.Fatal(err)
	}
}

// pump reads frames until cond is true.
func (w *wsTest) until(d time.Duration, what string, cond func() bool) {
	w.t.Helper()
	w.c.SetReadDeadline(time.Now().Add(d))
	for !cond() {
		mt, b, err := w.c.ReadMessage()
		if err != nil {
			w.t.Fatalf("waiting for %s: %v (output so far: %q)", what, err, w.out)
		}
		if mt == websocket.BinaryMessage {
			n := int(b[0])
			id := string(b[1 : 1+n])
			if w.out[id] == nil {
				w.out[id] = &bytes.Buffer{}
			}
			w.out[id].Write(b[1+n:])
			w.send(map[string]any{"type": "ack", "n": len(b) - 1 - n})
			continue
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		if m["type"] == "state" {
			w.last[m["id"].(string)] = m["state"].(string)
		} else if m["type"] == "error" {
			w.t.Fatalf("server error: %v", m["error"])
		} else if s, _ := m["type"].(string); s == "exists" || s == "gone" {
			w.last[m["id"].(string)] = s
		}
	}
}

func dial(t *testing.T, srv *httptest.Server, cookie string) *wsTest {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Cookie": {cookie}, "Origin": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &wsTest{t: t, c: c, out: map[string]*bytes.Buffer{}, last: map[string]string{}}
}

// The whole path a browser uses: open an agent session on "this machine",
// type into it, lose the browser, come back, find the session still running.
func TestEndToEndLocalTmux(t *testing.T) {
	isolatedTmux(t)
	srv, cookie := testServer(t)
	w := dial(t, srv, cookie)
	w.send(map[string]any{"type": "open", "id": "a1", "host": 0, "agent": 0, "cols": 100, "rows": 30})
	w.until(10*time.Second, "session up", func() bool { return w.last["a1"] == "up" })
	// é and ✓ are multi-byte: they must arrive intact through binary frames.
	w.send(map[string]any{"type": "stdin", "id": "a1", "data": "echo casca-$((40+2))-é✓\r"})
	w.until(10*time.Second, "command output", func() bool {
		return w.out["a1"] != nil && strings.Contains(w.out["a1"].String(), "casca-42-é✓")
	})

	// Same tmux session from a second request must not open a mirror tab.
	w.send(map[string]any{"type": "open", "id": "a2", "host": 0, "agent": 0, "cols": 100, "rows": 30})
	w.until(5*time.Second, "duplicate refused", func() bool { return w.last["a2"] == "exists" })

	// The browser goes away. The session must survive on the server.
	w.c.Close()
	time.Sleep(300 * time.Millisecond)
	req, _ := http.NewRequest("GET", srv.URL+"/api/sessions", nil)
	req.Header.Set("Cookie", cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var infos []Info
	json.NewDecoder(r.Body).Decode(&infos)
	r.Body.Close()
	if len(infos) != 1 || infos[0].ID != "a1" || infos[0].State != "up" || infos[0].Tmux != "c-shell" {
		t.Fatalf("session did not survive the browser closing: %+v", infos)
	}

	// A new browser attaches and gets a full repaint from tmux, showing the old output.
	w2 := dial(t, srv, cookie)
	w2.send(map[string]any{"type": "resize", "id": "a1", "cols": 100, "rows": 30})
	w2.send(map[string]any{"type": "attach", "id": "a1"})
	w2.until(10*time.Second, "repaint after reattach", func() bool {
		return w2.out["a1"] != nil && strings.Contains(w2.out["a1"].String(), "casca-42-é✓")
	})

	// /api/remote sees it, closing the tab leaves tmux running.
	req, _ = http.NewRequest("GET", srv.URL+"/api/remote?host=0", nil)
	req.Header.Set("Cookie", cookie)
	r, _ = http.DefaultClient.Do(req)
	var rem struct{ Sessions []RemoteSession }
	json.NewDecoder(r.Body).Decode(&rem)
	r.Body.Close()
	if len(rem.Sessions) != 1 || rem.Sessions[0].Name != "c-shell" {
		t.Fatalf("/api/remote: %+v", rem)
	}
	w2.send(map[string]any{"type": "close", "id": "a1"})
	time.Sleep(500 * time.Millisecond)
	if len(mgr.List()) != 0 {
		t.Fatal("close did not remove the session")
	}
	if list, _ := listRemote(context.Background(), Host{Target: "local"}); len(list) != 1 {
		t.Fatalf("closing the tab killed the tmux session: %+v", list)
	}
}

func TestUploadAndHostsAPI(t *testing.T) {
	srv, cookie := testServer(t)
	do := func(method, path string, body []byte) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("Cookie", cookie)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(r.Body)
		return r.StatusCode, b.Bytes()
	}
	payload := []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 0xff}
	code, body := do("POST", "/api/upload?host=0&name=shot%20one.png", payload)
	var up struct{ Path string }
	json.Unmarshal(body, &up)
	if code != 200 || !strings.HasPrefix(up.Path, "/tmp/casca-") || !strings.HasSuffix(up.Path, ".png") {
		t.Fatalf("upload: %d %s", code, body)
	}
	defer os.Remove(up.Path)
	if got, _ := os.ReadFile(up.Path); !bytes.Equal(got, payload) {
		t.Fatalf("uploaded file differs: %v", got)
	}
	if st, _ := os.Stat(up.Path); st.Mode().Perm() != 0o600 {
		t.Errorf("upload is readable by others: %v", st.Mode())
	}

	if code, _ := do("POST", "/api/hosts", []byte(`{"target":"-oProxyCommand=evil"}`)); code != 400 {
		t.Errorf("option-looking target accepted: %d", code)
	}
	code, body = do("POST", "/api/hosts", []byte(`{"name":"server","target":"dev@100.64.0.5","group":"tailnet"}`))
	if code != 200 || len(getConfig().Hosts) != 2 || getConfig().Hosts[1].Name != "server" {
		t.Fatalf("add host: %d %s", code, body)
	}
	if strings.Contains(string(body), "s3cret") {
		t.Error("config API leaks the token")
	}
	if code, _ = do("DELETE", "/api/hosts?i=1", nil); code != 200 || len(getConfig().Hosts) != 1 {
		t.Fatalf("delete host: %d", code)
	}
}
