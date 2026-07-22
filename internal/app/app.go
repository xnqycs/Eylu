package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"Eylu/internal/agent"
	"Eylu/internal/buildinfo"
	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/driver/anthropic_messages"
	"Eylu/internal/driver/gemini_interactions"
	"Eylu/internal/driver/mistral_conversations"
	"Eylu/internal/driver/openai_chat"
	"Eylu/internal/driver/openai_responses"
	"Eylu/internal/driver/perplexity_agent"
	"Eylu/internal/environment"
	"Eylu/internal/logging"
	"Eylu/internal/mcpclient"
	"Eylu/internal/metrics"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/routing"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
	"Eylu/internal/webtool"
)

const (
	exitOK       = 0
	exitUsage    = 2
	exitConfig   = 3
	exitNetwork  = 4
	exitProvider = 5
	exitInternal = 10
)

type runtime struct {
	stdin              io.Reader
	stdout             io.Writer
	stderr             io.Writer
	configPath         string
	workspace          string
	output             string
	secretMu           sync.RWMutex
	apiKeys            []string
	inputMu            sync.Mutex
	inputReader        *bufio.Reader
	inputRead          chan inputLineResult
	inputInterrupts    <-chan os.Signal
	trustPrompted      map[string]bool
	session            *sessionRuntime
	metrics            *metrics.Collector
	mcp                *mcpclient.Manager
	mcpKey             string
	mcpMu              sync.Mutex
	mcpHostMu          sync.RWMutex
	mcpHost            mcpHostCallbacks
	mcpStateMu         sync.RWMutex
	mcpState           agent.MCPRuntimeState
	mcpEvents          []mcpclient.Event
	mcpConversations   map[*agent.Conversation]int
	mcpWatchStop       func()
	mcpWatchDone       chan struct{}
	suppressMCPStderr  atomic.Bool
	environmentCapture func(context.Context, string) environment.Context
	limitMu            sync.Mutex
	limitResolver      *provider.LimitResolver
	metadataCachePath  string
	limitWarnings      map[string]bool
}

func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	r := &runtime{stdin: stdin, stdout: stdout, stderr: stderr, trustPrompted: make(map[string]bool), metrics: &metrics.Collector{}}
	root := r.rootCommand(ctx)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	defer func() {
		if err := r.closeMCP(); err != nil {
			fmt.Fprintf(stderr, "[mcp] close: %v\n", err)
		}
	}()
	if err := root.ExecuteContext(ctx); err != nil {
		r.printError(err)
		return exitCode(err)
	}
	return exitOK
}

func (r *runtime) rootCommand(ctx context.Context) *cobra.Command {
	var opts chatOptions
	root := &cobra.Command{
		Use:           "eylu [prompt]",
		Short:         "Eylu terminal programming agent",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.resumeSet = cmd.Flags().Changed("resume")
			return r.executeChatCommand(ctx, args, opts)
		},
		PersistentPreRunE: func(*cobra.Command, []string) error {
			switch r.output {
			case "text", "json", "jsonl":
				return nil
			default:
				return &protocol.Error{Code: protocol.ErrConfig, Message: "output must be text, json, or jsonl"}
			}
		},
	}
	root.Version = buildinfo.String()
	root.PersistentFlags().StringVar(&r.configPath, "config", "", "config file path")
	root.PersistentFlags().StringVar(&r.workspace, "workspace", "", "workspace directory")
	root.PersistentFlags().StringVar(&r.output, "output", "text", "output format: text, json, or jsonl")
	bindChatFlags(root, &opts)
	root.AddCommand(r.chatCommand(ctx), r.providersCommand(ctx), r.skillsCommand(ctx), r.sessionsCommand(), r.mcpCommand(ctx), r.versionCommand())
	return root
}

func (r *runtime) versionCommand() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "show Eylu build version", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		info := buildinfo.Current()
		if r.output != "text" {
			return json.NewEncoder(r.stdout).Encode(info)
		}
		fmt.Fprintln(r.stdout, buildinfo.String())
		return nil
	}}
}

type chatOptions struct {
	provider         string
	model            string
	baseURL          string
	adapter          string
	timeout          time.Duration
	approve          bool
	mode             string
	trustSkills      bool
	noAnimation      bool
	noTUI            bool
	sessionID        string
	resumeID         string
	resumeSet        bool
	routeMode        string
	task             string
	requireReasoning bool
}

