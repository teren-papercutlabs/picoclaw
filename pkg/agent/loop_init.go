// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/teren-papercutlabs/pclaw/pcl/telemetry" // PCL-DOWNSTREAM: cost tracking
	"github.com/teren-papercutlabs/pclaw/pkg/audio/tts"
	"github.com/teren-papercutlabs/pclaw/pkg/bus"
	"github.com/teren-papercutlabs/pclaw/pkg/channels"
	"github.com/teren-papercutlabs/pclaw/pkg/commands"
	"github.com/teren-papercutlabs/pclaw/pkg/config"
	"github.com/teren-papercutlabs/pclaw/pkg/logger"
	"github.com/teren-papercutlabs/pclaw/pkg/providers"
	"github.com/teren-papercutlabs/pclaw/pkg/skills"
	"github.com/teren-papercutlabs/pclaw/pkg/state"
	"github.com/teren-papercutlabs/pclaw/pkg/tools"
)

func NewAgentLoop(
	cfg *config.Config,
	msgBus *bus.MessageBus,
	provider providers.LLMProvider,
) *AgentLoop {
	registry := NewAgentRegistry(cfg, provider)

	// Set up shared fallback chain with rate limiting.
	cooldown := providers.NewCooldownTracker()
	rl := providers.NewRateLimiterRegistry()
	// Register rate limiters for all agents' candidates so that RPM limits
	// configured in ModelConfig are enforced before each LLM call.
	for _, agentID := range registry.ListAgentIDs() {
		if agent, ok := registry.GetAgent(agentID); ok {
			rl.RegisterCandidates(agent.Candidates)
			rl.RegisterCandidates(agent.LightCandidates)
		}
	}
	fallbackChain := providers.NewFallbackChain(cooldown, rl)

	// Create state manager using default agent's workspace for channel recording
	defaultAgent := registry.GetDefaultAgent()
	var stateManager *state.Manager
	if defaultAgent != nil {
		stateManager = state.NewManager(defaultAgent.Workspace)
	}

	eventBus := NewEventBus()

	// Determine worker pool size from config (default: 1 = sequential)
	workerPoolSize := cfg.Agents.Defaults.MaxParallelTurns
	if workerPoolSize <= 0 {
		workerPoolSize = 1
	}

	al := &AgentLoop{
		bus:         msgBus,
		cfg:         cfg,
		registry:    registry,
		state:       stateManager,
		eventBus:    eventBus,
		fallback:    fallbackChain,
		cmdRegistry: commands.NewRegistry(commands.BuiltinDefinitions()),
		steering:    newSteeringQueue(parseSteeringMode(cfg.Agents.Defaults.SteeringMode)),
		workerSem:   make(chan struct{}, workerPoolSize),
	}
	al.providerFactory = providers.CreateProviderFromConfig
	al.hooks = NewHookManager(eventBus)
	configureHookManagerFromConfig(al.hooks, cfg)
	al.contextManager = al.resolveContextManager()

	// PCL-DOWNSTREAM: cost tracking — initialise tracker when enabled.
	if cfg.CostTracking.Enabled {
		prices := make(map[string]telemetry.ModelPrice, len(cfg.CostTracking.Prices))
		for model, p := range cfg.CostTracking.Prices {
			prices[model] = telemetry.ModelPrice{Input: p.Input, Output: p.Output}
		}

		// Startup validation: refuse to start if any configured model lacks a
		// price entry. Prevents silent cost-untracked traffic.
		var allModels []string
		for _, m := range cfg.ModelList {
			if m != nil {
				allModels = append(allModels, m.ModelName)
			}
		}
		for _, a := range cfg.Agents.List {
			if a.Model != nil {
				allModels = append(allModels, a.Model.Primary)
				allModels = append(allModels, a.Model.Fallbacks...)
			}
		}
		if cfg.Agents.Defaults.ModelName != "" {
			allModels = append(allModels, cfg.Agents.Defaults.ModelName)
		}
		if err := telemetry.ValidateModelPrices(allModels, prices); err != nil {
			logger.ErrorCF("cost_tracking", "Startup blocked", map[string]any{"error": err.Error()})
			fmt.Fprintf(os.Stderr, "FATAL: %s\n", err.Error())
			os.Exit(1)
		}

		logPath := cfg.CostTracking.LogPath
		if logPath == "" {
			logPath = "/home/picoclaw/telemetry/cost.jsonl"
		}
		al.costTracker = telemetry.NewCostTracker(logPath, prices)
	}

	// Register shared tools to all agents (now that al is created)
	registerSharedTools(al, cfg, msgBus, registry, provider)

	return al
}

