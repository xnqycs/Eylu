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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"Eylu/internal/agent"
	"Eylu/internal/buildinfo"
	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/driver/openai_chat"
	"Eylu/internal/driver/openai_responses"
	"Eylu/internal/logging"
	"Eylu/internal/mcpclient"
	"Eylu/internal/metrics"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/routing"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
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
	stdin         io.Reader
	stdout        io.Writer
	stderr        io.Writer
	configPath    string
	workspace     string
	output        string
	credentials   *provider.CredentialStore
	inputReader   *bufio.Reader
	trustPrompted map[string]bool
	session       *sessionRuntime
	metrics       *metrics.Collector
	mcp           *mcpclient.Manager
	mcpKey        string
}

func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	r := &runtime{stdin: stdin, stdout: stdout, stderr: stderr, credentials: provider.NewCredentialStore(), trustPrompted: make(map[string]bool), metrics: &metrics.Collector{}}
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
		RunE: func(_ *cobra.Command, args []string) error {
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
	resume           bool
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
		RunE: func(_ *cobra.Command, args []string) error {
			return r.executeChatCommand(ctx, args, opts)
		},
	}
	bindChatFlags(cmd, &opts)
	return cmd
}

func (r *runtime) executeChatCommand(ctx context.Context, args []string, opts chatOptions) error {
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
	cmd.Flags().BoolVar(&opts.resume, "resume", false, "resume the most recent session in this workspace")
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
	conversation, err := r.openConversation(manager, &opts)
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
			Credential: config.CredentialRef{Type: "env", Env: "EYLU_API_KEY"},
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
	cfg := manager.Config()
	estimator := contextledger.ApproxEstimator{BytesPerToken: cfg.TokenBytesPerToken}
	report := conversation.ContextReport()
	estimatedInput := report.InputTokens + estimator.Estimate(prompt)
	stream := r.output == "text" || r.output == "jsonl"
	modelRuntime, routeDecision, err := r.resolveRuntimeForPrompt(manager, opts, prompt, estimatedInput+cfg.ReservedOutputTokens, estimatedInput, cfg.ReservedOutputTokens, stream)
	if err != nil {
		return err
	}
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
	modelRuntime.Workspace = cfg.Workspace
	modelRuntime.TokenEstimator = contextledger.ApproxEstimator{BytesPerToken: cfg.TokenBytesPerToken}
	modelRuntime.OutputReserveTokens = cfg.ReservedOutputTokens
	modelRuntime.ContextRecentRounds = cfg.ContextRecentRounds
	modelRuntime.MaxProjectMapBytes = cfg.MaxProjectMapBytes
	modelRuntime.MaxToolContextBytes = cfg.MaxToolContextBytes
	modelRuntime.SkillCatalogPageBytes = cfg.SkillCatalogPageBytes
	modelRuntime.MaxSummaryBytes = cfg.MaxSummaryBytes
	if err := r.configureMCPRuntime(ctx, cfg, &modelRuntime); err != nil {
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
	skillRegistry, skillSession, err := r.loadSkillRuntime(cfg, opts, conversation)
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
			}
			return nil
		}
	}
	audit := tool.AuditSink(&toolAuditWriter{writer: r.stderr})
	if r.output == "jsonl" {
		audit = &toolAuditWriter{writer: r.stdout, jsonl: true}
	}
	executor, err := r.toolExecutorWith(cfg, opts, skillRegistry, skillSession, r.confirmTools(opts.approve), audit)
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
	response, err := conversation.Run(requestCtx, prompt, modelRuntime, executor, agent.LoopOptions{MaxTurns: cfg.MaxTurns, MaxTotalTokens: cfg.MaxTotalTokens, RequestID: observation.RequestID()}, stream, emit)
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
		fmt.Fprintf(r.stderr, "[metrics] timestamp=%s session_id=%s request_id=%s provider_name=%s provider_generation=%d model=%s first_token_ms=%d duration_ms=%d tool_success_rate=%.3f compression_count=%d input_tokens=%d output_tokens=%d estimated_cost=%.8f error_code=%s\n",
			metric.Timestamp.Format(time.RFC3339Nano), metric.SessionID, metric.RequestID, metric.Provider, metric.ProviderGeneration, metric.Model, metric.FirstTokenMS, metric.DurationMS, metric.ToolSuccessRate, metric.CompressionCount, metric.Usage.InputTokens, metric.Usage.OutputTokens, metric.EstimatedCost, metric.ErrorCode)
	}
}

func (r *runtime) toolExecutorWith(cfg config.Config, opts chatOptions, skillRegistry *skill.Registry, skillSession *skill.Session, confirm tool.ConfirmFunc, audit tool.AuditSink) (*tool.Executor, error) {
	readFile, err := tool.NewReadFile(cfg.Workspace, cfg.MaxReadBytes)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize read_file", Cause: err}
	}
	writeFile, err := tool.NewWriteFile(cfg.Workspace)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize write_file", Cause: err}
	}
	bashTool, err := tool.NewBash(cfg.Workspace, cfg.MaxOutputBytes, nil)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize bash", Cause: err}
	}
	bashTool.AllowEnvironment(cfg.ShellEnvironment)
	editFile, err := tool.NewEditFile(cfg.Workspace, int64(cfg.MaxReadBytes))
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize edit_file", Cause: err}
	}
	index, err := tool.NewRepositoryIndex(cfg.Workspace)
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
	registered := []tool.Tool{readFile, writeFile, bashTool, editFile, searchCode, listDirectory}
	if r.mcp != nil {
		registered = append(registered, r.mcp.Tools()...)
	}
	if skillRegistry != nil && len(skillRegistry.Active()) > 0 {
		registered = append(registered, tool.NewActivateSkill(skillRegistry, skillSession), tool.NewReadSkillResource(skillSession))
	}
	return &tool.Executor{
		Registry: tool.NewRegistry(registered...), Policy: checker,
		Confirm: confirm, Audit: audit, Workspace: cfg.Workspace,
		Timeout: time.Duration(cfg.ToolTimeoutSec) * time.Second, MaxOutputBytes: cfg.MaxOutputBytes, MaxParallelTools: cfg.MaxParallelTools,
	}, nil
}

