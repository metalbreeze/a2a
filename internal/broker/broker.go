package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Broker bundles the registry store, inbox hub, and HTTP handlers.
type Broker struct {
	Store    *Store
	Hub      *InboxHub
	Logger   *zap.Logger
	PublicURL string // e.g. "http://www.cybertron.studio:6321"

	waitersMu sync.Mutex
	waiters   map[string][]chan *Result // task_id -> waiters
}

// New creates a broker with the given store and logger.
func New(store *Store, hub *InboxHub, logger *zap.Logger, publicURL string) *Broker {
	return &Broker{
		Store:     store,
		Hub:       hub,
		Logger:    logger,
		PublicURL: strings.TrimRight(publicURL, "/"),
		waiters:   make(map[string][]chan *Result),
	}
}

// Mount attaches every broker route to the given Gin engine.
func (b *Broker) Mount(r *gin.Engine) {
	// Registry
	r.POST("/registry/agents", b.handleRegister)
	r.GET("/registry/agents", b.handleListAgents)
	r.PUT("/registry/agents/:id", b.authMiddleware, b.handleUpdate)

	// Per-agent inbox (B -> broker)
	r.GET("/agents/:id/inbox/stream", b.authMiddleware, b.handleInboxStream)
	r.GET("/agents/:id/inbox", b.authMiddleware, b.handleInboxPoll)
	r.POST("/agents/:id/tasks/:tid/result", b.authMiddleware, b.handleResultPost)

	// Per-agent A2A (A -> broker -> B)
	r.POST("/agents/:id/a2a", b.handleAgentA2A)
	r.GET("/agents/:id/.well-known/agent-card.json", b.handleAgentCard)

	// Human-facing landing + SDK download + stats
	r.GET("/", b.serveLanding)
	r.GET("/download/a2a-broker-sdk.zip", b.serveSDKZip)
	r.GET("/sdk", func(c *gin.Context) { c.Redirect(http.StatusFound, b.prefixPath(c)+"/sdk/") })
	r.GET("/sdk/*path", b.serveSDKFile)
	r.GET("/stats", b.handleStatsJSON)
	r.GET("/agents", b.handleAgentsPage)
	r.GET("/services", b.handleServicesPage)
	r.GET("/registry/services", b.handleListServicesJSON)
}

// ---------- Registry handlers ----------

type registerReq struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Card        json.RawMessage `json:"card"` // optional full AgentCard JSON; overrides Name/Description/Version
	Mode        string          `json:"mode"` // realtime|offline|scheduled (default realtime)
	Schedule    string          `json:"schedule"`
}

type registerResp struct {
	AgentID string `json:"agent_id"`
	Token   string `json:"token"`
	A2AURL  string `json:"a2a_url"`
	CardURL string `json:"card_url"`
}

func (b *Broker) handleRegister(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "realtime"
	}

	card := req.Card
	if len(card) == 0 {
		// Build a minimal card from the scalar fields.
		stub := map[string]any{
			"name":            req.Name,
			"description":     req.Description,
			"version":         req.Version,
			"protocolVersion": "0.3.0",
			"capabilities": map[string]any{
				"streaming":              true,
				"pushNotifications":      false,
				"stateTransitionHistory": false,
			},
			"defaultInputModes":  []string{"text/plain"},
			"defaultOutputModes": []string{"text/plain"},
			"skills":             []any{},
		}
		card, _ = json.Marshal(stub)
	}

	a, token, err := b.Store.CreateAgent(req.Name, mode, req.Schedule, card)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, registerResp{
		AgentID: a.ID,
		Token:   token,
		A2AURL:  fmt.Sprintf("%s/agents/%s/a2a", b.PublicURL, a.ID),
		CardURL: fmt.Sprintf("%s/agents/%s/.well-known/agent-card.json", b.PublicURL, a.ID),
	})
}

