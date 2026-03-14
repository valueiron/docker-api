package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/example/docker-api/store"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// AgentStorer is the interface the Handler uses to persist agent registrations.
// *store.AgentStore satisfies this interface.
type AgentStorer interface {
	ListAgents(ctx context.Context) ([]*store.Agent, error)
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
	CreateAgent(ctx context.Context, name, tokenHash string) (*store.Agent, error)
	DeleteAgent(ctx context.Context, id string) error
	UpdateAgentStatus(ctx context.Context, id, status, dockerVersion, hostname string) error
	FindByTokenHash(ctx context.Context, hash string) (*store.Agent, error)
	Close()
}

// ── In-memory agent connection registry ──────────────────────────────────────

type agentConn struct {
	ws            *websocket.Conn
	wsMu          sync.Mutex
	pending       map[string]chan tunnelResponse
	pendingMu     sync.Mutex
	execChans     map[string]chan execMsg // execId → channel for exec I/O
	execChansMu   sync.Mutex
	hostname      string
	dockerVersion string
	done          chan struct{}
}

// execMsg carries a single exec_output or exec_close message from the agent.
type execMsg struct {
	Type   string // "exec_output" or "exec_close"
	Data   string // base64-encoded stdout (exec_output only)
}

func (c *agentConn) sendJSON(v any) error {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	return c.ws.WriteJSON(v)
}

// AgentConnections is a thread-safe registry of live agent WebSocket connections.
type AgentConnections struct {
	mu    sync.RWMutex
	conns map[string]*agentConn
}

func newAgentConnections() *AgentConnections {
	return &AgentConnections{conns: make(map[string]*agentConn)}
}

func (ac *AgentConnections) add(id string, c *agentConn) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.conns[id] = c
}

func (ac *AgentConnections) remove(id string) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	delete(ac.conns, id)
}

func (ac *AgentConnections) get(id string) (*agentConn, bool) {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	c, ok := ac.conns[id]
	return c, ok
}

func (ac *AgentConnections) isConnected(id string) bool {
	_, ok := ac.get(id)
	return ok
}

// ── Tunnel protocol ───────────────────────────────────────────────────────────

type tunnelRequest struct {
	Type   string `json:"type"` // "request"
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query,omitempty"`
	Body   string `json:"body,omitempty"`
}

type tunnelResponse struct {
	Type    string            `json:"type"` // "response"
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers,omitempty"`
}

type tunnelHeartbeat struct {
	Type          string `json:"type"` // "heartbeat"
	Hostname      string `json:"hostname"`
	DockerVersion string `json:"dockerVersion"`
}

type tunnelEnvelope struct {
	Type string `json:"type"`
}

// Exec tunnel message types.
type tunnelExecStart struct {
	Type      string `json:"type"`      // "exec_start"
	ExecID    string `json:"execId"`
	SessionID string `json:"sessionId"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

type tunnelExecInput struct {
	Type   string `json:"type"`   // "exec_input"
	ExecID string `json:"execId"`
	Data   string `json:"data"`   // base64-encoded
}

type tunnelExecResize struct {
	Type   string `json:"type"`   // "exec_resize"
	ExecID string `json:"execId"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

type tunnelExecClose struct {
	Type   string `json:"type"`   // "exec_close"
	ExecID string `json:"execId"`
}

// ── REST: list hosts ──────────────────────────────────────────────────────────

// HostInfo is the API representation of a Docker host (local or remote agent).
type HostInfo struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Type          string    `json:"type"` // "local" | "agent"
	Status        string    `json:"status"`
	Hostname      string    `json:"hostname,omitempty"`
	DockerVersion string    `json:"dockerVersion,omitempty"`
	LastSeen      time.Time `json:"lastSeen,omitempty"`
	CreatedAt     time.Time `json:"createdAt,omitempty"`
}

