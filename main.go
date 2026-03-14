package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/example/docker-api/handlers"
	"github.com/example/docker-api/store"
	"github.com/gorilla/mux"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		runHealthCheck()
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		slog.Error("failed to create docker client", "error", err)
		os.Exit(1)
	}
	defer dockerClient.Close()

	var h *handlers.Handler
	if redisAddr := os.Getenv("REDIS_ADDR"); redisAddr != "" {
		as, err := store.NewAgentStore(redisAddr)
		if err != nil {
			slog.Warn("agent store unavailable, running without agent support", "error", err)
			h = handlers.New(dockerClient)
		} else {
			defer as.Close()
			agentServerURL := os.Getenv("AGENT_SERVER_URL")
			h = handlers.NewWithAgents(dockerClient, as, agentServerURL)
			slog.Info("agent store connected", "redis", redisAddr)
		}
	} else {
		h = handlers.New(dockerClient)
	}

	r := mux.NewRouter()
	r.Use(loggingMiddleware)

	r.HandleFunc("/health", h.Health).Methods(http.MethodGet)

	// Container endpoints (specific paths before /containers/{id})
	r.HandleFunc("/containers", h.ListContainers).Methods(http.MethodGet)
	r.HandleFunc("/containers/run", h.RunContainer).Methods(http.MethodPost)
	r.HandleFunc("/containers/start/{id}", h.StartContainer).Methods(http.MethodPost)
	r.HandleFunc("/containers/stop/{id}", h.StopContainer).Methods(http.MethodPost)
	r.HandleFunc("/containers/restart/{id}", h.RestartContainer).Methods(http.MethodPost)
	r.HandleFunc("/containers/exec/ws", h.ContainerExecWS).Methods(http.MethodGet)
	r.HandleFunc("/containers/{id}", h.InspectContainer).Methods(http.MethodGet)
	r.HandleFunc("/containers/{id}", h.RemoveContainer).Methods(http.MethodDelete)
	r.HandleFunc("/containers/{id}/logs", h.GetContainerLogs).Methods(http.MethodGet)
	r.HandleFunc("/containers/{id}/metrics", h.GetContainerMetrics).Methods(http.MethodGet)
	r.HandleFunc("/containers/{id}/exec", h.CreateContainerExecSession).Methods(http.MethodPost)

	// System endpoints
	r.HandleFunc("/system/info", h.GetSystemInfo).Methods(http.MethodGet)
	r.HandleFunc("/system/disk", h.GetSystemDisk).Methods(http.MethodGet)

	// Image endpoints
	r.HandleFunc("/images", h.ListImages).Methods(http.MethodGet)
	r.HandleFunc("/images/pull", h.PullImage).Methods(http.MethodPost)
	r.HandleFunc("/images/{id}", h.InspectImage).Methods(http.MethodGet)
	r.HandleFunc("/images/{id}", h.RemoveImage).Methods(http.MethodDelete)

	// Volume endpoints (specific before /volumes/{name})
	r.HandleFunc("/volumes", h.ListVolumes).Methods(http.MethodGet)
	r.HandleFunc("/volumes", h.CreateVolume).Methods(http.MethodPost)
	r.HandleFunc("/volumes/prune", h.PruneVolumes).Methods(http.MethodPost)
	r.HandleFunc("/volumes/{name}", h.InspectVolume).Methods(http.MethodGet)
	r.HandleFunc("/volumes/{name}", h.RemoveVolume).Methods(http.MethodDelete)

	// Network endpoints (specific before /networks/{id})
	r.HandleFunc("/networks", h.ListNetworks).Methods(http.MethodGet)
	r.HandleFunc("/networks", h.CreateNetwork).Methods(http.MethodPost)
	r.HandleFunc("/networks/prune", h.PruneNetworks).Methods(http.MethodPost)
	r.HandleFunc("/networks/{id}/connect", h.ConnectNetwork).Methods(http.MethodPost)
	r.HandleFunc("/networks/{id}/disconnect", h.DisconnectNetwork).Methods(http.MethodPost)
	r.HandleFunc("/networks/{id}", h.InspectNetwork).Methods(http.MethodGet)
	r.HandleFunc("/networks/{id}", h.RemoveNetwork).Methods(http.MethodDelete)

	// Vulnerability scanning via Trivy container
	r.HandleFunc("/vulnerabilities/status", h.ScanStatus).Methods(http.MethodGet)
	r.HandleFunc("/vulnerabilities/download", h.TriggerDownload).Methods(http.MethodPost)
	r.HandleFunc("/vulnerabilities/scan", h.ScanImage).Methods(http.MethodPost)

	// Agent management + gateway (order matters: specific paths before parameterised ones)
	r.HandleFunc("/agents/hosts", h.ListHosts).Methods(http.MethodGet)
	r.HandleFunc("/agents/ws", h.AgentGatewayWS).Methods(http.MethodGet)
	r.HandleFunc("/agents", h.CreateAgent).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}", h.DeleteAgent).Methods(http.MethodDelete)
	r.HandleFunc("/agents/{id}/exec/ws", h.AgentExecWS).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/proxy/{path:.*}", h.AgentProxy).Methods(
		http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch,
	)

	addr := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("server starting", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	slog.Info("shutdown signal received", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	slog.Info("server stopped gracefully")
}

// responseWriter wraps http.ResponseWriter to capture the status code for logging.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker so that gorilla/websocket can take over
// the underlying TCP connection. Without this, the type assertion in
// websocket.Upgrader.Upgrade() fails and the handshake returns HTTP 500.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("responseWriter: underlying writer does not support hijacking")
	}
	return h.Hijack()
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := newResponseWriter(w)
		next.ServeHTTP(rw, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

// runHealthCheck performs an HTTP GET against the /health endpoint and exits
// with a non-zero code on failure. Used as the container health probe so that
// the distroless runtime image does not need curl or wget.
func runHealthCheck() {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://localhost:8080/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: unexpected status %d\n", resp.StatusCode)
		os.Exit(1)
	}
}
