// Package api serves the REST API, the live WebSocket feed and the embedded UI.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"labdeck/internal/adapter"
	"labdeck/internal/config"
	"labdeck/internal/engine"
	"labdeck/internal/store"
	"labdeck/internal/sshgw"
	"labdeck/web"
)

type Server struct {
	cfg    *config.Config
	eng    *engine.Engine
	st     *store.Store
	gw     *sshgw.Gateway
	col    *adapter.Collector
	mux    *http.ServeMux
	notify chan struct{} // poked on every transition; coalesced by the broadcaster

	wsMu    sync.Mutex
	wsConns map[*websocket.Conn]bool
}

func New(cfg *config.Config, eng *engine.Engine, st *store.Store, gw *sshgw.Gateway, col *adapter.Collector) *Server {
	s := &Server{
		cfg:     cfg,
		eng:     eng,
		st:      st,
		gw:      gw,
		col:     col,
		mux:     http.NewServeMux(),
		notify:  make(chan struct{}, 1),
		wsConns: map[*websocket.Conn]bool{},
	}
	s.mux.HandleFunc("GET /api/summary", s.handleSummary)
	s.mux.HandleFunc("GET /api/services/{id}/uptime", s.handleUptime)
	s.mux.HandleFunc("GET /api/services/{id}/history", s.handleHistory)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/ws", s.handleWS)

	s.mux.HandleFunc("GET /api/hosts", s.handleHosts)
	s.mux.HandleFunc("GET /api/credentials", s.handleListCredentials)
	s.mux.HandleFunc("POST /api/credentials", s.handleSaveCredential)
	s.mux.HandleFunc("DELETE /api/credentials/{id}", s.handleDeleteCredential)
	s.mux.HandleFunc("GET /api/ssh/sessions", s.handleSSHSessions)
	s.mux.HandleFunc("GET /api/ssh/{id}/ws", gw.HandleWS)
	s.mux.HandleFunc("GET /api/top", s.handleTop)

	staticFS, _ := fs.Sub(web.Static, "static")
	s.mux.Handle("/", http.FileServerFS(staticFS))
	return s
}

// Poke signals that state changed; the broadcaster pushes a fresh summary.
func (s *Server) Poke() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	if s.cfg.Auth.Password != "" {
		h = basicAuth(s.cfg.Auth.Password, h)
	}
	return h
}

func basicAuth(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="labdeck"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type summaryPayload struct {
	GeneratedAt time.Time            `json:"generated_at"`
	Services    []engine.ServiceView `json:"services"`
}

func (s *Server) summary() summaryPayload {
	return summaryPayload{GeneratedAt: time.Now(), Services: s.eng.Snapshot()}
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.summary())
}

func (s *Server) handleTop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"generated_at": time.Now(),
		"integrations": s.col.Snapshot(),
	})
}

func (s *Server) handleUptime(w http.ResponseWriter, r *http.Request) {
	days := intParam(r, "days", 30)
	buckets, err := s.st.DailyUptime(r.PathValue("id"), days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, buckets)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	hours := intParam(r, "hours", 24)
	limit := intParam(r, "limit", 500)
	points, err := s.st.RecentHistory(r.PathValue("id"), hours, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, points)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100)
	events, err := s.st.RecentEvents(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, events)
}

var upgrader = websocket.Upgrader{
	// The UI is same-origin; a reverse proxy terminates external access.
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.wsMu.Lock()
	s.wsConns[conn] = true
	s.wsMu.Unlock()

	_ = conn.WriteJSON(map[string]any{"type": "summary", "data": s.summary()})

	// Drain reads to detect close; the broadcaster is the only writer.
	go func() {
		defer s.dropConn(conn)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (s *Server) dropConn(conn *websocket.Conn) {
	s.wsMu.Lock()
	delete(s.wsConns, conn)
	s.wsMu.Unlock()
	conn.Close()
}

// Broadcast pushes summaries to all WebSocket clients on state changes
// (coalesced) and as a 30s keepalive snapshot. Blocks until done is closed.
func (s *Server) Broadcast(done <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-s.notify:
		case <-ticker.C:
		}
		payload := map[string]any{"type": "summary", "data": s.summary()}
		s.wsMu.Lock()
		conns := make([]*websocket.Conn, 0, len(s.wsConns))
		for c := range s.wsConns {
			conns = append(conns, c)
		}
		s.wsMu.Unlock()
		for _, c := range conns {
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.WriteJSON(payload); err != nil {
				s.dropConn(c)
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