// ListHosts handles GET /agents/hosts — returns local Docker + all registered agents.
func (h *Handler) ListHosts(w http.ResponseWriter, r *http.Request) {
	hosts := []HostInfo{
		{ID: "local", Name: "Local", Type: "local", Status: "connected"},
	}
	if h.agentStore != nil {
		agents, err := h.agentStore.ListAgents(r.Context())
		if err != nil {
			slog.Error("list agents", "error", err)
		} else {
			for _, a := range agents {
				status := "disconnected"
				if h.agentConns.isConnected(a.ID) {
					status = "connected"
				}
				hosts = append(hosts, HostInfo{
					ID:            a.ID,
					Name:          a.Name,
					Type:          "agent",
					Status:        status,
					Hostname:      a.Hostname,
					DockerVersion: a.DockerVersion,
					LastSeen:      a.LastSeen,
					CreatedAt:     a.CreatedAt,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, hosts)
}

// ── REST: create / delete agent ───────────────────────────────────────────────

type createAgentReq struct {
	Name string `json:"name"`
}

type createAgentResp struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Token        string    `json:"token"`
	DockerRunCmd string    `json:"dockerRunCmd"`
	CreatedAt    time.Time `json:"createdAt"`
}

// CreateAgent handles POST /agents.
func (h *Handler) CreateAgent(w http.ResponseWriter, r *http.Request) {
	if h.agentStore == nil {
		writeError(w, http.StatusServiceUnavailable, "agent store not configured (set REDIS_ADDR)")
		return
	}
	var req createAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	plaintext, hash, err := store.GenerateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	agent, err := h.agentStore.CreateAgent(r.Context(), strings.TrimSpace(req.Name), hash)
	if err != nil {
		slog.Error("create agent", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create agent")
		return
	}
	serverURL := h.agentServerURL
	cmd := fmt.Sprintf(
		"docker run -d --name docker-agent-%s --restart=unless-stopped \\\n"+
			"  -v /var/run/docker.sock:/var/run/docker.sock \\\n"+
			"  -e AGENT_SERVER_URL=\"%s\" \\\n"+
			"  -e AGENT_TOKEN=\"%s\" \\\n"+
			"  -e AGENT_NAME=\"%s\" \\\n"+
			"  -e AGENT_INSECURE_SKIP_VERIFY=\"true\" \\\n"+
			"  ghcr.io/valueiron/docker-agent:latest",
		agent.ID[:8], serverURL, plaintext, agent.Name,
	)
	writeJSON(w, http.StatusCreated, createAgentResp{
		ID:           agent.ID,
		Name:         agent.Name,
		Token:        plaintext,
		DockerRunCmd: cmd,
		CreatedAt:    agent.CreatedAt,
	})
}

// DeleteAgent handles DELETE /agents/{id}.
func (h *Handler) DeleteAgent(w http.ResponseWriter, r *http.Request) {
	if h.agentStore == nil {
		writeError(w, http.StatusServiceUnavailable, "agent store not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if conn, ok := h.agentConns.get(id); ok {
		close(conn.done)
		h.agentConns.remove(id)
	}
	if err := h.agentStore.DeleteAgent(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── WebSocket gateway (agents connect here) ───────────────────────────────────

// AgentGatewayWS handles GET /agents/ws — persistent WebSocket for remote agents.
// Agents authenticate with: Authorization: Bearer <token>  or  ?token=<token>
func (h *Handler) AgentGatewayWS(w http.ResponseWriter, r *http.Request) {
	if h.agentStore == nil {
		writeError(w, http.StatusServiceUnavailable, "agent store not configured")
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	hash := store.HashToken(token)
	agent, err := h.agentStore.FindByTokenHash(r.Context(), hash)
	if err != nil || agent == nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("agent ws upgrade", "error", err)
		return
	}

	conn := &agentConn{
		ws:        ws,
		pending:   make(map[string]chan tunnelResponse),
		execChans: make(map[string]chan execMsg),
		done:      make(chan struct{}),
	}
	h.agentConns.add(agent.ID, conn)
	slog.Info("agent connected", "id", agent.ID, "name", agent.Name)
	_ = h.agentStore.UpdateAgentStatus(r.Context(), agent.ID, "connected", "", "")

	defer func() {
		_ = ws.Close()
		h.agentConns.remove(agent.ID)
		_ = h.agentStore.UpdateAgentStatus(
			context.Background(), agent.ID, "disconnected",
			conn.dockerVersion, conn.hostname,
		)
		slog.Info("agent disconnected", "id", agent.ID)
	}()

	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var env tunnelEnvelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		switch env.Type {
		case "heartbeat":
			var hb tunnelHeartbeat
			if json.Unmarshal(raw, &hb) == nil {
				conn.hostname = hb.Hostname
				conn.dockerVersion = hb.DockerVersion
				_ = h.agentStore.UpdateAgentStatus(
					r.Context(), agent.ID, "connected",
					hb.DockerVersion, hb.Hostname,
				)
			}
		case "response":
			var resp tunnelResponse
			if json.Unmarshal(raw, &resp) == nil {
				conn.pendingMu.Lock()
				ch, ok := conn.pending[resp.ID]
				if ok {
					delete(conn.pending, resp.ID)
				}
				conn.pendingMu.Unlock()
				if ok {
					select {
					case ch <- resp:
					default:
					}
				}
			}
		case "exec_output", "exec_close":
			var msg struct {
				ExecID string `json:"execId"`
				Data   string `json:"data,omitempty"`
			}
			if json.Unmarshal(raw, &msg) == nil {
				conn.execChansMu.Lock()
				ch, ok := conn.execChans[msg.ExecID]
				conn.execChansMu.Unlock()
				if ok {
					select {
					case ch <- execMsg{Type: env.Type, Data: msg.Data}:
					default:
					}
				}
			}
		}
	}
}

// ── Agent proxy ───────────────────────────────────────────────────────────────

// AgentProxy handles any method on /agents/{id}/proxy/{path...}.
// It tunnels the HTTP request to the connected agent and returns its response.
func (h *Handler) AgentProxy(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	agentID := vars["id"]
	path := "/" + vars["path"]

	conn, ok := h.agentConns.get(agentID)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "agent not connected")
		return
	}

	var bodyStr string
	if r.Body != nil {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		bodyStr = buf.String()
	}

	reqID := uuid.New().String()
	req := tunnelRequest{
		Type:   "request",
		ID:     reqID,
		Method: r.Method,
		Path:   path,
		Query:  r.URL.RawQuery,
		Body:   bodyStr,
	}

	ch := make(chan tunnelResponse, 1)
	conn.pendingMu.Lock()
	conn.pending[reqID] = ch
	conn.pendingMu.Unlock()

	if err := conn.sendJSON(req); err != nil {
		conn.pendingMu.Lock()
		delete(conn.pending, reqID)
		conn.pendingMu.Unlock()
		writeError(w, http.StatusBadGateway, "failed to send request to agent")
		return
	}

	select {
	case resp := <-ch:
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write([]byte(resp.Body))
	case <-time.After(30 * time.Second):
		conn.pendingMu.Lock()
		delete(conn.pending, reqID)
		conn.pendingMu.Unlock()
		writeError(w, http.StatusGatewayTimeout, "agent response timeout")
	}
}

// ── Agent exec WebSocket ──────────────────────────────────────────────────────

// AgentExecWS handles GET /agents/{id}/exec/ws?sessionId=<id>[&cols=N&rows=N].
// It upgrades the browser connection to WebSocket and multiplexes a Docker exec
// session through the agent's existing persistent WebSocket.
func (h *Handler) AgentExecWS(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["id"]
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		http.Error(w, `{"error":"sessionId required"}`, http.StatusBadRequest)
		return
	}

	conn, ok := h.agentConns.get(agentID)
	if !ok {
		http.Error(w, `{"error":"agent not connected"}`, http.StatusServiceUnavailable)
		return
	}

	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("agent exec ws upgrade", "error", err)
		return
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Time{})  //nolint:errcheck
	ws.SetWriteDeadline(time.Time{}) //nolint:errcheck

	var wsMu sync.Mutex
	writeMsg := func(msgType int, data []byte) {
		wsMu.Lock()
		defer wsMu.Unlock()
		ws.WriteMessage(msgType, data) //nolint:errcheck
	}
	sendJSON := func(v any) {
		data, _ := json.Marshal(v)
		writeMsg(websocket.TextMessage, data)
	}

	// Parse optional initial terminal size.
	var cols, rows uint16 = 80, 24
	if c := r.URL.Query().Get("cols"); c != "" {
		if n, err := fmt.Sscanf(c, "%d", &cols); n == 0 || err != nil {
			cols = 80
		}
	}
	if rr := r.URL.Query().Get("rows"); rr != "" {
		if n, err := fmt.Sscanf(rr, "%d", &rows); n == 0 || err != nil {
			rows = 24
		}
	}

	execID := uuid.New().String()
	execCh := make(chan execMsg, 128)

	conn.execChansMu.Lock()
	conn.execChans[execID] = execCh
	conn.execChansMu.Unlock()
	defer func() {
		conn.execChansMu.Lock()
		delete(conn.execChans, execID)
		conn.execChansMu.Unlock()
	}()

	// Ask the agent to start the exec session.
	if err := conn.sendJSON(tunnelExecStart{
		Type:      "exec_start",
		ExecID:    execID,
		SessionID: sessionID,
		Cols:      cols,
		Rows:      rows,
	}); err != nil {
		slog.Error("exec_start send failed", "error", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Goroutine: agent exec output → browser.
	go func() {
		for {
			select {
			case msg, ok := <-execCh:
				if !ok {
					return
				}
				switch msg.Type {
				case "exec_output":
					decoded, err := base64.StdEncoding.DecodeString(msg.Data)
					if err == nil {
						writeMsg(websocket.BinaryMessage, decoded)
					}
				case "exec_close":
					sendJSON(map[string]string{"type": "disconnected"})
					wsMu.Lock()
					ws.Close()
					wsMu.Unlock()
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	sendJSON(map[string]string{"type": "connected", "sessionId": sessionID})

	// Main loop: browser → agent.
	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			break
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
					conn.sendJSON(tunnelExecResize{ //nolint:errcheck
						Type:   "exec_resize",
						ExecID: execID,
						Cols:   msg.Cols,
						Rows:   msg.Rows,
					})
					continue
				case "ping":
					sendJSON(map[string]string{"type": "pong"})
					continue
				case "inject":
					if msg.Data != "" {
						conn.sendJSON(tunnelExecInput{ //nolint:errcheck
							Type:   "exec_input",
							ExecID: execID,
							Data:   base64.StdEncoding.EncodeToString([]byte(msg.Data)),
						})
					}
					continue
				}
			}
			conn.sendJSON(tunnelExecInput{ //nolint:errcheck
				Type:   "exec_input",
				ExecID: execID,
				Data:   base64.StdEncoding.EncodeToString(data),
			})
		} else {
			conn.sendJSON(tunnelExecInput{ //nolint:errcheck
				Type:   "exec_input",
				ExecID: execID,
				Data:   base64.StdEncoding.EncodeToString(data),
			})
		}
	}

	conn.sendJSON(tunnelExecClose{Type: "exec_close", ExecID: execID}) //nolint:errcheck
}
