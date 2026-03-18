package agent

import (
	"context"
	"fmt"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

var createProviderFromModelConfig = providers.CreateProviderFromConfig // PCL-DOWNSTREAM: allow tests to verify per-candidate provider selection

func cloneLLMOptions(opts map[string]any) map[string]any {
	if len(opts) == 0 {
		return map[string]any{}
	}

	cloned := make(map[string]any, len(opts)+1)
	for key, value := range opts {
		cloned[key] = value
	}
	return cloned
}

func resolveProviderForCandidate(
	agent *AgentInstance,
	candidate *providers.FallbackCandidate,
) (providers.LLMProvider, string, error) {
	if candidate == nil || candidate.ModelConfig == nil {
		if candidate != nil && candidate.Model != "" {
			return agent.Provider, candidate.Model, nil
		}
		return agent.Provider, agent.Model, nil
	}

	candidateCfg := *candidate.ModelConfig
	provider, modelID, err := createProviderFromModelConfig(&candidateCfg)
	if err != nil {
		return nil, "", fmt.Errorf("create provider for %s/%s: %w", candidate.Provider, candidate.Model, err)
	}
	if modelID == "" {
		modelID = candidate.Model
	}
	return provider, modelID, nil
}

func chatWithCandidate(
	ctx context.Context,
	agent *AgentInstance,
	candidate *providers.FallbackCandidate,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	baseOpts map[string]any,
) (*providers.LLMResponse, error) {
	provider, modelID, err := resolveProviderForCandidate(agent, candidate)
	if err != nil {
		return nil, err
	}

	llmOpts := cloneLLMOptions(baseOpts)
	if agent.ThinkingLevel != ThinkingOff {
		if tc, ok := provider.(providers.ThinkingCapable); ok && tc.SupportsThinking() {
			llmOpts["thinking_level"] = string(agent.ThinkingLevel)
		} else {
			logger.WarnCF("agent", "thinking_level is set but current provider does not support it, ignoring",
				map[string]any{"agent_id": agent.ID, "thinking_level": string(agent.ThinkingLevel)})
		}
	}

	return provider.Chat(ctx, messages, toolDefs, modelID, llmOpts)
}