func registerSharedTools(
	al *AgentLoop,
	cfg *config.Config,
	msgBus *bus.MessageBus,
	registry *AgentRegistry,
	provider providers.LLMProvider,
) {
	allowReadPaths := buildAllowReadPatterns(cfg)
	var ttsProvider tts.TTSProvider
	if cfg.Tools.IsToolEnabled("send_tts") {
		ttsProvider = tts.DetectTTS(cfg)
		if ttsProvider == nil {
			logger.WarnCF("voice-tts", "send_tts enabled but no TTS provider configured", nil)
		}
	}

	for _, agentID := range registry.ListAgentIDs() {
		agent, ok := registry.GetAgent(agentID)
		if !ok {
			continue
		}

		if cfg.Tools.IsToolEnabled("web") {
			searchTool, err := tools.NewWebSearchTool(tools.WebSearchToolOptions{
				BraveAPIKeys:          cfg.Tools.Web.Brave.APIKeys.Values(),
				BraveMaxResults:       cfg.Tools.Web.Brave.MaxResults,
				BraveEnabled:          cfg.Tools.Web.Brave.Enabled,
				TavilyAPIKeys:         cfg.Tools.Web.Tavily.APIKeys.Values(),
				TavilyBaseURL:         cfg.Tools.Web.Tavily.BaseURL,
				TavilyMaxResults:      cfg.Tools.Web.Tavily.MaxResults,
				TavilyEnabled:         cfg.Tools.Web.Tavily.Enabled,
				DuckDuckGoMaxResults:  cfg.Tools.Web.DuckDuckGo.MaxResults,
				DuckDuckGoEnabled:     cfg.Tools.Web.DuckDuckGo.Enabled,
				PerplexityAPIKeys:     cfg.Tools.Web.Perplexity.APIKeys.Values(),
				PerplexityMaxResults:  cfg.Tools.Web.Perplexity.MaxResults,
				PerplexityEnabled:     cfg.Tools.Web.Perplexity.Enabled,
				SearXNGBaseURL:        cfg.Tools.Web.SearXNG.BaseURL,
				SearXNGMaxResults:     cfg.Tools.Web.SearXNG.MaxResults,
				SearXNGEnabled:        cfg.Tools.Web.SearXNG.Enabled,
				GLMSearchAPIKey:       cfg.Tools.Web.GLMSearch.APIKey.String(),
				GLMSearchBaseURL:      cfg.Tools.Web.GLMSearch.BaseURL,
				GLMSearchEngine:       cfg.Tools.Web.GLMSearch.SearchEngine,
				GLMSearchMaxResults:   cfg.Tools.Web.GLMSearch.MaxResults,
				GLMSearchEnabled:      cfg.Tools.Web.GLMSearch.Enabled,
				BaiduSearchAPIKey:     cfg.Tools.Web.BaiduSearch.APIKey.String(),
				BaiduSearchBaseURL:    cfg.Tools.Web.BaiduSearch.BaseURL,
				BaiduSearchMaxResults: cfg.Tools.Web.BaiduSearch.MaxResults,
				BaiduSearchEnabled:    cfg.Tools.Web.BaiduSearch.Enabled,
				Proxy:                 cfg.Tools.Web.Proxy,
			})
			if err != nil {
				logger.ErrorCF("agent", "Failed to create web search tool", map[string]any{"error": err.Error()})
			} else if searchTool != nil {
				agent.Tools.Register(searchTool)
			}
		}
		if cfg.Tools.IsToolEnabled("web_fetch") {
			fetchTool, err := tools.NewWebFetchToolWithProxy(
				50000,
				cfg.Tools.Web.Proxy,
				cfg.Tools.Web.Format,
				cfg.Tools.Web.FetchLimitBytes,
				cfg.Tools.Web.PrivateHostWhitelist)
			if err != nil {
				logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
			} else {
				agent.Tools.Register(fetchTool)
			}
		}

		// Hardware tools (I2C, SPI) - Linux only, returns error on other platforms
		if cfg.Tools.IsToolEnabled("i2c") {
			agent.Tools.Register(tools.NewI2CTool())
		}
		if cfg.Tools.IsToolEnabled("spi") {
			agent.Tools.Register(tools.NewSPITool())
		}

		// Message tool
		if cfg.Tools.IsToolEnabled("message") {
			messageTool := tools.NewMessageTool()
			messageTool.SetSendCallback(func(
				ctx context.Context,
				channel, chatID, content, replyToMessageID string,
			) error {
				pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer pubCancel()
				outboundCtx := bus.NewOutboundContext(channel, chatID, replyToMessageID)
				outboundAgentID, outboundSessionKey, outboundScope := outboundTurnMetadata(
					tools.ToolAgentID(ctx),
					tools.ToolSessionKey(ctx),
					tools.ToolSessionScope(ctx),
				)
				return msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
					Context:          outboundCtx,
					AgentID:          outboundAgentID,
					SessionKey:       outboundSessionKey,
					Scope:            outboundScope,
					Content:          content,
					ReplyToMessageID: replyToMessageID,
				})
			})
			agent.Tools.Register(messageTool)
		}
		if cfg.Tools.IsToolEnabled("reaction") {
			reactionTool := tools.NewReactionTool()
			reactionTool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID string) error {
				if al.channelManager == nil {
					return fmt.Errorf("channel manager not configured")
				}
				ch, ok := al.channelManager.GetChannel(channel)
				if !ok {
					return fmt.Errorf("channel %s not found", channel)
				}
				rc, ok := ch.(channels.ReactionCapable)
				if !ok {
					return fmt.Errorf("channel %s does not support reactions", channel)
				}
				_, err := rc.ReactToMessage(ctx, chatID, messageID)
				return err
			})
			agent.Tools.Register(reactionTool)
		}

		// Send file tool (outbound media via MediaStore — store injected later by SetMediaStore)
		if cfg.Tools.IsToolEnabled("send_file") {
			sendFileTool := tools.NewSendFileTool(
				agent.Workspace,
				cfg.Agents.Defaults.RestrictToWorkspace,
				cfg.Agents.Defaults.GetMaxMediaSize(),
				nil,
				allowReadPaths,
			)
			agent.Tools.Register(sendFileTool)
		}

		if ttsProvider != nil {
			agent.Tools.Register(tools.NewSendTTSTool(ttsProvider, nil))
		}

		if cfg.Tools.IsToolEnabled("load_image") {
			loadImageTool := tools.NewLoadImageTool(
				agent.Workspace,
				cfg.Agents.Defaults.RestrictToWorkspace,
				cfg.Agents.Defaults.GetMaxMediaSize(),
				nil,
				allowReadPaths,
			)
			agent.Tools.Register(loadImageTool)
		}

		// Skill discovery and installation tools
		skills_enabled := cfg.Tools.IsToolEnabled("skills")
		find_skills_enable := cfg.Tools.IsToolEnabled("find_skills")
		install_skills_enable := cfg.Tools.IsToolEnabled("install_skill")
		if skills_enabled && (find_skills_enable || install_skills_enable) {
			registryMgr := skills.NewRegistryManagerFromToolsConfig(cfg.Tools.Skills)

			if find_skills_enable {
				searchCache := skills.NewSearchCache(
					cfg.Tools.Skills.SearchCache.MaxSize,
					time.Duration(cfg.Tools.Skills.SearchCache.TTLSeconds)*time.Second,
				)
				agent.Tools.Register(tools.NewFindSkillsTool(registryMgr, searchCache))
			}

			if install_skills_enable {
				agent.Tools.Register(tools.NewInstallSkillTool(registryMgr, agent.Workspace))
			}
		}

		// PCL-DOWNSTREAM (fix/tgg-tool-execution): register the 5 TGG inversion
		// tools when PICOCLAW_TGG_TOOLS=1. These replace the AGENTS.md-prompted
		// curl-to-/api/ingest/wa-message pattern with native function calls
		// that picoclaw recognizes, validates, and dispatches to /api/tools/<name>.
		// Gated by env var so non-TGG picoclaw instances are unaffected.
		if os.Getenv("PICOCLAW_TGG_TOOLS") == "1" {
			for _, t := range tools.NewTGGTools() {
				agent.Tools.Register(t)
			}
			logger.InfoCF("agent", "registered TGG inversion tools",
				map[string]any{"count": 5, "agent": agentID})
		}

		// Spawn and spawn_status tools share a SubagentManager.
		// Construct it when either tool is enabled (both require subagent).
		spawnEnabled := cfg.Tools.IsToolEnabled("spawn")
		spawnStatusEnabled := cfg.Tools.IsToolEnabled("spawn_status")
		if (spawnEnabled || spawnStatusEnabled) && cfg.Tools.IsToolEnabled("subagent") {
			subagentManager := tools.NewSubagentManager(provider, agent.Model, agent.Workspace)
			subagentManager.SetLLMOptions(agent.MaxTokens, agent.Temperature)

			// PCL-DOWNSTREAM: resolve per-subagent model/provider from the agent registry,
			// so the legacy RunToolLoop fallback path runs on the target agent's model
			// rather than the parent's. The primary spawner path resolves the target
			// agent's Model into cfg.Model above (step "4. Resolve Model"); this fallback
			// hook keeps the non-spawner path consistent.
			subagentManager.SetModelResolver(func(targetAgentID string) (providers.LLMProvider, string, bool) {
				targetAgent, ok := registry.GetAgent(targetAgentID)
				if !ok || targetAgent.Provider == nil {
					return nil, "", false
				}
				return targetAgent.Provider, targetAgent.Model, true
			})

			// Inject a media resolver so the legacy RunToolLoop fallback path can
			// resolve media:// refs in the same way the main AgentLoop does.
			// This keeps subagent vision support working even when the optimized
			// sub-turn spawner path is unavailable.
			subagentManager.SetMediaResolver(func(msgs []providers.Message) []providers.Message {
				return resolveMediaRefs(msgs, al.mediaStore, cfg.Agents.Defaults.GetMaxMediaSize())
			})

			// Set the spawner that links into AgentLoop's turnState
			subagentManager.SetSpawner(func(
				ctx context.Context,
				task, label, targetAgentID string,
				tls *tools.ToolRegistry,
				maxTokens int,
				temperature float64,
				hasMaxTokens, hasTemperature bool,
			) (*tools.ToolResult, error) {
				// 1. Recover parent Turn State from Context
				parentTS := turnStateFromContext(ctx)
				if parentTS == nil {
					// Fallback: If no turnState exists in context, create an isolated ad-hoc root turn state
					// so that the tool can still function outside of an agent loop (e.g. tests, raw invocations).
					parentTS = &turnState{
						ctx:            ctx,
						turnID:         "adhoc-root",
						depth:          0,
						session:        nil, // Ephemeral session not needed for adhoc spawn
						pendingResults: make(chan *tools.ToolResult, 16),
						concurrencySem: make(chan struct{}, 5),
					}
				}

				// 2. Build Tools slice from registry
				var tlSlice []tools.Tool
				for _, name := range tls.List() {
					if t, ok := tls.Get(name); ok {
						tlSlice = append(tlSlice, t)
					}
				}

				// 3. System Prompt
				systemPrompt := "You are a subagent. Complete the given task independently and report the result.\n" +
					"You have access to tools - use them as needed to complete your task.\n" +
					"After completing the task, provide a clear summary of what was done.\n\n" +
					"Task: " + task

				// 4. Resolve Model
				modelToUse := agent.Model
				if targetAgentID != "" {
					if targetAgent, ok := al.GetRegistry().GetAgent(targetAgentID); ok {
						modelToUse = targetAgent.Model
					}
				}

				// 5. Build SubTurnConfig
				cfg := SubTurnConfig{
					Model:        modelToUse,
					Tools:        tlSlice,
					SystemPrompt: systemPrompt,
				}
				if hasMaxTokens {
					cfg.MaxTokens = maxTokens
				}

				// 6. Spawn SubTurn
				return spawnSubTurn(ctx, al, parentTS, cfg)
			})

			// Clone the parent's tool registry so subagents can use all
			// tools registered so far (file, web, etc.) but NOT spawn/
			// spawn_status which are added below — preventing recursive
			// subagent spawning.
			subagentManager.SetTools(agent.Tools.Clone())
			if spawnEnabled {
				spawnTool := tools.NewSpawnTool(subagentManager)
				spawnTool.SetSpawner(NewSubTurnSpawner(al))
				currentAgentID := agentID
				spawnTool.SetAllowlistChecker(func(targetAgentID string) bool {
					return registry.CanSpawnSubagent(currentAgentID, targetAgentID)
				})

				agent.Tools.Register(spawnTool)

				// Also register the synchronous subagent tool
				subagentTool := tools.NewSubagentTool(subagentManager)
				subagentTool.SetSpawner(NewSubTurnSpawner(al))
				agent.Tools.Register(subagentTool)
			}
			if spawnStatusEnabled {
				agent.Tools.Register(tools.NewSpawnStatusTool(subagentManager))
			}
		} else if (spawnEnabled || spawnStatusEnabled) && !cfg.Tools.IsToolEnabled("subagent") {
			logger.WarnCF("agent", "spawn/spawn_status tools require subagent to be enabled", nil)
		}
	}
}
