package handlers

import (
	"log/slog"
	"net/http"

	"github.com/docker/docker/api/types"
)

// GetSystemInfo handles GET /system/info.
// Returns Docker daemon and host info (version, OS, driver, memory, CPUs, etc.).
func (h *Handler) GetSystemInfo(w http.ResponseWriter, r *http.Request) {
	info, err := h.docker.Info(r.Context())
	if err != nil {
		slog.Error("system info", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get system info")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// GetSystemDisk handles GET /system/disk.
// Returns disk usage for images, containers, volumes, and build cache.
func (h *Handler) GetSystemDisk(w http.ResponseWriter, r *http.Request) {
	usage, err := h.docker.DiskUsage(r.Context(), types.DiskUsageOptions{})
	if err != nil {
		slog.Error("system disk", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get disk usage")
		return
	}
	writeJSON(w, http.StatusOK, usage)
}
