package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gorilla/mux"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	docker         *client.Client
	agentStore     AgentStorer
	agentConns     *AgentConnections
	agentServerURL string
}

// New returns a Handler backed by the given Docker client.
func New(docker *client.Client) *Handler {
	return &Handler{docker: docker, agentConns: newAgentConnections()}
}

// NewWithAgents returns a Handler with agent support enabled.
func NewWithAgents(docker *client.Client, as AgentStorer, serverURL string) *Handler {
	return &Handler{
		docker:         docker,
		agentStore:     as,
		agentConns:     newAgentConnections(),
		agentServerURL: serverURL,
	}
}

// ContainerSummary is the API representation of a Docker container.
type ContainerSummary struct {
	ID      string            `json:"id"`
	Names   []string          `json:"names"`
	Image   string            `json:"image"`
	Status  string            `json:"status"`
	State   string            `json:"state"`
	Labels  map[string]string `json:"labels"`
	Created int64             `json:"created"`
}

// ListContainers handles GET /containers.
// Returns all containers (running and stopped).
func (h *Handler) ListContainers(w http.ResponseWriter, r *http.Request) {
	list, err := h.docker.ContainerList(r.Context(), container.ListOptions{All: true})
	if err != nil {
		slog.Error("list containers", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list containers")
		return
	}

	result := make([]ContainerSummary, 0, len(list))
	for _, c := range list {
		result = append(result, ContainerSummary{
			ID:      c.ID,
			Names:   c.Names,
			Image:   c.Image,
			Status:  c.Status,
			State:   c.State,
			Labels:  c.Labels,
			Created: c.Created,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

// StartContainer handles POST /containers/start/{id}.
func (h *Handler) StartContainer(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "container id is required")
		return
	}

	if err := h.docker.ContainerStart(r.Context(), id, container.StartOptions{}); err != nil {
		slog.Error("start container", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start container")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "started"})
}

// StopContainer handles POST /containers/stop/{id}.
func (h *Handler) StopContainer(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "container id is required")
		return
	}

	if err := h.docker.ContainerStop(r.Context(), id, container.StopOptions{}); err != nil {
		slog.Error("stop container", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to stop container")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "stopped"})
}

// RestartContainer handles POST /containers/restart/{id}.
func (h *Handler) RestartContainer(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "container id is required")
		return
	}
	timeout := 10
	if err := h.docker.ContainerRestart(r.Context(), id, container.StopOptions{Timeout: &timeout}); err != nil {
		slog.Error("restart container", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to restart container")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "restarted"})
}

// RunContainer handles POST /containers/run.
// Body (required): {"image": "<image>", "name": "<name>"}
// Optional fields: "command" ([]string), "environment" ([]string "KEY=VALUE"),
// "labels" (object), "binds" ([]string "hostPath:containerPath"),
// "shm_size" (int64, bytes — e.g. 1073741824 for 1 GiB).
// Defaults to `sleep infinity` command when none is specified.
// Returns: {"id": "<full-id>", "name": "<name>"}
func (h *Handler) RunContainer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Image       string            `json:"image"`
		Name        string            `json:"name"`
		Command     []string          `json:"command,omitempty"`
		Environment []string          `json:"environment,omitempty"`
		Labels      map[string]string `json:"labels,omitempty"`
		Binds       []string          `json:"binds,omitempty"`
		ShmSize     int64             `json:"shm_size,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Image == "" {
		writeError(w, http.StatusBadRequest, "body must include 'image'")
		return
	}

	ctx := r.Context()

	// Pull image if not already present.
	reader, err := h.docker.ImagePull(ctx, body.Image, image.PullOptions{})
	if err != nil {
		slog.Error("image pull failed", "image", body.Image, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to pull image: "+err.Error())
		return
	}
	io.Copy(io.Discard, reader) //nolint:errcheck
	reader.Close()

	cmd := body.Command
	if len(cmd) == 0 {
		cmd = []string{"sleep", "infinity"}
	}

	cfg := &container.Config{
		Image:  body.Image,
		Cmd:    cmd,
		Tty:    false,
		Env:    body.Environment,
		Labels: body.Labels,
	}
	hostCfg := &container.HostConfig{
		AutoRemove: false,
		Binds:      body.Binds,
		ShmSize:    body.ShmSize,
	}

	resp, err := h.docker.ContainerCreate(ctx, cfg, hostCfg, nil, nil, body.Name)
	if err != nil {
		slog.Error("container create failed", "image", body.Image, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create container: "+err.Error())
		return
	}

	if err := h.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		slog.Error("container start failed", "id", resp.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start container: "+err.Error())
		return
	}

	slog.Info("container run", "id", resp.ID, "image", body.Image, "name", body.Name)
	writeJSON(w, http.StatusOK, map[string]string{"id": resp.ID, "name": body.Name})
}

// RemoveContainer handles DELETE /containers/{id}.
func (h *Handler) RemoveContainer(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "container id is required")
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if err := h.docker.ContainerRemove(r.Context(), id, container.RemoveOptions{Force: force}); err != nil {
		slog.Error("remove container", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to remove container")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "removed"})
}

// InspectContainer handles GET /containers/{id}.
func (h *Handler) InspectContainer(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "container id is required")
		return
	}
	info, err := h.docker.ContainerInspect(r.Context(), id)
	if err != nil {
		slog.Error("inspect container", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to inspect container")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// GetContainerLogs handles GET /containers/{id}/logs.
// Query params: tail (number, default 100), since, until (optional).
// Returns plain text (stdout and stderr combined). Non-streaming only.
func (h *Handler) GetContainerLogs(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "container id is required")
		return
	}
	tailStr := r.URL.Query().Get("tail")
	if tailStr == "" {
		tailStr = "100"
	}
	tail, _ := strconv.Atoi(tailStr)
	if tail <= 0 {
		tail = 100
	}
	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
		Follow:     false,
	}
	if s := r.URL.Query().Get("since"); s != "" {
		opts.Since = s
	}
	if u := r.URL.Query().Get("until"); u != "" {
		opts.Until = u
	}
	rdr, err := h.docker.ContainerLogs(r.Context(), id, opts)
	if err != nil {
		slog.Error("container logs", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get container logs")
		return
	}
	defer rdr.Close()
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, rdr); err != nil && err != io.EOF {
		slog.Error("copy container logs", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read container logs")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
