package handlers

import (
	"encoding/json"
	"net/http"
)

// Health handles GET /health.
// Returns {"status":"ok"} when the service is running.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
