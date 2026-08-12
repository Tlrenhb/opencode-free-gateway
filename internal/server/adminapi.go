package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tlrenhb/opencode-free-gateway/internal/auth"
	"github.com/Tlrenhb/opencode-free-gateway/internal/config"
	"github.com/Tlrenhb/opencode-free-gateway/internal/pool"
	"github.com/Tlrenhb/opencode-free-gateway/internal/rotator"
	"github.com/Tlrenhb/opencode-free-gateway/internal/stats"
)

// handleAdminLogin authenticates an admin and returns a session token.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !readJSONBody(w, r, &body) {
		return
	}
	tok := s.auth.Login(body.Password)
	if tok == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "wrong password"}})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "ocfr_session", Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: time.Now().Add(auth.SessionTTL),
	})
	writeJSON(w, http.StatusOK, map[string]any{"token": tok})
}

// handleAdminAPI routes all /admin/api/* requests. Every handler here
// requires a valid admin session, except the standalone login endpoint
// (registered separately in server.go).
func (s *Server) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// Setup mode: no admin password configured yet — allow ONLY the settings
	// endpoint so the operator can set the initial password. Everything else
	// stays locked.
	if !s.auth.AdminPasswordSet() && r.URL.Path == "/admin/api/settings" {
		s.apiSettings(w, r)
		return
	}
	if !s.adminAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": "admin auth required"},
		})
		return
	}
	switch {
	case r.URL.Path == "/admin/api/status":
		s.apiStatus(w, r)
	case r.URL.Path == "/admin/api/settings":
		s.apiSettings(w, r)
	case r.URL.Path == "/admin/api/pool":
		s.apiPool(w, r)
	case r.URL.Path == "/admin/api/pool/import":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "POST only"}})
			return
		}
		s.apiPoolImport(w, r)
	case r.URL.Path == "/admin/api/pool/probe":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "POST only"}})
			return
		}
		s.apiPoolProbe(w, r)
	case r.URL.Path == "/admin/api/pool/prune":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "POST only"}})
			return
		}
		s.apiPoolPrune(w, r)
	case r.URL.Path == "/admin/api/workers":
		s.apiWorkers(w, r)
	case r.URL.Path == "/admin/api/callkeys":
		s.apiCallKeys(w, r)
	case r.URL.Path == "/admin/api/stats/reset":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "POST only"}})
			return
		}
		s.stats.ResetAll()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

// apiStatus exposes runtime + worker + pool + stats state for the UI.
func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	workers := s.rot.Snapshots()
	// Stats cover live workers PLUS every account with recorded history —
	// deleting a worker keeps its usage visible (deletion ≠ reset).
	statIDs := s.stats.AllAccounts()
	live := idsOf(workers)
	seen := make(map[string]bool, len(live))
	for _, id := range live {
		seen[id] = true
	}
	for _, id := range statIDs {
		if !seen[id] {
			live = append(live, id)
		}
	}
	statsRows := s.stats.ForAccounts(live)
	// Tag rows whose worker has been deleted (history kept, worker gone).
	liveSet := make(map[string]bool, len(workers))
	for _, w := range workers {
		liveSet[w.ID] = true
	}
	type statRow struct {
		stats.WorkerStat
		Deleted bool `json:"deleted"`
	}
	rows := make([]statRow, 0, len(statsRows))
	for _, st := range statsRows {
		rows = append(rows, statRow{WorkerStat: st, Deleted: !liveSet[st.AccountID]})
	}

	type workerView struct {
		ID               string `json:"id"`
		ProxyID          string `json:"proxyId"`
		Status           string `json:"status"`
		CooldownUntil    int64  `json:"cooldownUntil"`
		BannedUntil      int64  `json:"bannedUntil"`
		ConsecutiveFails int    `json:"consecutiveFails"`
		LastError        string `json:"lastError"`
		LastErrorAt      int64  `json:"lastErrorAt"`
	}
	views := make([]workerView, 0, len(workers))
	for _, w := range workers {
		var errAt int64
		if !w.LastErrorAt.IsZero() {
			errAt = w.LastErrorAt.UnixMilli()
		}
		views = append(views, workerView{
			ID: w.ID, ProxyID: w.ProxyID, Status: w.Status(now),
			CooldownUntil:    w.CooldownUntil.UnixMilli(),
			BannedUntil:      w.BannedUntil.UnixMilli(),
			ConsecutiveFails: w.ConsecutiveFails,
			LastError:        w.LastError,
			LastErrorAt:      errAt,
		})
	}

	totals := s.stats.Totals(live)
	overall := s.stats.OverallStats(live)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":      true,
		"startedAt":    s.started.Format(time.RFC3339),
		"baseUrl":      s.cfg.BaseURL,
		"workers":      views,
		"workerStats":  rows,
		"totals":       totals,
		"overall":      overall,
		"pool":         s.sanitizedPool(),
		"poolState":    s.poolState(),
		"callKeyCount": s.auth.CallKeyCount(),
		"authEnabled":  s.cfg.RequireCallKeyAuth,
		"port":         s.cfg.ListenPort,
	})
}

