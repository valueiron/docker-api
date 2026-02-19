package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/gorilla/mux"
)

// NetworkSummary is the API representation of a network (list item).
type NetworkSummary struct {
	ID         string                 `json:"id"`
	Name       string                 `json:"name"`
	Driver     string                 `json:"driver"`
	Scope      string                 `json:"scope"`
	Containers map[string]interface{}  `json:"containers"` // container id -> endpoint info
}

// ListNetworks handles GET /networks.
func (h *Handler) ListNetworks(w http.ResponseWriter, r *http.Request) {
	list, err := h.docker.NetworkList(r.Context(), types.NetworkListOptions{})
	if err != nil {
		slog.Error("list networks", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list networks")
		return
	}
	result := make([]NetworkSummary, 0, len(list))
	for _, n := range list {
		containers := make(map[string]interface{})
		for k, v := range n.Containers {
			containers[k] = v
		}
		result = append(result, NetworkSummary{
			ID:         n.ID,
			Name:       n.Name,
			Driver:     n.Driver,
			Scope:      n.Scope,
			Containers: containers,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// InspectNetwork handles GET /networks/{id}.
func (h *Handler) InspectNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "network id or name is required")
		return
	}
	info, err := h.docker.NetworkInspect(r.Context(), id, types.NetworkInspectOptions{})
	if err != nil {
		slog.Error("inspect network", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to inspect network")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// CreateNetworkRequest is the JSON body for POST /networks.
type CreateNetworkRequest struct {
	Name   string            `json:"name"`
	Driver string            `json:"driver"`
	Options map[string]string `json:"options"`
	IPAM   *network.IPAM     `json:"ipam,omitempty"`
}

// CreateNetwork handles POST /networks.
func (h *Handler) CreateNetwork(w http.ResponseWriter, r *http.Request) {
	var req CreateNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	opts := types.NetworkCreate{
		Driver:  req.Driver,
		Options: req.Options,
		IPAM:    req.IPAM,
	}
	resp, err := h.docker.NetworkCreate(r.Context(), req.Name, opts)
	if err != nil {
		slog.Error("create network", "name", req.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create network")
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// RemoveNetwork handles DELETE /networks/{id}.
func (h *Handler) RemoveNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "network id or name is required")
		return
	}
	if err := h.docker.NetworkRemove(r.Context(), id); err != nil {
		slog.Error("remove network", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to remove network")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ConnectRequest is the JSON body for POST /networks/{id}/connect.
type ConnectRequest struct {
	Container string `json:"container"`
}

// ConnectNetwork handles POST /networks/{id}/connect.
func (h *Handler) ConnectNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "network id or name is required")
		return
	}
	var req ConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Container == "" {
		writeError(w, http.StatusBadRequest, "container is required")
		return
	}
	if err := h.docker.NetworkConnect(r.Context(), id, req.Container, nil); err != nil {
		slog.Error("connect network", "network", id, "container", req.Container, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to connect container to network")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DisconnectRequest is the JSON body for POST /networks/{id}/disconnect.
type DisconnectRequest struct {
	Container string `json:"container"`
	Force     bool   `json:"force"`
}

// DisconnectNetwork handles POST /networks/{id}/disconnect.
func (h *Handler) DisconnectNetwork(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "network id or name is required")
		return
	}
	var req DisconnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Container == "" {
		writeError(w, http.StatusBadRequest, "container is required")
		return
	}
	if err := h.docker.NetworkDisconnect(r.Context(), id, req.Container, req.Force); err != nil {
		slog.Error("disconnect network", "network", id, "container", req.Container, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to disconnect container from network")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PruneNetworks handles POST /networks/prune.
func (h *Handler) PruneNetworks(w http.ResponseWriter, r *http.Request) {
	report, err := h.docker.NetworksPrune(r.Context(), filters.Args{})
	if err != nil {
		slog.Error("prune networks", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to prune networks")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
