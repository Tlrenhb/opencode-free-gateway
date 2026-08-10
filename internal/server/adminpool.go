package server

import (
	"net/http"
	"strconv"
	"strings"
)

// apiPool handles pool listing, single-entry removal and toggle.
func (s *Server) apiPool(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"pool": s.pool.All()})
	case http.MethodPost:
		var body struct {
			Action *string `json:"action"`
			ID     *string `json:"id"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if body.ID == nil || *body.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "id required"}})
			return
		}
		action := ""
		if body.Action != nil {
			action = *body.Action
		}
		switch action {
		case "remove":
			s.pool.Remove(*body.ID)
			// Remove any worker bound to this proxy (stats history kept).
			s.deleteWorkersByProxyIDs([]string{*body.ID})
			if err := s.persistPool(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
				return
			}
			s.resync()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		case "toggle":
			if it, ok := s.pool.Get(*body.ID); ok {
				s.pool.SetEnabled(*body.ID, !it.Enabled)
			}
			if err := s.persistPool(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "unknown action"}})
		}
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "GET/POST only"}})
	}
}

// formatHostPort is a small helper for address strings.
func formatHostPort(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

// joinNonEmpty joins non-empty strings with the given separator.
func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
