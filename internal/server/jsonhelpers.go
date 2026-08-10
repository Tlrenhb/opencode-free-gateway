package server

import (
	"encoding/json"
	"io"
	"net/http"
)

// readJSONBody decodes a JSON request body with a 4 MiB cap.
func readJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(body).Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "bad JSON body: " + err.Error()},
		})
		return false
	}
	return true
}

// drainAndClose consumes and closes a body.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
