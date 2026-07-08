// Package sshgw bridges browser WebSocket terminals to SSH targets.
//
// Protocol: server→client binary frames carry raw terminal output; text frames
// carry JSON control messages {type: connected|error|exit, msg}. Client→server
// text frames carry {type: input, data} and {type: resize, cols, rows}.
package sshgw

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"labdeck/internal/config"
	"labdeck/internal/creds"
	"labdeck/internal/store"
)

type Gateway struct {
	st        *store.Store
	masterKey []byte
	hosts     map[string]*config.Host
	idle      time.Duration
	sem       chan struct{}
}

func New(cfg *config.Config, st *store.Store, masterKey []byte) *Gateway {
	g := &Gateway{
		st:        st,
		masterKey: masterKey,
		hosts:     map[string]*config.Host{},
		idle:      cfg.SSH.IdleTimeout.Std(),
		sem:       make(chan struct{}, cfg.SSH.MaxSessions),
	}
	for i := range cfg.Hosts {
		g.hosts[cfg.Hosts[i].ID] = &cfg.Hosts[i]
	}
	return g
}

// Enabled reports whether the master key was provided at startup.
func (g *Gateway) Enabled() bool { return len(g.masterKey) == 32 }

// Seal encrypts a credential secret under the gateway's master key.
func (g *Gateway) Seal(credID string, secret []byte) (nonce, sealed []byte, err error) {
	if !g.Enabled() {
		return nil, nil, fmt.Errorf("master key not set")
	}
	return creds.Seal(g.masterKey, credID, secret)
}

// HasSSH reports whether hostID exists and has an ssh block configured.
func (g *Gateway) HasSSH(hostID string) bool {
	h, ok := g.hosts[hostID]
	return ok && h.SSH != nil
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // same-origin UI behind auth/reverse proxy
}

func (g *Gateway) HandleWS(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	host, ok := g.hosts[hostID]
	if !ok || host.SSH == nil {
		http.Error(w, "host has no ssh configuration", http.StatusNotFound)
		return
	}
	if !g.Enabled() {
		http.Error(w, "ssh gateway disabled: LABDECK_MASTER_KEY not set", http.StatusServiceUnavailable)
		return
	}
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	default:
		http.Error(w, "too many concurrent ssh sessions", http.StatusTooManyRequests)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ws := &wsWriter{conn: conn}

	if err := g.run(r, ws, hostID, host); err != nil {
		ws.control("error", err.Error())
	}
}

// run performs the SSH dial and pumps data until either side closes.
func (g *Gateway) run(r *http.Request, ws *wsWriter, hostID string, host *config.Host) error {
	cred, err := g.st.GetCredential(host.SSH.Credential)
	if err != nil {
		return fmt.Errorf("credential %q not found — add it via POST /api/credentials", host.SSH.Credential)
	}
	secret, err := creds.Open(g.masterKey, cred.ID, cred.Nonce, cred.Secret)
	if err != nil {
		return err
	}

	var auth ssh.AuthMethod
	switch cred.Type {
	case "password":
		auth = ssh.Password(string(secret))
	case "key":
		signer, err := ssh.ParsePrivateKey(secret)
		if err != nil {
			return fmt.Errorf("parse private key: %w", err)
		}
		auth = ssh.PublicKeys(signer)
	default:
		return fmt.Errorf("unknown credential type %q", cred.Type)
	}

	user := host.SSH.User
	if user == "" {
		user = cred.Username
	}
	if user == "" {
		return fmt.Errorf("no ssh user: set host ssh.user or the credential's username")
	}

	target := net.JoinHostPort(host.Address, fmt.Sprint(host.SSH.Port))
	client, err := ssh.Dial("tcp", target, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: g.tofuCallback(hostID),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("connect %s: %w", target, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	var bytesIn, bytesOut atomic.Int64
	sess.Stdout = &countingWS{ws: ws, n: &bytesOut}
	sess.Stderr = &countingWS{ws: ws, n: &bytesOut}

	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		return err
	}
	if err := sess.Shell(); err != nil {
		return err
	}

	auditID, err := g.st.StartSSHSession(hostID, target, user, r.RemoteAddr)
	if err != nil {
		slog.Error("audit ssh session", "err", err)
	}
	slog.Info("ssh session opened", "host", hostID, "target", target, "user", user, "from", r.RemoteAddr)
	ws.control("connected", fmt.Sprintf("%s@%s", user, target))

	closeReason := "remote closed"
	defer func() {
		if auditID != 0 {
			_ = g.st.EndSSHSession(auditID, bytesIn.Load(), bytesOut.Load(), closeReason)
		}
		slog.Info("ssh session closed", "host", hostID, "reason", closeReason)
	}()

	// Idle timer: any keystroke rearms it; firing tears the session down.
	idleTimer := time.AfterFunc(g.idle, func() {
		closeReason = "idle timeout"
		ws.control("error", "会话空闲超时，已断开")
		ws.conn.Close()
	})
	defer idleTimer.Stop()

	// Reader: browser input / resize → SSH.
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := ws.conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			var msg struct {
				Type string `json:"type"`
				Data string `json:"data"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "input":
				idleTimer.Reset(g.idle)
				bytesIn.Add(int64(len(msg.Data)))
				if _, err := stdin.Write([]byte(msg.Data)); err != nil {
					readErr <- err
					return
				}
			case "resize":
				if msg.Cols > 0 && msg.Rows > 0 {
					_ = sess.WindowChange(msg.Rows, msg.Cols)
				}
			}
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()

	select {
	case <-readErr:
		closeReason = "client closed"
	case err := <-waitErr:
		if err != nil {
			closeReason = "shell exit: " + err.Error()
		} else {
			closeReason = "shell exit"
		}
		ws.control("exit", "连接已结束")
	}
	return nil
}

// tofuCallback pins the host key on first connect and rejects changes after.
func (g *Gateway) tofuCallback(hostID string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		pinned, err := g.st.HostKey(hostID)
		if err != nil {
			return err
		}
		if pinned == "" {
			slog.Info("pinning ssh host key on first use", "host", hostID, "fingerprint", fp)
			return g.st.PinHostKey(hostID, fp)
		}
		if pinned != fp {
			return fmt.Errorf("host key mismatch for %s: pinned %s, got %s — possible MITM; if the host was reinstalled, clear the pin in host_keys", hostID, pinned, fp)
		}
		return nil
	}
}

// wsWriter serializes writes: two output streams plus control frames share one socket.
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) writeBinary(p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.conn.WriteMessage(websocket.BinaryMessage, p)
}

func (w *wsWriter) control(typ, msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = w.conn.WriteJSON(map[string]string{"type": typ, "msg": msg})
}

type countingWS struct {
	ws *wsWriter
	n  *atomic.Int64
}

func (c *countingWS) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	if err := c.ws.writeBinary(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