// poolState summarizes proxy-pool health so the UI can warn when requests
// are falling back to direct egress (all proxies unusable/disabled/empty).
type poolState struct {
	Total    int  `json:"total"`
	Enabled  int  `json:"enabled"`
	Usable   int  `json:"usable"`
	Bypassed bool `json:"bypassed"` // true => requests egress directly
}

func (s *Server) poolState() poolState {
	st := poolState{}
	for _, p := range s.cfg.ProxyPool {
		st.Total++
		if p.Enabled {
			st.Enabled++
		}
		if p.Enabled && p.Usable {
			st.Usable++
		}
	}
	// Bypass whenever there is no usable proxy to carry the request.
	st.Bypassed = st.Usable == 0
	// Also bypass when zero workers bind a usable proxy (bound pool only);
	// keep it simple: if pool exists but nothing usable => bypass.
	return st
}

func idsOf(states []rotator.State) []string {
	out := make([]string, 0, len(states))
	for _, st := range states {
		out = append(out, st.ID)
	}
	return out
}

// apiSettings GET/PUT the gateway configuration.
func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"baseUrl":            s.cfg.BaseURL,
			"port":               s.cfg.ListenPort,
			"synthesizeCli":      s.cfg.SynthesizeCLI,
			"cliUserAgent":       s.cfg.CLIUserAgent,
			"cliClient":          s.cfg.CLIClient,
			"cliProject":         s.cfg.CLIProject,
			"freeModelsFilter":   s.cfg.FreeModelsFilter,
			"requireCallKeyAuth": s.cfg.RequireCallKeyAuth,
		})
	case http.MethodPut, http.MethodPost:
		var body struct {
			BaseURL            *string `json:"baseUrl"`
			Port               *int    `json:"port"`
			SynthesizeCLI      *bool   `json:"synthesizeCli"`
			CLIUserAgent       *string `json:"cliUserAgent"`
			CLIClient          *string `json:"cliClient"`
			CLIProject         *string `json:"cliProject"`
			FreeModelsFilter   *bool   `json:"freeModelsFilter"`
			RequireCallKeyAuth *bool   `json:"requireCallKeyAuth"`
			Password           *string `json:"password"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if body.BaseURL != nil && strings.TrimSpace(*body.BaseURL) != "" {
			s.cfg.BaseURL = strings.TrimRight(strings.TrimSpace(*body.BaseURL), "/")
		}
		if body.Port != nil && *body.Port > 0 && *body.Port < 65536 {
			s.cfg.ListenPort = *body.Port
		}
		if body.SynthesizeCLI != nil {
			s.cfg.SynthesizeCLI = *body.SynthesizeCLI
		}
		if body.CLIUserAgent != nil {
			s.cfg.CLIUserAgent = *body.CLIUserAgent
		}
		if body.CLIClient != nil {
			s.cfg.CLIClient = *body.CLIClient
		}
		if body.CLIProject != nil {
			s.cfg.CLIProject = *body.CLIProject
		}
		if body.FreeModelsFilter != nil {
			s.cfg.FreeModelsFilter = *body.FreeModelsFilter
		}
		if body.RequireCallKeyAuth != nil {
			s.cfg.RequireCallKeyAuth = *body.RequireCallKeyAuth
		}
		if body.Password != nil && strings.TrimSpace(*body.Password) != "" {
			hash, err := auth.HashPassword(*body.Password)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": "hash failed"}})
				return
			}
			s.cfg.AdminPasswordHash = hash
			s.auth.SetAdminHash(hash)
		}
		if err := s.persistConfig(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "GET/PUT only"}})
	}
}

// apiPoolImport handles TXT/paste bulk import (http + socks5 lines).
// Every newly added proxy gets a matching worker automatically
// (apiKey defaults to "public", bound to the new proxy).
func (s *Server) apiPoolImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !readJSONBody(w, r, &body) {
		return
	}
	valid, invalid := pool.ParseBatch(body.Text)
	added, skipped, ids := s.pool.Import(valid)

	// Auto-create one worker per new proxy.
	for _, id := range ids {
		it, ok := s.pool.Get(id)
		if !ok {
			continue
		}
		name := it.Host
		// avoid duplicate worker ids
		workerID := name
		n := 1
		for s.workerExists(workerID) {
			n++
			workerID = name + "-" + strconv.Itoa(n)
		}
		s.cfg.Workers = append(s.cfg.Workers, config.Worker{
			ID:      workerID,
			APIKey:  "public",
			ProxyID: id,
		})
	}

	if err := s.persistPool(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	s.resync()
	writeJSON(w, http.StatusOK, map[string]any{
		"added": added, "skipped": skipped, "invalidLines": invalid,
		"ids": ids, "workersCreated": len(ids),
	})

	// Probe the newly imported proxies in the background so the usable
	// state is fresh and persisted shortly after import.
	if len(ids) > 0 {
		go func() {
			time.Sleep(200 * time.Millisecond)
			s.pool.ProbeAll(10 * time.Second)
			_ = s.persistPool()
		}()
	}
}

// workerExists reports whether a worker with the given id exists.
func (s *Server) workerExists(id string) bool {
	for _, w := range s.cfg.Workers {
		if w.ID == id {
			return true
		}
	}
	return false
}

// apiPoolProbe probes all enabled proxies (or specific ids).
func (s *Server) apiPoolProbe(w http.ResponseWriter, r *http.Request) {
	latencies := s.pool.ProbeAll(10 * time.Second)
	// Persist probe results so the relay (which reads cfg.ProxyPool) sees
	// the updated usable flags even after a restart.
	if err := s.persistPool(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"latencies": latencies})
}

// apiPoolPrune removes dead/disabled entries and the workers bound to them.
func (s *Server) apiPoolPrune(w http.ResponseWriter, r *http.Request) {
	removed, ids := s.pool.Prune()
	s.deleteWorkersByProxyIDs(ids)
	if err := s.persistPool(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	s.resync()
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// apiWorkers GET/POST (add/update/delete with ?action=delete&id=).
func (s *Server) apiWorkers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"workers": s.cfg.Workers})
	case http.MethodPost:
		var body struct {
			Action  *string `json:"action"`
			ID      *string `json:"id"`
			APIKey  *string `json:"apiKey"`
			ProxyID *string `json:"proxyId"`
			Name    *string `json:"name"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if body.Action != nil && *body.Action == "delete" && body.ID != nil {
			// Deleting a worker NEVER removes its historical stats.
			kept := s.cfg.Workers[:0]
			for _, w := range s.cfg.Workers {
				if w.ID != *body.ID {
					kept = append(kept, w)
				}
			}
			s.cfg.Workers = kept
			if err := s.persistConfig(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
				return
			}
			s.resync()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		// add or update
		if body.ID == nil || strings.TrimSpace(*body.ID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "id required"}})
			return
		}
		id := *body.ID
		idx := -1
		for i := range s.cfg.Workers {
			if s.cfg.Workers[i].ID == id {
				idx = i
				break
			}
		}
		nw := config.Worker{ID: id}
		if body.APIKey != nil {
			nw.APIKey = *body.APIKey
		}
		if body.ProxyID != nil {
			nw.ProxyID = *body.ProxyID
		}
		if idx >= 0 {
			if body.APIKey != nil {
				s.cfg.Workers[idx].APIKey = nw.APIKey
			}
			if body.ProxyID != nil {
				s.cfg.Workers[idx].ProxyID = nw.ProxyID
			}
		} else {
			s.cfg.Workers = append(s.cfg.Workers, nw)
		}
		if err := s.persistConfig(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		s.resync()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "GET/POST only"}})
	}
}