func (r *runtime) chatCommand(ctx context.Context) *cobra.Command {
	var opts chatOptions
	cmd := &cobra.Command{
		Use:   "chat [prompt]",
		Short: "send a prompt to the active model",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.resumeSet = cmd.Flags().Changed("resume")
			return r.executeChatCommand(ctx, args, opts)
		},
	}
	bindChatFlags(cmd, &opts)
	return cmd
}

func (r *runtime) executeChatCommand(ctx context.Context, args []string, opts chatOptions) error {
	if opts.resumeSet && opts.resumeID == "" {
		return &protocol.Error{Code: protocol.ErrConfig, Message: "resume session ID is required"}
	}
	terminal := isTerminal(r.stdin)
	if len(args) == 0 && terminal {
		return r.runInteractive(ctx, opts)
	}
	prompt := ""
	if len(args) == 1 {
		prompt = args[0]
	} else if !terminal {
		raw, err := io.ReadAll(io.LimitReader(r.stdin, 1<<20))
		if err != nil {
			return err
		}
		prompt = strings.TrimSpace(string(raw))
	}
	if prompt == "" {
		return &protocol.Error{Code: protocol.ErrConfig, Message: "prompt is required; use eylu \"your request\" or run eylu in a terminal"}
	}
	return r.runChat(ctx, prompt, opts)
}

func bindChatFlags(cmd *cobra.Command, opts *chatOptions) {
	cmd.Flags().StringVar(&opts.provider, "provider", "", "provider name")
	cmd.Flags().StringVar(&opts.model, "model", "", "model ID override")
	cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "API base URL override")
	cmd.Flags().StringVar(&opts.adapter, "adapter", openai_responses.Name, "model driver adapter")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 0, "request timeout")
	cmd.Flags().BoolVarP(&opts.approve, "yes", "y", false, "approve tools that require confirmation")
	cmd.Flags().StringVar(&opts.mode, "mode", "", "permission mode: manual, plan, auto, or full")
	cmd.Flags().BoolVar(&opts.trustSkills, "trust-workspace-skills", false, "trust and load project-level skills for this workspace")
	cmd.Flags().BoolVar(&opts.noAnimation, "no-animation", false, "disable terminal animations")
	cmd.Flags().BoolVar(&opts.noTUI, "no-tui", false, "use the line-oriented interactive interface")
	cmd.Flags().StringVar(&opts.sessionID, "session", "", "create or open a session ID")
	cmd.Flags().StringVar(&opts.resumeID, "resume", "", "resume an existing session ID")
	cmd.Flags().StringVar(&opts.routeMode, "route", "", "provider routing mode: fixed or auto")
	cmd.Flags().StringVar(&opts.task, "task", "", "routing task: general, coding, review, debugging, testing, or documentation")
	cmd.Flags().BoolVar(&opts.requireReasoning, "require-reasoning", false, "require a driver with reasoning support")
	cmd.MarkFlagsMutuallyExclusive("session", "resume")
}

func (r *runtime) runChat(ctx context.Context, prompt string, opts chatOptions) error {
	manager, err := r.prepareManager(ctx, opts)
	if err != nil {
		return err
	}
	conversation, err := r.openConversation(ctx, manager, &opts)
	if err != nil {
		return err
	}
	return r.sendPrompt(ctx, conversation, manager, prompt, opts)
}

func (r *runtime) prepareManager(ctx context.Context, opts chatOptions) (*provider.Manager, error) {
	loaded, manager, err := r.loadManager()
	if err != nil {
		return nil, err
	}
	if len(loaded.Config.Providers) > 0 {
		return manager, nil
	}
	if opts.baseURL != "" && opts.model != "" {
		loaded.Config.Providers["runtime"] = config.ProviderConfig{
			Adapter: opts.adapter, BaseURL: opts.baseURL, Model: opts.model,
		}
		loaded.Config.ActiveProvider = "runtime"
		return provider.NewManager(loaded.Path, loaded.Config, nil)
	}
	if isTerminal(r.stdin) {
		if err := r.onboard(ctx, manager); err != nil {
			return nil, err
		}
		return manager, nil
	}
	return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "no provider configured; run eylu providers add or pass --base-url and --model"}
}

