package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"bytes"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/stdcopy"
)

const trivyImage = "aquasec/trivy:latest"

var (
	trivyMu      sync.Mutex
	trivyPulled  bool
	trivyPulling bool
	trivyVersion string
)

// ScanStatus handles GET /vulnerabilities/status.
func (h *Handler) ScanStatus(w http.ResponseWriter, r *http.Request) {
	// Check if trivy image exists locally
	f := filters.NewArgs()
	f.Add("reference", trivyImage)
	images, err := h.docker.ImageList(r.Context(), image.ListOptions{Filters: f})
	if err != nil {
		slog.Error("list trivy image", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to check trivy status")
		return
	}

	trivyMu.Lock()
	if len(images) > 0 {
		trivyPulled = true
		if trivyVersion == "" && len(images[0].RepoDigests) > 0 {
			trivyVersion = images[0].RepoDigests[0]
		}
		if trivyVersion == "" {
			trivyVersion = trivyImage
		}
	}
	ready := trivyPulled
	pulling := trivyPulling
	version := trivyVersion
	trivyMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ready":   ready,
		"pulling": pulling,
		"version": version,
	})
}

// TriggerDownload handles POST /vulnerabilities/download.
func (h *Handler) TriggerDownload(w http.ResponseWriter, r *http.Request) {
	trivyMu.Lock()
	if trivyPulled {
		trivyMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "pulling": false, "version": trivyVersion})
		return
	}
	if trivyPulling {
		trivyMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ready": false, "pulling": true, "version": ""})
		return
	}
	trivyPulling = true
	trivyMu.Unlock()

	go h.downloadTrivyImage()

	writeJSON(w, http.StatusOK, map[string]any{"ready": false, "pulling": true, "version": ""})
}

func (h *Handler) downloadTrivyImage() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	slog.Info("pulling trivy image", "image", trivyImage)
	rdr, err := h.docker.ImagePull(ctx, trivyImage, image.PullOptions{})
	if err != nil {
		slog.Error("pull trivy image", "error", err)
		trivyMu.Lock()
		trivyPulling = false
		trivyMu.Unlock()
		return
	}
	defer rdr.Close()
	_, _ = io.Copy(io.Discard, rdr)

	trivyMu.Lock()
	trivyPulled = true
	trivyPulling = false
	trivyVersion = trivyImage
	trivyMu.Unlock()
	slog.Info("trivy image pulled successfully")
}

// ScanImage handles POST /vulnerabilities/scan.
func (h *Handler) ScanImage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Image == "" {
		writeError(w, http.StatusBadRequest, "image field is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	result, err := h.runTrivyScan(ctx, req.Image)
	if err != nil {
		slog.Error("trivy scan", "image", req.Image, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func (h *Handler) runTrivyScan(ctx context.Context, imageRef string) ([]byte, error) {
	resp, err := h.docker.ContainerCreate(ctx,
		&container.Config{
			Image: trivyImage,
			Cmd:   []string{"image", "--format", "json", "--quiet", imageRef},
		},
		&container.HostConfig{
			Binds:      []string{"/var/run/docker.sock:/var/run/docker.sock"},
			AutoRemove: false,
		},
		nil, nil, "",
	)
	if err != nil {
		return nil, err
	}
	containerID := resp.ID

	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_ = h.docker.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true})
	}()

	if err := h.docker.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return nil, err
	}

	statusCh, errCh := h.docker.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return nil, err
		}
	case <-statusCh:
	}

	logs, err := h.docker.ContainerLogs(ctx, containerID, container.LogsOptions{ShowStdout: true})
	if err != nil {
		return nil, err
	}
	defer logs.Close()

	var stdout bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, io.Discard, logs); err != nil && err != io.EOF {
		return nil, err
	}

	raw := bytes.TrimSpace(stdout.Bytes())
	if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') {
		return []byte(`{"Results":[]}`), nil
	}
	return raw, nil
}
