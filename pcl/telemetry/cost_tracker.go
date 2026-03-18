// PicoClaw PCL downstream — cost tracking telemetry.
// All code in this file is PcL-only and is not part of upstream picoclaw.
// Downstream: permanent

package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ModelPrice holds per-million-token pricing for a model.
type ModelPrice struct {
	Input  float64 `json:"input"`  // USD per 1M prompt tokens
	Output float64 `json:"output"` // USD per 1M completion tokens
}

// ToolResult records the outcome of a single tool call within a turn.
type ToolResult struct {
	Name    string `json:"name"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// CostEntry is one JSONL line written to the telemetry log.
type CostEntry struct {
	Ts               string       `json:"ts"`
	AgentID          string       `json:"agent_id"`
	Model            string       `json:"model"`
	SessionKey       string       `json:"session_key"`
	PromptTokens     int          `json:"prompt_tokens"`
	CompletionTokens int          `json:"completion_tokens"`
	TotalTokens      int          `json:"total_tokens"`
	CostUSD          float64      `json:"cost_usd"`
	ToolResults      []ToolResult `json:"tool_results"`
	Iterations       int          `json:"iterations"`
	DurationMs       int64        `json:"duration_ms"`
}

// CostTracker appends CostEntry records to a JSONL file.
type CostTracker struct {
	logPath string
	prices  map[string]ModelPrice
	mu      sync.Mutex
}

// NewCostTracker creates a CostTracker that writes to logPath using the given price table.
// logPath is created (including parent directories) on the first write.
func NewCostTracker(logPath string, prices map[string]ModelPrice) *CostTracker {
	return &CostTracker{
		logPath: logPath,
		prices:  prices,
	}
}

// computeCost calculates the USD cost for a response given token counts and model name.
// Returns 0 if the model is not in the price table.
func (ct *CostTracker) computeCost(model string, promptTokens, completionTokens int) float64 {
	price, ok := ct.prices[model]
	if !ok {
		return 0
	}
	return float64(promptTokens)*price.Input/1_000_000 +
		float64(completionTokens)*price.Output/1_000_000
}

// Record computes the cost from token counts, fills in the timestamp, and appends
// the entry as a JSON line to the log file. Thread-safe.
func (ct *CostTracker) Record(entry CostEntry) error {
	if entry.Ts == "" {
		entry.Ts = time.Now().UTC().Format(time.RFC3339)
	}
	if entry.CostUSD == 0 && (entry.PromptTokens > 0 || entry.CompletionTokens > 0) {
		entry.CostUSD = ct.computeCost(entry.Model, entry.PromptTokens, entry.CompletionTokens)
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("cost_tracker: marshal entry: %w", err)
	}

	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(ct.logPath), 0o755); err != nil {
		return fmt.Errorf("cost_tracker: create log dir: %w", err)
	}

	f, err := os.OpenFile(ct.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("cost_tracker: open log: %w", err)
	}
	defer f.Close()

	if _, err := fmt.Fprintf(f, "%s\n", line); err != nil {
		return fmt.Errorf("cost_tracker: write log: %w", err)
	}
	return nil
}
