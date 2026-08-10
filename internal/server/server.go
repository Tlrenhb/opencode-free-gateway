// Package server wires the HTTP surface: /v1 passthrough with call-key auth,
// /health, and the admin API + embedded management UI.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Tlrenhb/ocfreelay-go/internal/adminui"
	"github.com/Tlrenhb/ocfreelay-go/internal/auth"
	"github.com/Tlrenhb/ocfreelay-go/internal/config"
	"github.com/Tlrenhb/ocfreelay-go/internal/pool"
	"github.com/Tlrenhb/ocfreelay-go/internal/relayproxy"
	"github.com/Tlrenhb/ocfreelay-go/internal/rotator"
	"github.com/Tlrenhb/ocfreelay-go/internal/stats"
)

// Server is the assembled HTTP application.
type Server struct {
	cfg     *config.Settings
	store   *config.Store
	rot     *rotator.Rotator
	relay   *relayproxy.Client
	pool    *pool.Manager
	auth    *auth.Manager
	stats   *stats.Store
	logger  *slog.Logger
	started time.Time
	mux     *http.ServeMux
}

// New assembles the server with all dependencies.
func New(
	cfg *config.Settings,
	store *config.Store,
	rot *rotator.Rotator,
	relay *relayproxy.Client,
	pm *pool.Manager,
	am *auth.Manager,
	st *stats.Store,
	logger *slog.Logger,
) *Server {
	return &Server{
		cfg:     cfg,
		store:   store,
		rot:     rot,
		relay:   relay,
		pool:    pm,
		auth:    am,
		stats:   st,
		logger:  logger,
		started: time.Now(),
	}
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/", s.handleV1)
	mux.HandleFunc("/v1", s.handleV1)
	mux.HandleFunc("/admin/api/login", s.handleAdminLogin)
	mux.HandleFunc("/admin/api/setup", s.handleSetupStatus)
	mux.HandleFunc("/admin/api/logout", s.handleLogout)
	mux.HandleFunc("/admin/api/", s.handleAdminAPI)
	mux.HandleFunc("/admin/", s.handleAdminPage)
	mux.Handle("/admin/static/", http.StripPrefix("/admin/static/", adminui.Handler()))
	mux.HandleFunc("/", s.handleRoot)
	return logMiddleware(s.logger, mux)
}

// ---------------------------------------------------------------------------
// admin page + auth
// ---------------------------------------------------------------------------

// handleAdminPage serves the SPA shell (login/setup is handled client-side by
// checking /admin/api/status; the shell is always served).
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/" && r.URL.Path != "/admin" {
		http.NotFound(w, r)
		return
	}
	adminui.ServeShell(w, r)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "ocfr_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetupStatus tells the UI whether the initial admin password is set.
// This is deliberately unauthenticated (no data is leaked, just a boolean).
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"setupRequired": !s.auth.AdminPasswordSet()})
}

// ---------------------------------------------------------------------------
// /health
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "ocfreelay-go",
		"uptime":  time.Since(s.started).String(),
	})
}

// ---------------------------------------------------------------------------
// /v1/* transparent passthrough
// ---------------------------------------------------------------------------

func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	// Optional call-key gate.
	if s.cfg.RequireCallKeyAuth {
		bearer := auth.BearerFromHeader(r.Header.Get("Authorization"))
		if !s.auth.CallKeyOK(bearer) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"message": "invalid or missing call key"},
			})
			return
		}
	}

	// Buffer the request body so the gateway can apply the minimal
	// client_metadata fix before forwarding.
	rawBody, err := relayproxy.ReadBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "request body too large"},
		})
		return
	}

	res, err := s.relay.Forward(r.Context(), r.Method, r.URL.Path, r.URL.Query(), r.Header, rawBody)
	if err != nil {
		s.logger.Error("forward failed", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "upstream unavailable: " + err.Error()},
		})
		return
	}

	kind := "chat"
	if strings.HasSuffix(r.URL.Path, "/models") {
		kind = "models"
	}
	s.stats.RecordRequest(res.WorkerID, kind, res.Status)

	// Best-effort token accounting from non-stream JSON responses.
	if res.Status >= 200 && res.Status < 300 && !isStreaming(res.Header) && strings.Contains(
		strings.ToLower(res.Header.Get("Content-Type")), "application/json") {
		if usage, err := extractUsage(res.Body); err == nil && usage != nil {
			s.stats.AddTokens(res.WorkerID, *usage)
		} else {
			_ = res.Body.Close()
		}
	}

	relayproxy.CopyResponse(w, res)
}

func isStreaming(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// extractUsage drains a JSON body to pull the usage object, then restores the
// body for downstream. Callers must close the returned body.
func extractUsage(body io.ReadCloser) (*stats.TokenUsage, error) {
	data, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			CacheReadTokens  int64 `json:"prompt_cache_hit_tokens"`
			CacheWriteTokens int64 `json:"prompt_cache_miss_tokens"`
			PromptCacheHit   int64 `json:"cache_read_input_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.Usage == nil {
		return nil, fmt.Errorf("no usage")
	}
	u := doc.Usage
	return &stats.TokenUsage{
		PromptTokens:     u.PromptTokens + u.InputTokens,
		CompletionTokens: u.CompletionTokens + u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CacheReadTokens:  u.CacheReadTokens + u.PromptCacheHit,
		CacheWriteTokens: u.CacheWriteTokens,
	}, nil
}

// ---------------------------------------------------------------------------
// admin
// ---------------------------------------------------------------------------

// adminAuthorized checks the session token from cookie or header.
func (s *Server) adminAuthorized(r *http.Request) bool {
	// Accept the session token via Cookie or X-Admin-Token header.
	if tok := r.Header.Get("X-Admin-Token"); tok != "" {
		return s.auth.ValidSession(tok)
	}
	if c, err := r.Cookie("ocfr_session"); err == nil {
		return s.auth.ValidSession(c.Value)
	}
	return false
}

// handleRoot redirects / to /admin/.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func logMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).Round(time.Millisecond))
	})
}
