package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	bufSize     = 128 * 1024 // raw output kept for repainting a client that attaches
	tailOnRetry = 8 * 1024   // what a late client sees of a session that is not up
	stableAfter = 20 * time.Second

	// Flow control: stop reading a PTY while a client has this much output it
	// has not rendered yet, so a slow browser slows the producer down instead
	// of piling up megabytes it will take minutes to draw.
	lagHigh  = 512 * 1024
	lagLow   = 192 * 1024
	lagGrace = 5 * time.Second // a client stuck this long is disconnected
)

// Session states.
const (
	stConnecting = "connecting"
	stUp         = "up"
	stRetry      = "retry" // link dropped, respawning ssh
	stDead       = "dead"
)

// backoff is the wait before reconnect attempt n (1-based). It stays short
// because the usual outage is a WiFi drop of a few minutes and each attempt is
// already bounded by ConnectTimeout.
func backoff(n int) time.Duration {
	steps := []time.Duration{1, 2, 4, 8, 15}
	if n < 1 {
		n = 1
	}
	if n > len(steps) {
		n = len(steps)
	}
	return steps[n-1] * time.Second
}

// sink receives a session's output and state changes. push must not block.
type sink interface {
	pushData(id string, data []byte)
	pushState(s *Session)
	lag() int64 // bytes sent and not yet acknowledged as rendered
	shutdown()
}

// Session is one terminal: a PTY running ssh (or a local shell). For tmux
// sessions the remote tmux holds the durable state and the PTY is only
// transport, so a dropped link is repaired by starting ssh again.
type Session struct {
	ID      string
	HostIdx int
	Host    Host
	Agent   string
	Label   string
	Spec    Spec
	Created time.Time

	mu      sync.Mutex
	ptmx    *os.File
	cmd     *exec.Cmd
	buf     []byte
	state   string
	attempt int
	closed  bool
	cols    uint16
	rows    uint16
	sinks   map[sink]struct{}
	wake    chan struct{} // interrupts the backoff sleep
	done    chan struct{}
	bell    bool // output rang the bell while nobody was attached
	created bool // the remote tmux session exists; respawns only attach
	patient bool // the host holds connections for a human approval
}

// Info is the JSON view of a session.
type Info struct {
	ID      string    `json:"id"`
	Host    int       `json:"host"`
	Label   string    `json:"label"`
	Agent   string    `json:"agent"`
	Tmux    string    `json:"tmux"`
	State   string    `json:"state"`
	Attempt int       `json:"attempt"`
	Bell    bool      `json:"bell"`
	Created time.Time `json:"created"`
}

func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoLocked()
}

func (s *Session) infoLocked() Info {
	return Info{ID: s.ID, Host: s.HostIdx, Label: s.Label, Agent: s.Agent, Tmux: s.Spec.Tmux,
		State: s.state, Attempt: s.attempt, Bell: s.bell, Created: s.Created}
}

// persistent sessions survive a lost link because tmux keeps them on the far side.
func (s *Session) persistent() bool { return s.Spec.Tmux != "" && !s.Host.isLocal() }

type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	order    []string
}

var mgr = &SessionManager{sessions: map[string]*Session{}}