func (b *Broker) handleListAgents(c *gin.Context) {
	onlineOnly := c.Query("available") == "now"
	skill := strings.ToLower(c.Query("skill"))

	agents, err := b.Store.ListAgents(onlineOnly)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(agents))
	for _, a := range agents {
		if skill != "" && !cardHasSkill(a.CardJSON, skill) {
			continue
		}
		out = append(out, gin.H{
			"agent_id":  a.ID,
			"name":      a.Name,
			"card":      json.RawMessage(a.CardJSON),
			"mode":      a.Mode,
			"schedule":  a.Schedule,
			"online":    a.OnlineRT,
			"last_seen": a.LastSeen,
			"a2a_url":   fmt.Sprintf("%s/agents/%s/a2a", b.PublicURL, a.ID),
		})
	}
	c.JSON(http.StatusOK, out)
}

// cardHasSkill does a loose substring match against skill names / tags / id.
func cardHasSkill(card json.RawMessage, needle string) bool {
	var c struct {
		Skills []struct {
			ID   string   `json:"id"`
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(card, &c); err != nil {
		return false
	}
	for _, s := range c.Skills {
		if strings.Contains(strings.ToLower(s.ID), needle) ||
			strings.Contains(strings.ToLower(s.Name), needle) {
			return true
		}
		for _, t := range s.Tags {
			if strings.Contains(strings.ToLower(t), needle) {
				return true
			}
		}
	}
	return false
}

func (b *Broker) handleUpdate(c *gin.Context) {
	id := c.Param("id")
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	card := req.Card
	if len(card) == 0 {
		// keep existing
		a, err := b.Store.GetAgent(id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
			return
		}
		card = a.CardJSON
	}
	mode := req.Mode
	if mode == "" {
		mode = "realtime"
	}
	if err := b.Store.UpdateAgent(id, card, mode, req.Schedule); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------- Auth ----------

func (b *Broker) authMiddleware(c *gin.Context) {
	id := c.Param("id")
	auth := c.GetHeader("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" || token == auth {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing Bearer token"})
		return
	}
	ok, err := b.Store.VerifyToken(id, token)
	if err != nil || !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}
	c.Next()
}

// ---------- Agent inbox handlers (B side) ----------

func (b *Broker) handleInboxStream(c *gin.Context) {
	id := c.Param("id")

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	sub := b.Hub.Subscribe(id)
	if err := b.Store.SetOnline(id, true); err != nil {
		b.Logger.Warn("set online failed", zap.Error(err))
	}
	defer func() {
		b.Hub.Unsubscribe(id, sub)
		_ = b.Store.SetOnline(id, false)
	}()

	// Replay any pending tasks on connect so we don't lose messages that arrived
	// while B was offline.
	if pending, err := b.Store.ListPending(id); err == nil {
		for _, e := range pending {
			writeSSE(c, "task", InboxMessage{TaskID: e.TaskID, ContextID: e.ContextID, Payload: e.Payload})
			_ = b.Store.MarkDelivered(e.TaskID)
		}
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	ctx := c.Request.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub:
			if !ok {
				return
			}
			writeSSE(c, "task", msg)
			_ = b.Store.MarkDelivered(msg.TaskID)
		case <-ticker.C:
			// keep-alive comment
			_, _ = io.WriteString(c.Writer, ": keepalive\n\n")
			c.Writer.Flush()
		}
	}
}

func writeSSE(c *gin.Context, event string, data any) {
	raw, _ := json.Marshal(data)
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, raw)
	c.Writer.Flush()
}

func (b *Broker) handleInboxPoll(c *gin.Context) {
	id := c.Param("id")
	pending, err := b.Store.ListPending(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]InboxMessage, 0, len(pending))
	for _, e := range pending {
		out = append(out, InboxMessage{TaskID: e.TaskID, ContextID: e.ContextID, Payload: e.Payload})
		_ = b.Store.MarkDelivered(e.TaskID)
	}
	c.JSON(http.StatusOK, out)
}

type resultReq struct {
	State   string          `json:"state"` // TASK_STATE_COMPLETED | TASK_STATE_FAILED
	Message json.RawMessage `json:"message,omitempty"`
	Artifacts json.RawMessage `json:"artifacts,omitempty"`
}