func (r *runtime) sendPrompt(ctx context.Context, conversation *agent.Conversation, manager *provider.Manager, prompt string, opts chatOptions) error {
	detachMCP := r.attachMCPConversation(conversation)
	defer detachMCP()
	conversation.RecordPrompt(prompt)
	cfg := manager.Config()
	estimator := contextledger.ApproxEstimator{BytesPerToken: cfg.TokenBytesPerToken}
	report := conversation.ContextReport()
	estimatedInput := report.InputTokens + estimator.Estimate(prompt)
	stream := r.output == "text" || r.output == "jsonl"
	modelRuntime, routeDecision, err := r.resolveRuntimeForPrompt(ctx, manager, opts, prompt, estimatedInput+cfg.ReservedOutputTokens, estimatedInput, cfg.ReservedOutputTokens, stream)
	if err != nil {
		return err
	}
	r.warnContextLimit(modelRuntime.Provider)
	jsonlEncoder := json.NewEncoder(r.stdout)
	if routeDecision != nil {
		if r.output == "jsonl" {
			_ = jsonlEncoder.Encode(map[string]any{"type": "routing", "routing": routeDecision})
		} else {
			fmt.Fprintf(r.stderr, "[routing] task=%s provider=%s %s\n", routeDecision.Task, routeDecision.Provider, routeDecision.Candidates[0].Reason)
		}
	}
	modeName := cfg.PermissionMode
	if opts.mode != "" {
		modeName = opts.mode
	}
	mode, err := policy.ParseMode(modeName)
	if err != nil {
		return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	modelRuntime.PermissionMode = mode.String()
	modelRuntime.Workspace = r.workspace
	modelRuntime.TokenEstimator = contextledger.ApproxEstimator{BytesPerToken: cfg.TokenBytesPerToken}
	modelRuntime.OutputReserveTokens = cfg.ReservedOutputTokens
	if maximum := modelRuntime.Provider.Limits.MaxOutputTokens; maximum > 0 && maximum < modelRuntime.OutputReserveTokens {
		modelRuntime.OutputReserveTokens = maximum
	}
	modelRuntime.ContextRecentRounds = cfg.ContextRecentRounds
	modelRuntime.ContextCompactTrigger = cfg.ContextCompactTrigger
	modelRuntime.ContextCompactTarget = cfg.ContextCompactTarget
	modelRuntime.MaxProjectMapBytes = cfg.MaxProjectMapBytes
	modelRuntime.MaxToolContextBytes = cfg.MaxToolContextBytes
	modelRuntime.SkillCatalogPageBytes = cfg.SkillCatalogPageBytes
	modelRuntime.MaxSummaryBytes = cfg.MaxSummaryBytes
	confirm := r.confirmTools(opts.approve)
	var ask tool.AskFunc
	if r.output == "text" && isTerminal(r.stdin) {
		ask = r.askUser
	}
	host := buildMCPHostCallbacks(modelRuntime, confirm, ask, openMCPElicitationURL)
	if err := r.configureMCPRuntimeWithHost(ctx, cfg, &modelRuntime, host); err != nil {
		return err
	}
	var observation *metrics.Observation
	modelRuntime.ContextEvent = func(event contextledger.Event) {
		if observation != nil {
			observation.ObserveContextEvent(event)
		}
		if r.output == "jsonl" {
			_ = jsonlEncoder.Encode(map[string]any{"type": "context", "context": event})
			return
		}
		switch event.Kind {
		case contextledger.EventCompression:
			if event.Compression != nil {
				fmt.Fprintf(r.stderr, "[context] compressed before=%d after=%d omitted_turns=%d summary_bytes=%d\n", event.Compression.BeforeTokens, event.Compression.AfterTokens, event.Compression.OmittedTurns, event.Compression.SummaryBytes)
			}
		case contextledger.EventBudget:
			fmt.Fprintf(r.stderr, "[context] budget input=%d reserve=%d window=%d percent=%.1f\n", event.InputTokens, event.OutputReserve, event.ContextWindow, event.Percent)
		}
	}
	skillRegistry, skillSession, err := r.loadSkillRuntime(ctx, cfg, opts, conversation, nil)
	if err != nil {
		return err
	}
	modelRuntime.SkillCatalog = skillRegistry.Catalog()
	overallTimeout := time.Duration(cfg.MaxTurns) * modelRuntime.Timeout
	requestCtx, cancel := context.WithTimeout(ctx, overallTimeout)
	defer cancel()
	var emit driver.EmitFunc
	if stream {
		emit = func(event protocol.ModelEvent) error {
			if r.output == "jsonl" {
				return jsonlEncoder.Encode(map[string]any{"type": "model_event", "event": event})
			}
			switch event.Kind {
			case protocol.EventTextDelta:
				_, err := fmt.Fprint(r.stdout, event.Delta)
				return err
			case protocol.EventToolStart:
				fmt.Fprintf(r.stderr, "\n[tool] %s call_id=%s\n", event.ToolCall.Name, event.ToolCall.ID)
			case protocol.EventToolResult:
				fmt.Fprintf(r.stderr, "[tool] completed call_id=%s error=%t truncated=%t\n", event.ToolResult.CallID, event.ToolResult.IsError, event.ToolResult.Truncated)
			case protocol.EventWebSearchStarted, protocol.EventWebSearchCompleted, protocol.EventWebFetchStarted, protocol.EventWebFetchCompleted:
				if event.WebActivity != nil {
					fmt.Fprintf(r.stderr, "[web] kind=%s status=%s call_id=%s sources=%d\n", event.WebActivity.Kind, event.WebActivity.Status, event.WebActivity.CallID, len(event.WebActivity.Sources))
				}
			case protocol.EventCitation:
				if event.Citation != nil {
					fmt.Fprintf(r.stderr, "[citation] %s %s\n", event.Citation.Title, event.Citation.URL)
				}
			}
			return nil
		}
	}
	audit := tool.AuditSink(&toolAuditWriter{writer: r.stderr})
	if r.output == "jsonl" {
		audit = &toolAuditWriter{writer: r.stdout, jsonl: true}
	}
	executor, err := r.toolExecutorWith(cfg, opts, skillRegistry, skillSession, confirm, ask, audit)
	if err != nil {
		return err
	}
	task := routing.Classify(prompt)
	if routeDecision != nil {
		task = routeDecision.Task
	} else if opts.task != "" {
		task = opts.task
	}
	observation = r.metricCollector().Begin(metrics.Metadata{
		SessionID: conversation.SessionID(), Provider: modelRuntime.Provider.Name, ProviderGeneration: modelRuntime.Provider.Generation,
		Model: modelRuntime.Provider.Config.Model, Task: task,
		InputCostPerMillion:  modelRuntime.Provider.Config.Routing.InputCostPerMillion,
		OutputCostPerMillion: modelRuntime.Provider.Config.Routing.OutputCostPerMillion,
	})
	executor.SessionID, executor.ProviderName = conversation.SessionID(), modelRuntime.Provider.Name
	executor.ProviderGeneration, executor.Model = modelRuntime.Provider.Generation, modelRuntime.Provider.Config.Model
	baseEmit := emit
	emit = func(event protocol.ModelEvent) error {
		observation.ObserveModelEvent(event)
		if baseEmit != nil {
			return baseEmit(event)
		}
		return nil
	}
	response, err := runConversationWithProfile(requestCtx, conversation, prompt, modelRuntime, executor, agent.LoopOptions{MaxTurns: cfg.MaxTurns, MaxTotalTokens: cfg.MaxTotalTokens, RequestID: observation.RequestID()}, stream, emit)
	metric := observation.Finish(response.Usage, err)
	r.reportMetric(jsonlEncoder, metric)
	syncErr := error(nil)
	if r.session != nil {
		syncErr = r.session.Sync(conversation, manager, opts, err)
	}
	if err != nil {
		if r.output == "text" {
			fmt.Fprintln(r.stdout)
		}
		return err
	}
	if syncErr != nil {
		return syncErr
	}
	if r.output == "json" {
		return json.NewEncoder(r.stdout).Encode(response)
	}
	if r.output == "jsonl" {
		return jsonlEncoder.Encode(map[string]any{"type": "response", "response": response})
	}
	fmt.Fprintln(r.stdout)
	return nil
}

func runConversationWithProfile(ctx context.Context, conversation *agent.Conversation, prompt string, runtime agent.Runtime, executor *tool.Executor, options agent.LoopOptions, stream bool, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	profile := agent.ProfileForMode(runtime.PermissionMode)
	options.MaxTurns = profile.LimitTurns(options.MaxTurns)
	if !profile.Isolated {
		return conversation.Run(ctx, prompt, runtime, executor, options, stream, emit)
	}
	fork, err := conversation.Fork(profile)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	response, runErr := fork.Run(ctx, prompt, runtime, executor, options, stream, emit)
	if runErr != nil {
		return response, runErr
	}
	forkState := fork.ExportState()
	if adoptErr := conversation.Adopt(prompt, runtime, &response, forkState.ProtectedSkills...); adoptErr != nil {
		return response, adoptErr
	}
	return response, nil
}

func (r *runtime) metricCollector() *metrics.Collector {
	if r.metrics == nil {
		r.metrics = &metrics.Collector{}
	}
	return r.metrics
}

func (r *runtime) reportMetric(jsonlEncoder *json.Encoder, metric metrics.RequestMetric) {
	if r.output == "jsonl" {
		_ = jsonlEncoder.Encode(map[string]any{"type": "metrics", "metrics": metric})
		return
	}
	if r.output == "text" {
		fmt.Fprintf(r.stderr, "[metrics] timestamp=%s session_id=%s request_id=%s provider_name=%s provider_generation=%d model=%s first_token_ms=%d generation_ms=%d tokens_per_second=%.3f duration_ms=%d tool_success_rate=%.3f compression_count=%d input_tokens=%d output_tokens=%d estimated_cost=%.8f error_code=%s\n",
			metric.Timestamp.Format(time.RFC3339Nano), metric.SessionID, metric.RequestID, metric.Provider, metric.ProviderGeneration, metric.Model, metric.FirstTokenMS, metric.GenerationMS, metric.TokensPerSecond, metric.DurationMS, metric.ToolSuccessRate, metric.CompressionCount, metric.Usage.InputTokens, metric.Usage.OutputTokens, metric.EstimatedCost, metric.ErrorCode)
	}
}

func (r *runtime) toolExecutorWith(cfg config.Config, opts chatOptions, skillRegistry *skill.Registry, skillSession *skill.Session, confirm tool.ConfirmFunc, ask tool.AskFunc, audit tool.AuditSink) (*tool.Executor, error) {
	readFile, err := tool.NewReadFile(r.workspace, cfg.MaxReadBytes)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize read_file", Cause: err}
	}
	writeFile, err := tool.NewWriteFile(r.workspace)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize write_file", Cause: err}
	}
	bashTool, err := tool.NewBash(r.workspace, cfg.MaxOutputBytes, nil)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize bash", Cause: err}
	}
	bashTool.AllowEnvironment(cfg.ShellEnvironment)
	editFile, err := tool.NewEditFile(r.workspace, int64(cfg.MaxReadBytes))
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize edit_file", Cause: err}
	}
	index, err := tool.NewRepositoryIndex(r.workspace)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize repository index", Cause: err}
	}
	searchCode := tool.NewSearchCode(index, cfg.MaxSearchResults, int64(cfg.MaxReadBytes))
	listDirectory := tool.NewListDirectory(index, cfg.MaxSearchResults*10)
	modeName := cfg.PermissionMode
	if opts.mode != "" {
		modeName = opts.mode
	}
	mode, err := policy.ParseMode(modeName)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	checker := policy.NewChecker(policy.Config{Mode: mode, ReadOnlyCommands: cfg.ReadOnlyCommands, AutoAllowCommands: cfg.AutoAllowCommands, DangerousPatterns: cfg.DangerousCommands, BlockedPatterns: cfg.BlockedCommands})
	registered := []tool.Tool{readFile, writeFile, bashTool, editFile, searchCode, listDirectory, tool.NewTodoList()}
	if ask != nil {
		registered = append(registered, tool.NewAsk(ask))
	}
	registered = append(registered, r.currentMCPState().Tools...)
	if skillRegistry != nil && len(skillRegistry.Active()) > 0 {
		registered = append(registered, tool.NewActivateSkill(skillRegistry, skillSession), tool.NewReadSkillResource(skillSession))
	}
	profile := agent.ProfileForMode(mode.String())
	filtered := registered[:0]
	for _, item := range registered {
		if profile.AllowsTool(item.Definition().Name, item.Risk()) {
			filtered = append(filtered, item)
		}
	}
	registered = filtered
	return &tool.Executor{
		Registry: tool.NewRegistry(registered...), Policy: checker,
		Confirm: confirm, Audit: audit, Workspace: r.workspace,
		Timeout: time.Duration(cfg.ToolTimeoutSec) * time.Second, MaxOutputBytes: cfg.MaxOutputBytes, MaxParallelTools: cfg.MaxParallelTools,
	}, nil
}

