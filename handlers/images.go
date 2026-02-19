package handlers

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/docker/docker/api/types/image"
	"github.com/gorilla/mux"
)

// ImageSummary is the API representation of a Docker image (list item).
type ImageSummary struct {
	ID         string   `json:"id"`
	RepoTags   []string `json:"repo_tags"`
	Size       int64    `json:"size"`
	Created    int64    `json:"created"`
	Containers int64    `json:"containers"` // matches types.ImageSummary
}

// ListImages handles GET /images.
func (h *Handler) ListImages(w http.ResponseWriter, r *http.Request) {
	list, err := h.docker.ImageList(r.Context(), image.ListOptions{})
	if err != nil {
		slog.Error("list images", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list images")
		return
	}
	result := make([]ImageSummary, 0, len(list))
	for _, im := range list {
		result = append(result, ImageSummary{
			ID:         im.ID,
			RepoTags:   im.RepoTags,
			Size:       im.Size,
			Created:    im.Created,
			Containers: im.Containers,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// InspectImage handles GET /images/{id}.
func (h *Handler) InspectImage(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "image id is required")
		return
	}
	info, _, err := h.docker.ImageInspectWithRaw(r.Context(), id)
	if err != nil {
		slog.Error("inspect image", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to inspect image")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// RemoveImage handles DELETE /images/{id}.
func (h *Handler) RemoveImage(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "image id is required")
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	_, err := h.docker.ImageRemove(r.Context(), id, image.RemoveOptions{Force: force})
	if err != nil {
		slog.Error("remove image", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to remove image")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "removed"})
}

// PullImage handles POST /images/pull?image=name:tag.
// Consumes the pull stream and returns a summary on success.
func (h *Handler) PullImage(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("image")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "query parameter image is required")
		return
	}
	rdr, err := h.docker.ImagePull(r.Context(), ref, image.PullOptions{})
	if err != nil {
		slog.Error("pull image", "image", ref, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to pull image")
		return
	}
	defer rdr.Close()
	_, _ = io.Copy(io.Discard, rdr)
	writeJSON(w, http.StatusOK, map[string]string{"status": "pulled", "image": ref})
}
