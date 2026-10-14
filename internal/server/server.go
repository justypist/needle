package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"needle/internal/proto"
	"needle/internal/store"
)

// maxReportBytes caps an ingest body. A full report with per-core, per-interface
// and per-mount series is a few kilobytes; anything larger is a mistake or an
// attack.
const maxReportBytes = 256 << 10

// maxRange is the widest history window the API will serve.
const maxRange = 365 * 24 * time.Hour

// Server wires the store and auth into an http.Handler.
type Server struct {
	store *store.Store
	auth  *auth
	web   fs.FS
}

// New builds the handler tree. webFS must contain a "web" directory.
func New(st *store.Store, webFS fs.FS, cfg Config) http.Handler {
	s := &Server{
		store: st,
		auth:  newAuth(cfg.AgentToken, cfg.Password, cfg.SessionSecret, cfg.SecureCookies),
		web:   webFS,
	}
	mux := http.NewServeMux()

	// Ingest: the only endpoint an agent ever talks to.
	mux.HandleFunc("POST /report", s.handleReport)

	// Dashboard API. Everything under /api needs a session.
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		s.auth.logout(w)
	})
	mux.HandleFunc("GET /api/session", s.guarded(s.handleSession))
	mux.HandleFunc("GET /api/nodes", s.guarded(s.handleNodes))
	mux.HandleFunc("GET /api/node", s.guarded(s.handleNode))
	mux.HandleFunc("GET /api/stats", s.guarded(s.handleStats))

	// Static dashboard.
	mux.Handle("GET /", http.FileServerFS(s.web))

	return recoverMW(logMW(mux))
}

func (s *Server) guarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.checkSession(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if !s.auth.checkAgent(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReportBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unreadable body"})
		return
	}
	var rep proto.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if rep.Host.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing host.id"})
		return
	}
	if rep.Interval <= 0 {
		rep.Interval = reportedInterval(r)
	}
	if err := s.store.Ingest(rep, rep.Interval); err != nil {
		slog.Error("ingest failed", "host", rep.Host.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "storage failure"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// reportedInterval takes the agent's own notion of its period when it sends
// one, so the dashboard can show an accurate cadence.
func reportedInterval(r *http.Request) int {
	if v := r.Header.Get("X-Needle-Interval"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

type nodeView struct {
	store.HostRow
	Online bool           `json:"online"`
	Sample proto.Sample   `json:"sample"`
	Series []proto.Series `json:"series"`
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.Hosts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]nodeView, 0, len(hosts))
	for _, h := range hosts {
		n := nodeView{HostRow: h, Online: isOnline(h.LastSeen)}
		// A host is listed as soon as it registers, even before its first
		// sample lands, so missing history is not an error here.
		if sample, series, ok, err := s.store.Latest(h.ID); err == nil && ok {
			n.Sample = sample
			n.Series = series
		}
		out = append(out, n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out, "now": time.Now().UnixMilli()})
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
		return
	}
	host, err := s.store.Host(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown node"})
		return
	}

	to := time.Now().UnixMilli()
	from := to - store.ParseRange(r.URL.Query().Get("range"), 6*time.Hour, maxRange).Milliseconds()
	if v := r.URL.Query().Get("to"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			to = n
		}
	}
	if v := r.URL.Query().Get("from"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			from = n
		}
	}
	if from >= to {
		from = to - time.Hour.Milliseconds()
	}

	window, err := s.store.Query(id, from, to)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown node"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	latest, series, _, err := s.store.Latest(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"host":   host,
		"online": isOnline(host.LastSeen),
		"latest": latest,
		"series": series,
		"range":  window,
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth": s.auth.enabled})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	s.auth.login(w, r, body.Password)
}

// isOnline treats a host as online for three missed reporting periods, so a
// single dropped POST does not flap the dashboard.
func isOnline(lastSeen int64) bool {
	if lastSeen == 0 {
		return false
	}
	return time.Since(time.UnixMilli(lastSeen)) < 90*time.Second
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.code,
			"dur_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic in handler", "path", r.URL.Path, "panic", v)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// Config carries the runtime settings handed in from main.
type Config struct {
	AgentToken    string
	Password      string
	SessionSecret string
	SecureCookies bool
}

// TruncateForLog shortens a credential so it can appear in a startup log
// line without leaking the whole thing.
func TruncateForLog(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "..." + strings.Repeat("*", 4)
}