func (b *Broker) handleResultPost(c *gin.Context) {
	agentID := c.Param("id")
	taskID := c.Param("tid")
	var req resultReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.State == "" {
		req.State = "TASK_STATE_COMPLETED"
	}
	body, _ := json.Marshal(req)
	if err := b.Store.SaveResult(taskID, agentID, req.State, body); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Wake any waiters (A side polling via tasks/get).
	r, _ := b.Store.GetResult(taskID)
	b.notifyWaiters(taskID, r)

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------- A -> Broker A2A handlers ----------

// handleAgentCard proxies the stored card JSON.
func (b *Broker) handleAgentCard(c *gin.Context) {
	id := c.Param("id")
	a, err := b.Store.GetAgent(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		return
	}
	c.Data(http.StatusOK, "application/json", a.CardJSON)
}

// jsonrpc request/response envelopes
type jsonRPCReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCErr     `json:"error,omitempty"`
}

type jsonRPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (b *Broker) handleAgentA2A(c *gin.Context) {
	agentID := c.Param("id")
	if _, err := b.Store.GetAgent(agentID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		return
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var req jsonRPCReq
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json-rpc"})
		return
	}

	switch req.Method {
	case "message/send":
		b.handleMessageSend(c, agentID, req)
	case "tasks/get":
		b.handleTasksGet(c, agentID, req)
	case "message/stream":
		b.handleMessageStream(c, agentID, req)
	default:
		c.JSON(http.StatusOK, jsonRPCResp{
			JSONRPC: "2.0", ID: req.ID,
			Error: &jsonRPCErr{Code: -32601, Message: "method not supported: " + req.Method},
		})
	}
}

type messageSendParams struct {
	Message struct {
		MessageID string          `json:"messageId"`
		Role      string          `json:"role"`
		Parts     json.RawMessage `json:"parts"`
		ContextID string          `json:"contextId,omitempty"`
		TaskID    string          `json:"taskId,omitempty"`
	} `json:"message"`
	Configuration json.RawMessage `json:"configuration,omitempty"`
}

func (b *Broker) handleMessageSend(c *gin.Context, agentID string, req jsonRPCReq) {
	var p messageSendParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		c.JSON(http.StatusOK, jsonRPCResp{
			JSONRPC: "2.0", ID: req.ID,
			Error: &jsonRPCErr{Code: -32602, Message: "invalid params"},
		})
		return
	}

	taskID := uuid.NewString()
	contextID := p.Message.ContextID
	if contextID == "" {
		contextID = uuid.NewString()
	}

	payload, _ := json.Marshal(p)
	if err := b.Store.EnqueueTask(taskID, agentID, contextID, payload); err != nil {
		c.JSON(http.StatusOK, jsonRPCResp{
			JSONRPC: "2.0", ID: req.ID,
			Error: &jsonRPCErr{Code: -32603, Message: err.Error()},
		})
		return
	}

	// Try to deliver realtime; if offline, task just waits in the inbox.
	delivered := b.Hub.Publish(agentID, &InboxMessage{
		TaskID: taskID, ContextID: contextID, Payload: payload,
	})
	if delivered {
		_ = b.Store.MarkDelivered(taskID)
	}

	// Return a minimal Task object in SUBMITTED / WORKING state.
	state := "TASK_STATE_SUBMITTED"
	if delivered {
		state = "TASK_STATE_WORKING"
	}
	resultTask := gin.H{
		"id":        taskID,
		"contextId": contextID,
		"status":    gin.H{"state": state},
		"history":   []any{p.Message},
	}
	c.JSON(http.StatusOK, jsonRPCResp{JSONRPC: "2.0", ID: req.ID, Result: resultTask})
}

type taskQueryParams struct {
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength,omitempty"`
}

func (b *Broker) handleTasksGet(c *gin.Context, agentID string, req jsonRPCReq) {
	var p taskQueryParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		c.JSON(http.StatusOK, jsonRPCResp{
			JSONRPC: "2.0", ID: req.ID,
			Error: &jsonRPCErr{Code: -32602, Message: "invalid params"},
		})
		return
	}

	// First check results.
	if r, err := b.Store.GetResult(p.ID); err == nil {
		task := buildTaskFromResult(p.ID, r)
		c.JSON(http.StatusOK, jsonRPCResp{JSONRPC: "2.0", ID: req.ID, Result: task})
		return
	}
	// Fall back to inbox (still in flight).
	if e, err := b.Store.GetInboxEntry(p.ID); err == nil {
		state := "TASK_STATE_SUBMITTED"
		if e.State == "delivered" {
			state = "TASK_STATE_WORKING"
		}
		c.JSON(http.StatusOK, jsonRPCResp{
			JSONRPC: "2.0", ID: req.ID,
			Result: gin.H{
				"id":        e.TaskID,
				"contextId": e.ContextID,
				"status":    gin.H{"state": state},
			},
		})
		return
	}
	c.JSON(http.StatusOK, jsonRPCResp{
		JSONRPC: "2.0", ID: req.ID,
		Error: &jsonRPCErr{Code: -32001, Message: "task not found"},
	})
}

