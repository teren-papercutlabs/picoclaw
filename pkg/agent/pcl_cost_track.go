// PCL-DOWNSTREAM: cost tracking
// All code in this file is PcL-only and is not part of upstream picoclaw.
// Downstream: permanent

package agent

import (
	"github.com/sipeed/picoclaw/pcl/telemetry"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// pclTurnUsage accumulates LLM usage and tool results across all iterations
// within a single agent turn. It is populated inside runLLMIteration and
// consumed by pclCostTrack in runAgentLoop.
type pclTurnUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ToolResults      []telemetry.ToolResult
}

// pclAccumulateUsage adds token counts from one LLM response to the accumulator.
func pclAccumulateUsage(acc *pclTurnUsage, u *providers.UsageInfo) {
	if u == nil {
		return
	}
	acc.PromptTokens += u.PromptTokens
	acc.CompletionTokens += u.CompletionTokens
	acc.TotalTokens += u.TotalTokens
}

// pclAppendToolResult appends a single tool result to the accumulator.
// Called inline inside runLLMIteration where agentResults are processed.
func pclAppendToolResult(acc *pclTurnUsage, name string, err error) {
	tr := telemetry.ToolResult{
		Name:    name,
		Success: err == nil,
	}
	if err != nil {
		tr.Error = err.Error()
	}
	acc.ToolResults = append(acc.ToolResults, tr)
}

// pclCostTrack records a completed turn to the cost tracker if one is configured.
// Called from runAgentLoop after runLLMIteration completes.
func (al *AgentLoop) pclCostTrack(
	agentID, model, sessionKey string,
	usage *pclTurnUsage,
	iterations int,
	durationMs int64,
) {
	if al.costTracker == nil || usage == nil {
		return
	}
	entry := telemetry.CostEntry{
		AgentID:          agentID,
		Model:            model,
		SessionKey:       sessionKey,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		ToolResults:      usage.ToolResults,
		Iterations:       iterations,
		DurationMs:       durationMs,
	}
	if err := al.costTracker.Record(entry); err != nil {
		logger.WarnCF("agent", "cost tracking write failed", map[string]any{"error": err.Error()})
	}
}
