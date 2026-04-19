// Package http provides an HTTP webhook channel for picoclaw.
// It listens on POST /webhook/http, accepts a JSON message body,
// returns 202 Accepted immediately, and processes the message through
// an isolated agent session in the background.
//
// The channel also exposes a status endpoint:
//
//	GET /webhook/http/status/<request_id>
//
// The channel is inbound-only: outbound Send is a no-op. It is intended
// for programmatic invocation by automation platforms (n8n, Make, cron jobs)
// rather than human conversation.
package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"strings"
	"sync"
	"time"

	"github.com/teren-papercutlabs/pclaw/pkg/bus"
	"github.com/teren-papercutlabs/pclaw/pkg/channels"
	"github.com/teren-papercutlabs/pclaw/pkg/config"
	"github.com/teren-papercutlabs/pclaw/pkg/logger"
)

const (
	channelName   = "http"
	webhookBase   = "/webhook/http"
	resultTTL     = 1 * time.Hour
	resultCleanup = 5 * time.Minute
)

// webhookRequest is the JSON body accepted on POST /webhook/http.
type webhookRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"` // optional — for session continuity
}

// webhookResponse is returned immediately on 202 Accepted.
type webhookResponse struct {
	OK        bool   `json:"ok"`
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

// statusResponse is returned on GET /webhook/http/status/<request_id>.
type statusResponse struct {
	OK        bool   `json:"ok"`
	RequestID string `json:"request_id"`
	Status    string `json:"status"` // "pending", "done", "error"
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}

// resultEntry stores the outcome of a completed request.
type resultEntry struct {
	status    string // "pending", "done", or "error"
	result    string
	errMsg    string
	createdAt time.Time
}

// Channel is the HTTP webhook channel. It implements channels.Channel and
// channels.WebhookHandler so the channel manager auto-registers it on the
// shared HTTP server.
type Channel struct {
	*channels.BaseChannel
	cfg      *config.HTTPSettings
	bus      *bus.MessageBus
	executor channels.AgentExecutor // injected by manager after factory creation; may be nil initially

	mu sync.RWMutex

	// in-memory result store with TTL-based cleanup
	results   map[string]*resultEntry
	resultsMu sync.RWMutex
	stopClean chan struct{}
	stopOnce  sync.Once
}

// NewHTTPChannel creates a new HTTP webhook channel.
// executor may be nil at construction time and injected later via SetExecutor.
func NewHTTPChannel(cfg *config.HTTPSettings, b *bus.MessageBus) (*Channel, error) {
	base := channels.NewBaseChannel(
		channelName,
		cfg,
		b,
		[]string{"*"}, // inbound-only; caller authentication is out of scope (localhost-only by default)
	)
	ch := &Channel{
		BaseChannel: base,
		cfg:         cfg,
		bus:         b,
		results:     make(map[string]*resultEntry),
		stopClean:   make(chan struct{}),
	}
	return ch, nil
}

// SetExecutor injects the agent executor. Called by the channel manager
// after factory creation, mirroring the SetMediaStore injection pattern.
// Implements channels.AgentExecutorSetter.
func (c *Channel) SetExecutor(exec channels.AgentExecutor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.executor = exec
}

// --- channels.Channel interface ---

// Start begins the TTL cleanup goroutine and marks the channel running.
func (c *Channel) Start(_ context.Context) error {
	c.SetRunning(true)
	go c.runCleanup()

	logger.InfoCF(channelName, "HTTP webhook channel started", map[string]any{
		"path": webhookBase,
	})
	return nil
}

// Stop marks the channel stopped and signals the TTL cleanup goroutine to exit.
func (c *Channel) Stop(_ context.Context) error {
	c.SetRunning(false)
	c.stopOnce.Do(func() {
		close(c.stopClean)
	})
	return nil
}

// Send is a no-op: the HTTP webhook channel is inbound-only.
func (c *Channel) Send(_ context.Context, _ bus.OutboundMessage) ([]string, error) {
	return nil, nil
}

// --- channels.WebhookHandler interface ---

// WebhookPath returns the path prefix for this channel.
// Trailing slash enables http.ServeMux prefix matching so that both
// POST /webhook/http and GET /webhook/http/status/<id> are routed here.
func (c *Channel) WebhookPath() string { return webhookBase + "/" }

// ServeHTTP dispatches to POST (process) or GET/status handlers.
func (c *Channel) ServeHTTP(w nethttp.ResponseWriter, r *nethttp.Request) {
	path := r.URL.Path

	switch {
	case r.Method == nethttp.MethodPost && (path == webhookBase || path == webhookBase+"/"):
		c.handlePost(w, r)
	case r.Method == nethttp.MethodGet && strings.HasPrefix(path, webhookBase+"/status/"):
		reqID := strings.TrimPrefix(path, webhookBase+"/status/")
		if reqID == "" {
			nethttp.Error(w, "request_id required", nethttp.StatusBadRequest)
			return
		}
		c.handleStatus(w, reqID)
	default:
		nethttp.Error(w, "not found", nethttp.StatusNotFound)
	}
}

// handlePost accepts a webhook request and spawns an isolated agent session.
func (c *Channel) handlePost(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !c.IsRunning() {
		nethttp.Error(w, "channel not running", nethttp.StatusServiceUnavailable)
		return
	}

	var req webhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		nethttp.Error(w, fmt.Sprintf("invalid JSON: %v", err), nethttp.StatusBadRequest)
		return
	}

	if req.Message == "" {
		nethttp.Error(w, "message is required", nethttp.StatusBadRequest)
		return
	}

	reqID := newRequestID()
	sessionKey := fmt.Sprintf("http-%s", reqID)
	if req.SessionID != "" {
		// Use caller-provided session ID for continuity across requests
		sessionKey = fmt.Sprintf("http-%s", req.SessionID)
	}

	// Register the request as pending before spawning the goroutine
	c.resultsMu.Lock()
	c.results[reqID] = &resultEntry{status: "pending", createdAt: time.Now()}
	c.resultsMu.Unlock()

	go c.processRequest(reqID, req.Message, sessionKey)

	resp := webhookResponse{
		OK:        true,
		RequestID: reqID,
		Status:    "accepted",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(nethttp.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)

	logger.InfoCF(channelName, "Webhook request accepted", map[string]any{
		"request_id":  reqID,
		"session_key": sessionKey,
		"message_len": len(req.Message),
	})
}

// handleStatus returns the current status of a previously accepted request.
func (c *Channel) handleStatus(w nethttp.ResponseWriter, reqID string) {
	c.resultsMu.RLock()
	entry, ok := c.results[reqID]
	c.resultsMu.RUnlock()

	if !ok {
		resp := statusResponse{
			OK:        false,
			RequestID: reqID,
			Status:    "not_found",
			Error:     "request ID not found",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(nethttp.StatusNotFound)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	resp := statusResponse{
		OK:        true,
		RequestID: reqID,
		Status:    entry.status,
		Result:    entry.result,
		Error:     entry.errMsg,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// processRequest runs the message through the agent and stores the result.
// Runs in a goroutine; does not write to the HTTP response.
func (c *Channel) processRequest(reqID, message, sessionKey string) {
	c.mu.RLock()
	exec := c.executor
	c.mu.RUnlock()

	if exec == nil {
		logger.ErrorCF(channelName, "No executor set; cannot process request", map[string]any{
			"request_id": reqID,
		})
		c.setResult(reqID, "", "agent executor not configured")
		return
	}

	ctx := context.Background()
	result, err := exec.ProcessDirectWithChannel(ctx, message, sessionKey, channelName, reqID)

	if err != nil {
		logger.ErrorCF(channelName, "Agent processing failed", map[string]any{
			"request_id": reqID,
			"error":      err.Error(),
		})
		c.setResult(reqID, "", err.Error())
		return
	}

	logger.InfoCF(channelName, "Agent processing complete", map[string]any{
		"request_id": reqID,
		"result_len": len(result),
	})
	c.setResult(reqID, result, "")
}

// setResult stores the final outcome of a request.
func (c *Channel) setResult(reqID, result, errMsg string) {
	status := "done"
	if errMsg != "" {
		status = "error"
	}
	c.resultsMu.Lock()
	c.results[reqID] = &resultEntry{
		status:    status,
		result:    result,
		errMsg:    errMsg,
		createdAt: time.Now(),
	}
	c.resultsMu.Unlock()
}

// runCleanup periodically removes result entries that have exceeded their TTL.
func (c *Channel) runCleanup() {
	ticker := time.NewTicker(resultCleanup)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopClean:
			return
		case now := <-ticker.C:
			c.resultsMu.Lock()
			for id, entry := range c.results {
				if now.Sub(entry.createdAt) > resultTTL {
					delete(c.results, id)
				}
			}
			c.resultsMu.Unlock()
		}
	}
}

// newRequestID generates a random hex ID for a request.
func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