func buildTaskFromResult(taskID string, r *Result) gin.H {
	var body struct {
		State     string          `json:"state"`
		Message   json.RawMessage `json:"message,omitempty"`
		Artifacts json.RawMessage `json:"artifacts,omitempty"`
	}
	_ = json.Unmarshal(r.ResultJSON, &body)
	task := gin.H{
		"id":        taskID,
		"contextId": "",
		"status": gin.H{
			"state":   body.State,
			"message": json.RawMessage(body.Message),
		},
	}
	if len(body.Artifacts) > 0 {
		var arts any
		_ = json.Unmarshal(body.Artifacts, &arts)
		task["artifacts"] = arts
	}
	return task
}

// handleMessageStream holds an SSE connection to A and pipes B's result
// through once the result arrives. If B isn't online we still accept the
// request and relay when B eventually completes the task.
func (b *Broker) handleMessageStream(c *gin.Context, agentID string, req jsonRPCReq) {
	var p messageSendParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid params"})
		return
	}

	taskID := uuid.NewString()
	contextID := p.Message.ContextID
	if contextID == "" {
		contextID = uuid.NewString()
	}

	payload, _ := json.Marshal(p)
	if err := b.Store.EnqueueTask(taskID, agentID, contextID, payload); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Flush()

	// Register waiter before publishing so we don't miss an immediate result.
	waiter := b.registerWaiter(taskID)
	defer b.unregisterWaiter(taskID, waiter)

	// Kick off delivery to B.
	b.Hub.Publish(agentID, &InboxMessage{TaskID: taskID, ContextID: contextID, Payload: payload})
	_ = b.Store.MarkDelivered(taskID)

	// Initial working event.
	fmt.Fprintf(c.Writer, "data: %s\n\n", mustJSON(jsonRPCResp{
		JSONRPC: "2.0", ID: req.ID,
		Result: gin.H{"taskId": taskID, "contextId": contextID, "final": false,
			"status": gin.H{"state": "TASK_STATE_WORKING"}},
	}))
	c.Writer.Flush()

	// Wait for result or client disconnect.
	ctx := c.Request.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case r := <-waiter:
			task := buildTaskFromResult(taskID, r)
			task["taskId"] = taskID
			task["contextId"] = contextID
			task["final"] = true
			fmt.Fprintf(c.Writer, "data: %s\n\n", mustJSON(jsonRPCResp{
				JSONRPC: "2.0", ID: req.ID, Result: task,
			}))
			c.Writer.Flush()
			fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
			c.Writer.Flush()
			return
		case <-ticker.C:
			_, _ = io.WriteString(c.Writer, ": keepalive\n\n")
			c.Writer.Flush()
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------- Waiter plumbing (result fan-out to streaming subscribers) ----------

func (b *Broker) registerWaiter(taskID string) chan *Result {
	ch := make(chan *Result, 1)
	b.waitersMu.Lock()
	b.waiters[taskID] = append(b.waiters[taskID], ch)
	b.waitersMu.Unlock()
	return ch
}

func (b *Broker) unregisterWaiter(taskID string, ch chan *Result) {
	b.waitersMu.Lock()
	defer b.waitersMu.Unlock()
	w := b.waiters[taskID]
	for i, c := range w {
		if c == ch {
			b.waiters[taskID] = append(w[:i], w[i+1:]...)
			break
		}
	}
	if len(b.waiters[taskID]) == 0 {
		delete(b.waiters, taskID)
	}
}

func (b *Broker) notifyWaiters(taskID string, r *Result) {
	b.waitersMu.Lock()
	ws := b.waiters[taskID]
	delete(b.waiters, taskID)
	b.waitersMu.Unlock()
	for _, ch := range ws {
		select {
		case ch <- r:
		default:
		}
	}
}