// apiCallKeys manages client-facing call keys.
func (s *Server) apiCallKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"keys": s.cfg.CallKeys})
	case http.MethodPost:
		var body struct {
			Action  *string `json:"action"`
			Key     *string `json:"key"`
			Name    *string `json:"name"`
			Enabled *bool   `json:"enabled"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if body.Action != nil && *body.Action == "delete" && body.Key != nil {
			kept := s.cfg.CallKeys[:0]
			for _, k := range s.cfg.CallKeys {
				if k.Key != *body.Key {
					kept = append(kept, k)
				}
			}
			s.cfg.CallKeys = kept
		} else if body.Key != nil && strings.TrimSpace(*body.Key) != "" {
			key := strings.TrimSpace(*body.Key)
			idx := -1
			for i := range s.cfg.CallKeys {
				if s.cfg.CallKeys[i].Key == key {
					idx = i
					break
				}
			}
			nk := config.CallKey{Key: key, Enabled: true}
			if body.Name != nil {
				nk.Name = *body.Name
			}
			if body.Enabled != nil {
				nk.Enabled = *body.Enabled
			}
			if idx >= 0 {
				s.cfg.CallKeys[idx] = nk
			} else {
				s.cfg.CallKeys = append(s.cfg.CallKeys, nk)
			}
		}
		if err := s.persistConfig(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		s.resyncAuth()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "GET/POST only"}})
	}
}

// resync pushes config into live components (rotator + relay).
func (s *Server) resync() {
	ws := make([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}, 0, len(s.cfg.Workers))
	for _, w := range s.cfg.Workers {
		ws = append(ws, struct {
			ID      string
			APIKey  string
			ProxyID string
		}{ID: w.ID, APIKey: w.APIKey, ProxyID: w.ProxyID})
	}
	s.rot.Sync(ws)
}

// deleteWorkersByProxyIDs removes every worker bound to one of the given
// proxy ids. Stats history is intentionally untouched.
func (s *Server) deleteWorkersByProxyIDs(proxyIDs []string) {
	if len(proxyIDs) == 0 {
		return
	}
	dead := make(map[string]bool, len(proxyIDs))
	for _, id := range proxyIDs {
		dead[id] = true
	}
	kept := s.cfg.Workers[:0]
	for _, w := range s.cfg.Workers {
		if w.ProxyID != "" && dead[w.ProxyID] {
			continue
		}
		kept = append(kept, w)
	}
	s.cfg.Workers = kept
}

// resyncAuth refreshes the call-key allow-list in the auth manager.
func (s *Server) resyncAuth() {
	m := make(map[string]string, len(s.cfg.CallKeys))
	for _, k := range s.cfg.CallKeys {
		if k.Enabled {
			m[k.Key] = k.Name
		}
	}
	s.auth.SetCallKeys(m)
}

// persistConfig writes settings.json.
func (s *Server) persistConfig() error { return s.store.Save(s.cfg) }

// persistPool saves the pool into settings.json (pool entries live there).
func (s *Server) persistPool() error {
	// reflect pool.Manager into config.Settings.ProxyPool
	items := s.pool.All()
	poolOut := make([]config.PoolProxy, 0, len(items))
	for _, it := range items {
		poolOut = append(poolOut, config.PoolProxy{
			ID: it.ID, Name: it.Name, Type: it.Type,
			Host: it.Host, Port: it.Port,
			Username: it.Username, Password: it.Password,
			Enabled: it.Enabled, Usable: it.Usable, Source: it.Source,
		})
	}
	s.cfg.ProxyPool = poolOut
	return s.persistConfig()
}

// loadPoolIntoManager hydrates pool.Manager from persisted settings.
func (s *Server) LoadPoolFromConfig() {
	// NOTE: Must preserve the persisted proxy IDs. Worker→proxy bindings in
	// settings.json reference the original px_* ids; using Import() would
	// regenerate new ids and silently break every worker binding (gateway then
	// falls back to direct egress). Use Restore() to keep ids stable across
	// restarts so workers actually route through their bound proxy.
	items := make([]pool.RestoreItem, 0, len(s.cfg.ProxyPool))
	for _, p := range s.cfg.ProxyPool {
		items = append(items, pool.RestoreItem{
			ID:       p.ID,
			Name:     p.Name,
			Type:     p.Type,
			Host:     p.Host,
			Port:     p.Port,
			Username: p.Username,
			Password: p.Password,
			Enabled:  p.Enabled,
			Usable:   p.Usable,
			Source:   p.Source,
		})
	}
	s.pool.Restore(items)
}

func hostPort(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}
