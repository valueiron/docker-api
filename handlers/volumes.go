package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/gorilla/mux"
)

// VolumeSummary is the API representation of a volume (list item).
type VolumeSummary struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	Labels     map[string]string `json:"labels"`
}

// ListVolumes handles GET /volumes.
func (h *Handler) ListVolumes(w http.ResponseWriter, r *http.Request) {
	list, err := h.docker.VolumeList(r.Context(), volume.ListOptions{})
	if err != nil {
		slog.Error("list volumes", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list volumes")
		return
	}
	result := make([]VolumeSummary, 0, len(list.Volumes))
	for _, v := range list.Volumes {
		result = append(result, VolumeSummary{
			Name:       v.Name,
			Driver:     v.Driver,
			Mountpoint: v.Mountpoint,
			Labels:     v.Labels,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// InspectVolume handles GET /volumes/{name}.
func (h *Handler) InspectVolume(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if name == "" {
		writeError(w, http.StatusBadRequest, "volume name is required")
		return
	}
	info, err := h.docker.VolumeInspect(r.Context(), name)
	if err != nil {
		slog.Error("inspect volume", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to inspect volume")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// CreateVolumeRequest is the JSON body for POST /volumes.
type CreateVolumeRequest struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Labels     map[string]string `json:"labels"`
	DriverOpts map[string]string `json:"driver_opts"`
}

// CreateVolume handles POST /volumes.
func (h *Handler) CreateVolume(w http.ResponseWriter, r *http.Request) {
	var req CreateVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	opts := volume.CreateOptions{
		Name:       req.Name,
		Driver:     req.Driver,
		Labels:     req.Labels,
		DriverOpts: req.DriverOpts,
	}
	vol, err := h.docker.VolumeCreate(r.Context(), opts)
	if err != nil {
		slog.Error("create volume", "name", req.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create volume")
		return
	}
	writeJSON(w, http.StatusCreated, vol)
}

// RemoveVolume handles DELETE /volumes/{name}.
func (h *Handler) RemoveVolume(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if name == "" {
		writeError(w, http.StatusBadRequest, "volume name is required")
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if err := h.docker.VolumeRemove(r.Context(), name, force); err != nil {
		slog.Error("remove volume", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to remove volume")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PruneVolumes handles POST /volumes/prune.
func (h *Handler) PruneVolumes(w http.ResponseWriter, r *http.Request) {
	report, err := h.docker.VolumesPrune(r.Context(), filters.Args{})
	if err != nil {
		slog.Error("prune volumes", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to prune volumes")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
