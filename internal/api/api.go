// Package api serves the daemon's HTTP API: contract §4 (health, sessions), §C (queue), the event
// endpoint the council webhook entry point forwards issue events to, and the configuration report and check.
package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/queue"
	"github.com/oh-my-omp/pm-council/internal/sessions"
)

// Event is what the webhook entry point forwards: the issue ("owner/repo#n"), the GitHub event and its action.
type Event struct {
	Issue  string `json:"issue"`
	Event  string `json:"event"`
	Action string `json:"action"`
}

// Daemon is what the API reads from and hands events to.
type Daemon interface {
	// Queue returns the latest computed queue.
	Queue() queue.Queue
	// Notify hands the issue number to the refresh worker; it returns when the worker accepted
	// it or ctx ends.
	Notify(ctx context.Context, number int) error
}

type Server struct {
	daemon      Daemon
	sessions    *sessions.Registry
	goals       config.Goals
	loaded      config.Loaded
	pmRepo      string
	sessionsDir string
	startedAt   time.Time
}

func New(d Daemon, reg *sessions.Registry, goals config.Goals, loaded config.Loaded, pmRepo, sessionsDir string, startedAt time.Time) *Server {
	return &Server{daemon: d, sessions: reg, goals: goals, loaded: loaded, pmRepo: pmRepo, sessionsDir: sessionsDir, startedAt: startedAt}
}

// Handler returns the API routes wrapped with CORS headers.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/queue", s.queue)
	mux.HandleFunc("POST /v1/events", s.event)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}/log", s.sessionLog)
	mux.HandleFunc("GET /v1/config", s.loadedConfig)
	mux.HandleFunc("POST /v1/config/check", s.checkConfig)
	mux.HandleFunc("OPTIONS /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	version := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	writeJSON(w, http.StatusOK, map[string]any{"component": "council", "version": version, "startedAt": s.startedAt})
}

func (s *Server) queue(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.daemon.Queue())
}

func (s *Server) loadedConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.loaded)
}

// checkConfig loads the files of the body in memory with the loader the daemon started with and
// reports the issues; the running configuration and the disk are untouched.
func (s *Server) checkConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body: "+err.Error())
		return
	}
	if body.Files == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", `body must be {"files": {"<path>": "<content>"}}`)
		return
	}
	issues, err := config.Check(body.Files)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if len(issues) == 0 {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "errors": issues})
}

func (s *Server) event(w http.ResponseWriter, r *http.Request) {
	var ev Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&ev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body: "+err.Error())
		return
	}
	repo, n, ok := queue.ParseRef(ev.Issue)
	if !ok || !strings.EqualFold(repo, s.pmRepo) {
		writeError(w, http.StatusBadRequest, "invalid_request", "issue must be "+s.pmRepo+"#<n>")
		return
	}
	slog.Info("event", "issue", ev.Issue, "event", ev.Event, "action", ev.Action)
	if err := s.daemon.Notify(r.Context(), n); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"issue": ev.Issue})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	list, err := s.sessions.List(s.sessionsDir, s.goals, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) sessionLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, ok, err := sessions.FindFile(s.sessionsDir, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_session", "no session "+id)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	if _, err := io.Copy(w, f); err != nil {
		slog.Warn("session log copy failed", "session", id, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