func (m *SessionManager) Open(id string, hostIdx int, h Host, agent, label string, sp Spec, cols, rows uint16) (*Session, error) {
	if id == "" {
		return nil, errors.New("missing session id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; ok {
		return nil, fmt.Errorf("session %s exists", id)
	}
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	s := &Session{
		ID: id, HostIdx: hostIdx, Host: h, Agent: agent, Label: label, Spec: sp, Created: time.Now(),
		state: stConnecting, cols: cols, rows: rows,
		sinks: map[sink]struct{}{}, wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	m.sessions[id] = s
	m.order = append(m.order, id)
	go s.run(s.done)
	return s, nil
}

func (m *SessionManager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Close ends the local side. A remote tmux session keeps running.
func (m *SessionManager) Close(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	if s != nil {
		s.close()
	}
}

func (m *SessionManager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.sessions[id])
	}
	return out
}

// run owns the process lifecycle: start, pump output, and decide what an exit means.
func (s *Session) run(done chan struct{}) {
	defer close(done)
	failures := 0
	for {
		started := time.Now()
		code, head, err := s.runOnce()
		if s.isClosed() {
			return
		}
		lasted := time.Since(started)
		if err != nil {
			s.note(fmt.Sprintf("não consegui iniciar: %v", err))
			s.setState(stDead, 0)
			return
		}
		// ssh exits 255 for its own failures (no route, timeout, auth, dropped
		// link); any other status is the remote command ending on purpose.
		if !s.persistent() || code != 255 {
			if code != 0 {
				s.note(fmt.Sprintf("sessão encerrada (status %d)", code))
			} else if s.Spec.Tmux != "" {
				s.note("desconectado — a sessão tmux continua no host")
			} else {
				s.note("sessão encerrada")
			}
			s.setState(stDead, 0)
			return
		}
		if lasted >= stableAfter {
			failures = 0
		}
		// Retrying a rejected login only hammers the host (and trips fail2ban).
		if reason := fatalSSH(head); reason != "" && lasted < stableAfter {
			s.note("ssh recusou: " + reason + " — não vou insistir")
			s.setState(stDead, 0)
			return
		}
		// A transient failure is retried for as long as the tab is open: the
		// usual outage here outlasts any sensible attempt limit.
		failures++
		wait := backoff(failures)
		s.note(fmt.Sprintf("conexão caiu — reconectando em %s (tentativa %d)", wait, failures))
		s.setState(stRetry, failures)
		select {
		case <-time.After(wait):
		case <-s.wake:
		}
		if s.isClosed() {
			return
		}
	}
}

// fatalSSH spots ssh failures that another attempt cannot fix.
func fatalSSH(out []byte) string {
	for _, m := range []string{"Permission denied", "Host key verification failed",
		"Too many authentication failures", "REMOTE HOST IDENTIFICATION HAS CHANGED"} {
		if bytes.Contains(out, []byte(m)) {
			return m
		}
	}
	return ""
}

// runOnce starts the process and blocks until it exits. head is the first
// output of the attempt, used to classify a failure.
func (s *Session) runOnce() (code int, head []byte, err error) {
	s.mu.Lock()
	sp := s.Spec
	if s.created {
		// Only the first start may create the session. If the agent's session
		// ended while the link was down, a respawn must report that, not
		// quietly launch a second agent.
		sp.Attach = true
	}
	patient := s.patient
	s.mu.Unlock()
	flag := connFlag()
	defer os.Remove(flag)
	cmd := command(s.Host, sp, flag, patient)
	cmd.Env = append(cleanEnv(), "TERM=xterm-256color", "COLORTERM=truecolor")
	s.mu.Lock()
	cols, rows := s.cols, s.rows
	s.mu.Unlock()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return 0, nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return 0, nil, nil
	}
	s.ptmx, s.cmd = ptmx, cmd
	s.mu.Unlock()

	up, made := false, false
	b := make([]byte, 32*1024)
	for {
		s.throttle()
		n, err := ptmx.Read(b)
		if n > 0 {
			if !up {
				up = true
				s.setState(stUp, 0)
			}
			if !made && connectedFor(flag) > 3*time.Second {
				made = true
				s.mu.Lock()
				s.created = true
				s.mu.Unlock()
			}
			if len(head) < 2048 {
				head = append(head, b[:n]...)
			}
			s.emit(b[:n])
		}
		if err != nil {
			break
		}
	}
	werr := cmd.Wait()
	s.mu.Lock()
	ptmx.Close()
	s.ptmx, s.cmd = nil, nil
	if connectedFor(flag) > 3*time.Second {
		s.created = true
	}
	// A host that is waiting for a person gets time on the next attempt, so
	// the approval link stays valid instead of rotating every few seconds.
	s.patient = connectedFor(flag) < 0 && needsApproval(head)
	s.mu.Unlock()

	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 255, head, nil // ssh killed under us: treat like a dropped link
		}
		return ee.ExitCode(), head, nil
	}
	return 0, head, nil
}

