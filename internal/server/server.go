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

	"github.com/Tlrenhb/opencode-free-gateway/internal/adminui"
	"github.com/Tlrenhb/opencode-free-gateway/internal/auth"
	"github.com/Tlrenhb/opencode-free-gateway/internal/config"
	"github.com/Tlrenhb/opencode-free-gateway/internal/pool"
	"github.com/Tlrenhb/opencode-free-gateway/internal/relayproxy"
	"github.com/Tlrenhb/opencode-free-gateway/internal/rotator"
	"github.com/Tlrenhb/opencode-free-gateway/internal/stats"
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
	// Revoke the server-side session so the token dies immediately.
	if tok := r.Header.Get("X-Admin-Token"); tok != "" {
		s.auth.RevokeSession(tok)
	} else if c, err := r.Cookie("ocfr_session"); err == nil {
		s.auth.RevokeSession(c.Value)
	}
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
		"service": "opencode-free-gateway",
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

	// Token accounting: SSE responses are observed via a side-channel hook
	// on the stream; non-stream JSON bodies are buffered so usage extraction
	// doesn't deplete the body that must still be written to the client.
	if res.Status >= 200 && res.Status < 300 && isStreaming(res.Header) {
		parser := &sseUsageParser{}
		relayproxy.CopyResponseWithHook(w, res, parser.write)
		if usage := parser.usage(); usage != nil {
			s.stats.AddTokens(res.WorkerID, *usage)
		}
		return
	}

	// Non-streaming: buffer the body so we can extract usage without
	// depleting the stream that CopyResponse needs.
	if res.Status >= 200 && res.Status < 300 {
		bodyBytes, rerr := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if rerr == nil {
			if strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "application/json") {
				if usage, uerr := parseUsage(bodyBytes); uerr == nil && usage != nil {
					s.stats.AddTokens(res.WorkerID, *usage)
				}
			}
			// Echo headers + buffered body to client.
			// CopyResponse would re-drain the body, so we write directly.
			h := w.Header()
			for k, vv := range res.Header {
				if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Keep-Alive") ||
					strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Upgrade") ||
					strings.EqualFold(k, "Content-Length") {
					continue
				}
				for _, v := range vv {
					h.Add(k, v)
				}
			}
			w.WriteHeader(res.Status)
			_, _ = w.Write(bodyBytes)
			return
		}
	}

	relayproxy.CopyResponse(w, res)
}

func isStreaming(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// sseUsageParser collects SSE payload bytes and extracts the last
// `data: {...usage...}` block for token accounting. The stream itself is
// untouched — this is a side-channel observer.
type sseUsageParser struct {
	buf []byte
}

func (p *sseUsageParser) write(b []byte) {
	// Keep a bounded tail window; usage blocks sit at the end of the stream.
	p.buf = append(p.buf, b...)
	if len(p.buf) > 1<<20 {
		p.buf = p.buf[len(p.buf)-(1<<20):]
	}
}

// usage extracts the last usage object from the captured SSE tail.
func (p *sseUsageParser) usage() *stats.TokenUsage {
	lines := strings.Split(string(p.buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj struct {
			Usage *jsonUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &obj); err == nil && obj.Usage != nil {
			return obj.Usage.toTokenUsage()
		}
	}
	return nil
}

type jsonUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CacheReadTokens  int64 `json:"prompt_cache_hit_tokens"`
	CacheWriteTokens int64 `json:"prompt_cache_miss_tokens"`
	PromptCacheHit   int64 `json:"cache_read_input_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

func (u *jsonUsage) toTokenUsage() *stats.TokenUsage {
	return &stats.TokenUsage{
		PromptTokens:     u.PromptTokens + u.InputTokens,
		CompletionTokens: u.CompletionTokens + u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CacheReadTokens:  u.CacheReadTokens + u.PromptCacheHit,
		CacheWriteTokens: u.CacheWriteTokens,
	}
}

// parseUsage extracts the token usage from a buffered JSON response body.
func parseUsage(body []byte) (*stats.TokenUsage, error) {
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
	if err := json.Unmarshal(body, &doc); err != nil || doc.Usage == nil {
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