func (r *runtime) confirmTools(approve bool) tool.ConfirmFunc {
	return func(ctx context.Context, request policy.Request, outcome policy.Outcome) (tool.Confirmation, error) {
		if approve {
			return tool.Confirmation{Approved: true}, nil
		}
		if !isTerminal(r.stdin) {
			return tool.Confirmation{}, nil
		}
		reader := r.currentInputReader()
		if reader == nil {
			reader = bufio.NewReader(r.stdin)
		}
		modelReason, preview := approvalRequestDetails(request.Tool, request.Input)
		preview = r.redact(preview)
		if len(preview) > 512 {
			preview = preview[:512] + "..."
		}
		label := "CONFIRM"
		if outcome.Warning {
			label = "DANGER"
		}
		fmt.Fprintf(r.stderr, "%s [%d/%d] approve %s tool %s?\nReason: %s\nPolicy: %s\n%s\n[y/N]: ", label, request.ConfirmationStep, request.ConfirmationTotal, outcome.Risk, request.Tool, modelReason, outcome.Reason, preview)
		answer, err := r.readInteractiveLine(ctx, reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return tool.Confirmation{}, err
		}
		return tool.Confirmation{Approved: strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes")}, nil
	}
}

func (r *runtime) resolveRuntimeForPrompt(ctx context.Context, manager *provider.Manager, opts chatOptions, prompt string, requiredContext, estimatedInput, estimatedOutput int, stream bool) (agent.Runtime, *routing.Decision, error) {
	var snapshot provider.Snapshot
	var decision *routing.Decision
	var err error
	metadata := manager.Config().ModelMetadata
	resolver := r.modelLimitResolver(metadata)
	resolveOne := func(candidate provider.Snapshot) (provider.Snapshot, error) {
		candidate = applyChatProviderOverrides(candidate, opts)
		resolved, resolveErr := resolver.Resolve(ctx, candidate, providerAPIKey(candidate.Config))
		if resolveErr != nil {
			return provider.Snapshot{}, resolveErr
		}
		return resolved, nil
	}
	if opts.provider != "" {
		var ok bool
		snapshot, ok = manager.Snapshot(opts.provider)
		if !ok {
			return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", opts.provider)}
		}
		snapshot, err = resolveOne(snapshot)
	} else {
		routeMode := opts.routeMode
		if routeMode == "" {
			routeMode = manager.Config().RoutingMode
		}
		switch routeMode {
		case "fixed":
			snapshot, err = manager.Active()
			if err == nil {
				snapshot, err = resolveOne(snapshot)
			}
		case "auto":
			task := opts.task
			if task == "" {
				task = routing.Classify(prompt)
			}
			if !routing.ValidTask(task) {
				return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid routing task %q", task)}
			}
			candidates := manager.List()
			for index := range candidates {
				candidates[index] = applyChatProviderOverrides(candidates[index], opts)
			}
			resolveCtx, cancel := context.WithTimeout(ctx, time.Duration(metadata.RequestTimeoutSeconds)*time.Second)
			candidates = resolver.ResolveMany(resolveCtx, candidates, providerAPIKey, 4)
			cancel()
			routeDecision, routeErr := routing.Select(candidates, routing.Request{
				Task: task, RequiredContext: requiredContext, EstimatedInput: estimatedInput, EstimatedOutput: estimatedOutput,
				Capabilities: driver.Capabilities{TextStreaming: stream, ToolCalling: true, Reasoning: opts.requireReasoning},
			}, knownDriverCapabilities)
			if routeErr != nil {
				return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: routeErr.Error(), Cause: routeErr}
			}
			snapshot = routeDecision.Selected
			decision = &routeDecision
		default:
			return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid routing mode %q", routeMode)}
		}
		if err != nil {
			return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
	}
	if err != nil {
		return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	providerConfig := snapshot.Config
	if err := config.ValidateProvider(snapshot.Name, providerConfig); err != nil {
		return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	snapshot.Config = providerConfig
	apiKey := providerAPIKey(providerConfig)
	requestTimeout := providerConfig.Timeout(60 * time.Second)
	if opts.timeout > 0 {
		requestTimeout = opts.timeout
	}
	httpClient := &http.Client{Timeout: requestTimeout}
	registry := defaultDriverRegistry(httpClient)
	modelDriver, err := registry.Get(providerConfig.Adapter)
	if err != nil {
		return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	return agent.Runtime{
		Provider: snapshot, APIKey: apiKey, Driver: modelDriver, LimitResolver: resolver, Timeout: requestTimeout,
		WebDelegate: r.delegatedWebBackend(manager),
	}, decision, nil
}

func defaultDriverRegistry(client *http.Client) *driver.Registry {
	return driver.NewRegistry(
		openai_responses.New(client), openai_chat.New(client), anthropic_messages.New(client), gemini_interactions.New(client),
		mistral_conversations.New(client), perplexity_agent.New(client),
	)
}

func (r *runtime) resolveCompactionRuntime(ctx context.Context, manager *provider.Manager, conversation *agent.Conversation, opts chatOptions, skillCatalog string) (agent.Runtime, error) {
	selected, err := selectedEffortProvider(manager, conversation, opts)
	if err != nil {
		return agent.Runtime{}, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	compactOptions := opts
	compactOptions.provider = selected.Name
	report := conversation.ContextReport()
	modelRuntime, _, err := r.resolveRuntimeForPrompt(ctx, manager, compactOptions, "context compaction", report.TotalTokens, report.InputTokens, report.OutputReserve, false)
	if err != nil {
		return agent.Runtime{}, err
	}
	modelRuntime.PermissionMode = selectedMode(manager, opts)
	if skillCatalog == "" {
		skillCatalog = conversation.ExportState().SkillCatalog
	}
	modelRuntime.SkillCatalog = skillCatalog
	if err := r.configureMCPRuntime(ctx, manager.Config(), &modelRuntime); err != nil {
		return agent.Runtime{}, err
	}
	configureContextRuntime(&modelRuntime, r.workspace, manager.Config())
	return modelRuntime, nil
}

func applyChatProviderOverrides(snapshot provider.Snapshot, opts chatOptions) provider.Snapshot {
	if opts.model != "" {
		snapshot.Config.Model = opts.model
	}
	if opts.baseURL != "" {
		snapshot.Config.BaseURL = opts.baseURL
	}
	if opts.adapter != "" && opts.adapter != openai_responses.Name {
		snapshot.Config.Adapter = opts.adapter
	}
	return snapshot
}

func (r *runtime) probeStartupModelLimits(ctx context.Context, manager *provider.Manager, opts chatOptions) (provider.Snapshot, error) {
	cfg := manager.Config()
	metadata := cfg.ModelMetadata
	resolver := r.modelLimitResolver(metadata)
	if opts.provider != "" {
		snapshot, ok := manager.Snapshot(opts.provider)
		if !ok {
			return provider.Snapshot{}, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", opts.provider)}
		}
		return resolver.Resolve(ctx, applyChatProviderOverrides(snapshot, opts), providerAPIKey(snapshot.Config))
	}
	routeMode := opts.routeMode
	if routeMode == "" {
		routeMode = cfg.RoutingMode
	}
	if routeMode == "fixed" {
		snapshot, err := manager.Active()
		if err != nil {
			return provider.Snapshot{}, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		snapshot = applyChatProviderOverrides(snapshot, opts)
		return resolver.Resolve(ctx, snapshot, providerAPIKey(snapshot.Config))
	}
	if routeMode != "auto" {
		return provider.Snapshot{}, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid routing mode %q", routeMode)}
	}
	candidates := manager.List()
	for index := range candidates {
		candidates[index] = applyChatProviderOverrides(candidates[index], opts)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, time.Duration(metadata.RequestTimeoutSeconds)*time.Second)
	resolved := resolver.ResolveMany(resolveCtx, candidates, providerAPIKey, 4)
	cancel()
	active, err := manager.Active()
	if err != nil {
		return provider.Snapshot{}, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	for _, snapshot := range resolved {
		if snapshot.Name == active.Name {
			return snapshot, nil
		}
	}
	return applyChatProviderOverrides(active, opts), nil
}

func (r *runtime) probeProviderModelLimits(ctx context.Context, manager *provider.Manager, name string) (provider.Snapshot, error) {
	snapshot, ok := manager.Snapshot(name)
	if !ok {
		return provider.Snapshot{}, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", name)}
	}
	resolver := r.modelLimitResolver(manager.Config().ModelMetadata)
	return resolver.Resolve(ctx, snapshot, providerAPIKey(snapshot.Config))
}

func (r *runtime) modelLimitResolver(metadata config.ModelMetadataConfig) *provider.LimitResolver {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if r.limitResolver == nil {
		r.limitResolver = provider.NewLimitResolver(metadata, r.metadataCachePath, nil)
	}
	return r.limitResolver
}

func (r *runtime) warnContextLimit(snapshot provider.Snapshot) {
	configured, detected := snapshot.Config.ContextWindow, snapshot.Limits.ContextWindow
	if r.stderr == nil || configured <= 0 || detected <= 0 || configured <= detected {
		return
	}
	key := fmt.Sprintf("%s\x00%s\x00%d", snapshot.Name, snapshot.Config.Model, detected)
	r.limitMu.Lock()
	if r.limitWarnings == nil {
		r.limitWarnings = make(map[string]bool)
	}
	if r.limitWarnings[key] {
		r.limitMu.Unlock()
		return
	}
	r.limitWarnings[key] = true
	r.limitMu.Unlock()
	fmt.Fprintf(r.stderr, "[context] configured override=%d exceeds detected limit=%d; effective=%d source=%s\n", configured, detected, snapshot.ContextWindowLimit(), snapshot.Limits.Source)
}

func knownDriverCapabilities(snapshot provider.Snapshot) (driver.Capabilities, bool) {
	var modelDriver driver.ModelDriver
	switch snapshot.Config.Adapter {
	case openai_responses.Name:
		modelDriver = openai_responses.New(nil)
	case openai_chat.Name:
		modelDriver = openai_chat.New(nil)
	case anthropic_messages.Name:
		modelDriver = anthropic_messages.New(nil)
	case gemini_interactions.Name:
		modelDriver = gemini_interactions.New(nil)
	case mistral_conversations.Name:
		modelDriver = mistral_conversations.New(nil)
	case perplexity_agent.Name:
		modelDriver = perplexity_agent.New(nil)
	default:
		return driver.Capabilities{}, false
	}
	target := driver.CapabilityTarget{Provider: snapshot.Config.CatalogProvider, Protocol: snapshot.Config.Adapter, Model: snapshot.Config.Model}
	return webtool.ApplyCapabilityOverrides(driver.CapabilitiesFor(modelDriver, target), snapshot.Config.WebCapabilities), true
}

func (r *runtime) loadManager() (config.Loaded, *provider.Manager, error) {
	workspace, err := r.resolveWorkspace()
	if err != nil {
		return config.Loaded{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	configPath := r.configPath
	if configPath == "" {
		configPath = os.Getenv("EYLU_CONFIG")
	}
	loaded, err := config.Load(config.LoadOptions{ExplicitPath: configPath, Workspace: workspace, Environ: os.Environ()})
	if err != nil {
		return config.Loaded{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	r.rememberProviderAPIKeys(loaded.Config)
	r.workspace = loaded.Workspace
	manager, err := provider.NewManagerWithStore(loaded.Store)
	if err != nil {
		return config.Loaded{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	return loaded, manager, nil
}

func providerAPIKey(providerConfig config.ProviderConfig) string {
	if apiKey := os.Getenv("EYLU_API_KEY"); apiKey != "" {
		return apiKey
	}
	return providerConfig.APIKey
}

func (r *runtime) rememberProviderAPIKeys(cfg config.Config) {
	apiKeys := make([]string, 0, len(cfg.Providers))
	for _, providerConfig := range cfg.Providers {
		if providerConfig.APIKey != "" {
			apiKeys = append(apiKeys, providerConfig.APIKey)
		}
	}
	r.secretMu.Lock()
	r.apiKeys = apiKeys
	r.secretMu.Unlock()
}

func (r *runtime) redact(value string) string {
	r.secretMu.RLock()
	secrets := append([]string(nil), r.apiKeys...)
	r.secretMu.RUnlock()
	secrets = append(secrets, os.Getenv("EYLU_API_KEY"))
	return logging.Redact(value, secrets...)
}

func (r *runtime) resolveWorkspace() (string, error) {
	workspace := strings.TrimSpace(r.workspace)
	if workspace == "" {
		workspace = strings.TrimSpace(os.Getenv("EYLU_WORKSPACE"))
	}
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve current workspace: %w", err)
		}
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func (r *runtime) printError(err error) {
	var typed *protocol.Error
	if !errors.As(err, &typed) {
		typed = &protocol.Error{Code: protocol.ErrProtocol, Message: err.Error()}
	}
	message := r.redact(typed.Message)
	if r.output == "json" || r.output == "jsonl" {
		payload := map[string]any{"error": map[string]any{"code": typed.Code, "message": message, "retryable": typed.Retryable}}
		if r.output == "jsonl" {
			payload["type"] = "error"
		}
		_ = json.NewEncoder(r.stderr).Encode(payload)
		return
	}
	fmt.Fprintf(r.stderr, "error [%s]: %s\n", typed.Code, message)
}

func exitCode(err error) int {
	var typed *protocol.Error
	if !errors.As(err, &typed) {
		return exitInternal
	}
	switch typed.Code {
	case protocol.ErrConfig, protocol.ErrCredential:
		return exitConfig
	case protocol.ErrNetwork, protocol.ErrTimeout, protocol.ErrCancelled:
		return exitNetwork
	case protocol.ErrAuth, protocol.ErrRateLimit, protocol.ErrProvider, protocol.ErrContextWindow:
		return exitProvider
	default:
		return exitInternal
	}
}
