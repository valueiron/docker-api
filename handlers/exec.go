package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	dtypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// dockerExecSession holds the container target for a pending exec WebSocket connection.
type dockerExecSession struct {
	containerID string
	shell       string // "auto", "bash", "sh"
}

var (
	dockerExecSessions   = map[string]dockerExecSession{}
	dockerExecSessionsMu sync.Mutex
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// dockerExecCommand returns the shell command to run in the container.
// The "auto" mode uses script(1) to force a fresh PTY allocation, which ensures
// proper echo and prompt rendering in interactive shells (same strategy as k8s-api).
func dockerExecCommand(shell string) []string {
	base := "export TERM=xterm-256color; "
	switch shell {
	case "sh":
		return []string{"/bin/sh", "-c", base + "exec sh -i"}
	case "bash":
		return []string{"/bin/sh", "-c", base + "exec bash -i 2>/dev/null || exec sh -i"}
	default: // auto
		return []string{"/bin/sh", "-c", base + `(script -q -c "exec bash -i" /dev/null) 2>/dev/null || (exec bash -i) 2>/dev/null || exec sh -i`}
	}
}

// CreateContainerExecSession handles POST /containers/{id}/exec.
// Body (optional JSON): {"shell": "auto"|"bash"|"sh"}
// Returns: {"sessionId": "<uuid>"}
func (h *Handler) CreateContainerExecSession(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	var body struct {
		Shell string `json:"shell"`
	}
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck

	shell := body.Shell
	if shell != "bash" && shell != "sh" {
		shell = "auto"
	}

	sessionID := uuid.New().String()
	dockerExecSessionsMu.Lock()
	dockerExecSessions[sessionID] = dockerExecSession{containerID: id, shell: shell}
	dockerExecSessionsMu.Unlock()

	slog.Info("docker exec session created", "sessionId", sessionID, "containerID", id)
	writeJSON(w, http.StatusOK, map[string]string{"sessionId": sessionID})
}

// ContainerExecWS handles GET /containers/exec/ws?sessionId=<uuid>.
// Upgrades to WebSocket and bridges browser ↔ docker exec (TTY mode).
//
// Browser → server message protocol:
//   - Binary frame or plain text  →  stdin
//   - JSON {"type":"resize","cols":N,"rows":N}  →  terminal resize
//   - JSON {"type":"ping"}        →  pong response
//   - JSON {"type":"inject","data":"..."}  →  write literal string to stdin
//
// Server → browser message protocol:
//   - Binary frames  →  stdout/stderr (merged in TTY mode)
//   - JSON {"type":"connected"}
//   - JSON {"type":"disconnected"}
//   - JSON {"type":"pong"}
//   - JSON {"type":"error","message":"..."}
func (h *Handler) ContainerExecWS(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		http.Error(w, `{"error":"sessionId required"}`, http.StatusBadRequest)
		return
	}

	dockerExecSessionsMu.Lock()
	session, ok := dockerExecSessions[sessionID]
	if ok {
		delete(dockerExecSessions, sessionID)
	}
	dockerExecSessionsMu.Unlock()

	if !ok {
		http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	// Clear any deadlines set by the HTTP server so long-lived sessions aren't killed.
	conn.SetReadDeadline(time.Time{})  //nolint:errcheck
	conn.SetWriteDeadline(time.Time{}) //nolint:errcheck

	// gorilla/websocket requires a single concurrent writer — protect with a mutex.
	var writeMu sync.Mutex
	writeMsg := func(msgType int, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(msgType, data)
	}
	sendJSON := func(v any) {
		data, _ := json.Marshal(v)
		writeMsg(websocket.TextMessage, data) //nolint:errcheck
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Create the exec instance inside the container.
	execID, err := h.docker.ContainerExecCreate(ctx, session.containerID, dtypes.ExecConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          dockerExecCommand(session.shell),
	})
	if err != nil {
		slog.Error("exec create failed", "containerID", session.containerID, "error", err)
		sendJSON(map[string]string{"type": "error", "message": "failed to create exec: " + err.Error()})
		return
	}

	// Attach to the exec instance. In TTY mode, stdout/stderr are merged into resp.Reader.
	resp, err := h.docker.ContainerExecAttach(ctx, execID.ID, dtypes.ExecStartCheck{Tty: true})
	if err != nil {
		slog.Error("exec attach failed", "execID", execID.ID, "error", err)
		sendJSON(map[string]string{"type": "error", "message": "failed to attach: " + err.Error()})
		return
	}
	defer resp.Close()

	// Goroutine: forward exec stdout/stderr → browser as binary WebSocket frames.
	// When the exec process exits, resp.Reader returns EOF which triggers disconnected.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := resp.Reader.Read(buf)
			if n > 0 {
				if err := writeMsg(websocket.BinaryMessage, buf[:n]); err != nil {
					return // browser gone
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					slog.Error("exec read error", "sessionId", sessionID, "error", readErr)
				}
				sendJSON(map[string]string{"type": "disconnected"})
				writeMu.Lock()
				conn.Close()
				writeMu.Unlock()
				return
			}
		}
	}()

	sendJSON(map[string]string{"type": "connected", "sessionId": sessionID})
	slog.Info("docker exec session started", "sessionId", sessionID, "containerID", session.containerID)

	// Main loop: read messages from the browser and dispatch them.
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			break // browser closed, or conn.Close() called by stdout goroutine
		}

		if msgType == websocket.TextMessage {
			var msg struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
				Data string `json:"data"`
			}
			if json.Unmarshal(data, &msg) == nil {
				switch msg.Type {
				case "resize":
					h.docker.ContainerExecResize(ctx, execID.ID, container.ResizeOptions{ //nolint:errcheck
						Height: uint(msg.Rows),
						Width:  uint(msg.Cols),
					})
					continue
				case "ping":
					sendJSON(map[string]string{"type": "pong"})
					continue
				case "inject":
					if msg.Data != "" {
						resp.Conn.Write([]byte(msg.Data)) //nolint:errcheck
					}
					continue
				}
			}
			resp.Conn.Write(data) //nolint:errcheck
		} else {
			resp.Conn.Write(data) //nolint:errcheck
		}
	}

	// Browser disconnected — cancel context and close stdin.
	cancel()
	resp.CloseWrite() //nolint:errcheck
	slog.Info("docker exec session ended", "sessionId", sessionID)
}
