package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"
)

// ContainerMetrics is the API representation of a container's resource usage.
type ContainerMetrics struct {
	ContainerID   string  `json:"container_id"`
	Name          string  `json:"name"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemUsageMiB   float64 `json:"mem_usage_mib"`
	MemLimitMiB   float64 `json:"mem_limit_mib"`
	MemPercent    float64 `json:"mem_percent"`
	NetRxMiB      float64 `json:"net_rx_mib"`
	NetTxMiB      float64 `json:"net_tx_mib"`
	BlockReadMiB  float64 `json:"block_read_mib"`
	BlockWriteMiB float64 `json:"block_write_mib"`
	PIDs          uint64  `json:"pids"`
}

// dockerStats mirrors the Docker Engine stats JSON payload. Defined locally to
// remain version-agnostic with respect to the Docker SDK.
type dockerStats struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`

	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`

	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"` // includes "cache" on cgroup v1
	} `json:"memory_stats"`

	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`

	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`

	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

// GetContainerMetrics handles GET /containers/{id}/metrics.
// Returns a one-shot snapshot of CPU, memory, network and block I/O usage.
func (h *Handler) GetContainerMetrics(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest, "container id is required")
		return
	}

	// stream=false → single stats snapshot, no keep-alive
	resp, err := h.docker.ContainerStats(r.Context(), id, false)
	if err != nil {
		slog.Error("get container stats", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get container stats")
		return
	}
	defer resp.Body.Close()

	var s dockerStats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		slog.Error("decode container stats", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to decode stats")
		return
	}

	writeJSON(w, http.StatusOK, computeMetrics(id, s))
}

const mib = 1024 * 1024

func computeMetrics(id string, s dockerStats) ContainerMetrics {
	m := ContainerMetrics{
		ContainerID: id,
		Name:        s.Name,
		PIDs:        s.PidsStats.Current,
	}

	// ── CPU % ─────────────────────────────────────────────────────────────────
	// Mirrors the calculation used by `docker stats`.
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) -
		float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) -
		float64(s.PreCPUStats.SystemUsage)
	numCPU := float64(s.CPUStats.OnlineCPUs)
	if numCPU == 0 {
		numCPU = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if sysDelta > 0 && cpuDelta > 0 {
		m.CPUPercent = (cpuDelta / sysDelta) * numCPU * 100.0
	}

	// ── Memory ────────────────────────────────────────────────────────────────
	// Subtract page-cache to get RSS (cgroup v1 only; cgroup v2 uses
	// "inactive_file"). Fall back to raw usage if neither key exists.
	memUsage := s.MemoryStats.Usage
	if cache, ok := s.MemoryStats.Stats["inactive_file"]; ok && memUsage > cache {
		memUsage -= cache
	} else if cache, ok := s.MemoryStats.Stats["cache"]; ok && memUsage > cache {
		memUsage -= cache
	}
	m.MemUsageMiB = float64(memUsage) / mib
	m.MemLimitMiB = float64(s.MemoryStats.Limit) / mib
	if s.MemoryStats.Limit > 0 {
		m.MemPercent = float64(memUsage) / float64(s.MemoryStats.Limit) * 100.0
	}

	// ── Network I/O ───────────────────────────────────────────────────────────
	for _, iface := range s.Networks {
		m.NetRxMiB += float64(iface.RxBytes) / mib
		m.NetTxMiB += float64(iface.TxBytes) / mib
	}

	// ── Block I/O ─────────────────────────────────────────────────────────────
	for _, entry := range s.BlkioStats.IoServiceBytesRecursive {
		switch entry.Op {
		case "Read":
			m.BlockReadMiB += float64(entry.Value) / mib
		case "Write":
			m.BlockWriteMiB += float64(entry.Value) / mib
		}
	}

	return m
}