// throttle blocks the PTY reader while an attached client is too far behind.
func (s *Session) throttle() {
	var since time.Time
	for {
		s.mu.Lock()
		var worst sink
		var max int64
		for k := range s.sinks {
			if l := k.lag(); l > max {
				worst, max = k, l
			}
		}
		closed := s.closed
		s.mu.Unlock()
		limit := int64(lagHigh)
		if !since.IsZero() {
			limit = lagLow
		}
		if closed || max <= limit {
			return
		}
		if since.IsZero() {
			since = time.Now()
		} else if time.Since(since) > lagGrace {
			worst.shutdown() // it reconnects and repaints from scratch
			since = time.Time{}
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func (s *Session) emit(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > bufSize {
		s.buf = append([]byte(nil), s.buf[len(s.buf)-bufSize:]...)
	}
	if len(s.sinks) == 0 {
		for _, c := range p {
			if c == 0x07 {
				s.bell = true
				break
			}
		}
		return
	}
	for k := range s.sinks {
		k.pushData(s.ID, p)
	}
}

// note writes a casca status line into the terminal stream.
func (s *Session) note(msg string) {
	s.emit([]byte("\r\n\x1b[0;33m[casca] " + msg + "\x1b[0m\r\n"))
}

func (s *Session) setState(st string, attempt int) {
	s.mu.Lock()
	s.state, s.attempt = st, attempt
	sinks := make([]sink, 0, len(s.sinks))
	for k := range s.sinks {
		sinks = append(sinks, k)
	}
	s.mu.Unlock()
	for _, k := range sinks {
		k.pushState(s)
	}
}

func (s *Session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.ptmx != nil {
		s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Signal(syscall.SIGHUP)
	}
	done := s.done
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		s.mu.Lock()
		if s.cmd != nil && s.cmd.Process != nil {
			s.cmd.Process.Kill()
		}
		s.mu.Unlock()
		log.Printf("session %s: process needed SIGKILL", s.ID)
	}
}

// Retry skips the backoff wait, or restarts a dead session with the same spec.
func (s *Session) Retry() {
	s.mu.Lock()
	switch {
	case s.closed:
	case s.state == stRetry:
		select {
		case s.wake <- struct{}{}:
		default:
		}
	case s.state == stDead:
		s.state, s.attempt = stConnecting, 0
		s.done = make(chan struct{})
		go s.run(s.done)
	}
	s.mu.Unlock()
	if st := s.Info(); st.State == stConnecting {
		s.setState(stConnecting, 0)
	}
}

func (s *Session) Write(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmx != nil {
		s.ptmx.Write(data)
	}
}

func (s *Session) Resize(cols, rows uint16) {
	if cols == 0 || rows == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cols, s.rows = cols, rows
	if s.ptmx != nil {
		pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	}
}

// Attach registers a client and returns what it should paint first.
//
// A tmux session that is up is not replayed: raw bytes cut at an arbitrary
// point would start mid escape sequence. Changing the window size makes tmux
// redraw the whole screen instead, which is exact.
func (s *Session) Attach(k sink) (replay []byte, redraw bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks[k] = struct{}{}
	s.bell = false
	switch {
	case s.Spec.Tmux != "" && s.state == stUp:
		return nil, true
	case s.Spec.Tmux != "":
		return tail(s.buf, tailOnRetry), false
	default:
		return append([]byte(nil), s.buf...), false
	}
}

// Redraw forces a full repaint by shrinking the window one row and restoring it.
func (s *Session) Redraw() {
	s.mu.Lock()
	cols, rows, ptmx := s.cols, s.rows, s.ptmx
	s.mu.Unlock()
	if ptmx == nil || rows < 3 {
		return
	}
	pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows - 1})
	time.AfterFunc(120*time.Millisecond, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ptmx != nil {
			pty.Setsize(s.ptmx, &pty.Winsize{Cols: s.cols, Rows: s.rows})
		}
	})
}

func (s *Session) Detach(k sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sinks, k)
}

// tail returns the last n bytes of b starting at a line boundary.
func tail(b []byte, n int) []byte {
	if len(b) > n {
		b = b[len(b)-n:]
		for i, c := range b {
			if c == '\n' {
				b = b[i+1:]
				break
			}
		}
	}
	return append([]byte(nil), b...)
}