func (r *runtime) confirmTools(approve bool) tool.ConfirmFunc {
	return func(ctx context.Context, request policy.Request, outcome policy.Outcome) (bool, error) {
		if approve {
			return true, nil
		}
		if !isTerminal(r.stdin) {
			return false, nil
		}
		reader := r.inputReader
		if reader == nil {
			reader = bufio.NewReader(r.stdin)
		}
		preview := logging.Redact(string(request.Input), os.Getenv("EYLU_API_KEY"))
		if len(preview) > 512 {
			preview = preview[:512] + "..."
		}
		label := "CONFIRM"
		if outcome.Warning {
			label = "DANGER"
		}
		fmt.Fprintf(r.stderr, "%s [%d/%d] approve %s tool %s? %s\n%s\n[y/N]: ", label, request.ConfirmationStep, request.ConfirmationTotal, outcome.Risk, request.Tool, outcome.Reason, preview)
		answer, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
	}
}

func (r *runtime) resolveRuntimeForPrompt(manager *provider.Manager, opts chatOptions, prompt string, requiredContext, estimatedInput, estimatedOutput int, stream bool) (agent.Runtime, *routing.Decision, error) {
	var snapshot provider.Snapshot
	var decision *routing.Decision
	var err error
	if opts.provider != "" {
		var ok bool
		snapshot, ok = manager.Snapshot(opts.provider)
		if !ok {
			return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", opts.provider)}
		}
	} else {
		routeMode := opts.routeMode
		if routeMode == "" {
			routeMode = manager.Config().RoutingMode
		}
		switch routeMode {
		case "fixed":
			snapshot, err = manager.Active()
		case "auto":
			task := opts.task
			if task == "" {
				task = routing.Classify(prompt)
			}
			if !routing.ValidTask(task) {
				return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid routing task %q", task)}
			}
			routeDecision, routeErr := routing.Select(manager.List(), routing.Request{
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
	providerConfig := snapshot.Config
	if opts.model != "" {
		providerConfig.Model = opts.model
	}
	if opts.baseURL != "" {
		providerConfig.BaseURL = opts.baseURL
	}
	if opts.adapter != "" && opts.adapter != openai_responses.Name {
		providerConfig.Adapter = opts.adapter
	}
	if err := config.ValidateProvider(snapshot.Name, providerConfig); err != nil {
		return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	snapshot.Config = providerConfig
	apiKey := os.Getenv("EYLU_API_KEY")
	if apiKey == "" {
		apiKey, err = r.credentials.Resolve(providerConfig.Credential)
		if err != nil {
			return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrCredential, Message: "provider credential is unavailable", Cause: err}
		}
	}
	requestTimeout := providerConfig.Timeout(60 * time.Second)
	if opts.timeout > 0 {
		requestTimeout = opts.timeout
	}
	httpClient := &http.Client{Timeout: requestTimeout}
	registry := driver.NewRegistry(openai_responses.New(httpClient), openai_chat.New(httpClient))
	modelDriver, err := registry.Get(providerConfig.Adapter)
	if err != nil {
		return agent.Runtime{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	return agent.Runtime{Provider: snapshot, APIKey: apiKey, Driver: modelDriver, Timeout: requestTimeout}, decision, nil
}

func knownDriverCapabilities(adapter string) (driver.Capabilities, bool) {
	switch adapter {
	case openai_responses.Name:
		return driver.Capabilities{TextStreaming: true, ToolCalling: true, ParallelTools: true, Reasoning: true, ImageInput: true, RemoteSession: true}, true
	case openai_chat.Name:
		return driver.Capabilities{TextStreaming: true, ToolCalling: true, ParallelTools: true, ImageInput: true}, true
	default:
		return driver.Capabilities{}, false
	}
}

func (r *runtime) loadManager() (config.Loaded, *provider.Manager, error) {
	workspace := r.workspace
	if workspace == "" {
		workspace, _ = os.Getwd()
	}
	configPath := r.configPath
	if configPath == "" {
		configPath = os.Getenv("EYLU_CONFIG")
	}
	loaded, err := config.Load(config.LoadOptions{ExplicitPath: configPath, Workspace: workspace, Environ: os.Environ()})
	if err != nil {
		return config.Loaded{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	manager, err := provider.NewManager(loaded.Path, loaded.Config, nil)
	if err != nil {
		return config.Loaded{}, nil, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	return loaded, manager, nil
}

func (r *runtime) printError(err error) {
	var typed *protocol.Error
	if !errors.As(err, &typed) {
		typed = &protocol.Error{Code: protocol.ErrProtocol, Message: err.Error()}
	}
	message := logging.Redact(typed.Message, os.Getenv("EYLU_API_KEY"))
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
	case protocol.ErrAuth, protocol.ErrRateLimit, protocol.ErrProvider:
		return exitProvider
	default:
		return exitInternal
	}
}
