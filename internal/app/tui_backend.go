package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/logging"
	"Eylu/internal/metrics"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/routing"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
	"Eylu/internal/ui"
)

type tuiBackend struct {
	mu           sync.Mutex
	runtime      *runtime
	conversation *agent.Conversation
	manager      *provider.Manager
	opts         chatOptions
	skills       *skill.Registry
	skillSession *skill.Session
}

type tuiAuditSink struct {
	operationID string
	emit        func(ui.Event)
}

func (s *tuiAuditSink) Record(record tool.AuditRecord) {
	s.emit(ui.Event{OperationID: s.operationID, Kind: ui.EventToolAudit, ToolAudit: &ui.ToolAudit{
		CallID: record.CallID, DurationMS: record.DurationMS, Decision: string(record.Decision), Risk: string(record.Risk), ExitCode: record.ExitCode,
	}})
}

func (r *runtime) runTUI(ctx context.Context, conversation *agent.Conversation, manager *provider.Manager, opts chatOptions) error {
	cfg := manager.Config()
	registry, session, err := r.loadSkillRuntime(cfg, opts, conversation)
	if err != nil {
		return err
	}
	backend := &tuiBackend{runtime: r, conversation: conversation, manager: manager, opts: opts, skills: registry, skillSession: session}
	return ui.Run(backend, ui.Options{
		Context: ctx, Input: r.stdin, Output: r.stdout, NoAnimation: opts.noAnimation || os.Getenv("TERM") == "dumb",
		NoColor: os.Getenv("NO_COLOR") != "", Clock: nil,
	})
}

func (b *tuiBackend) Snapshot(context.Context) (ui.Snapshot, error) {
	b.mu.Lock()
	opts := b.opts
	b.mu.Unlock()
	cfg := b.manager.Config()
	mode := cfg.PermissionMode
	if opts.mode != "" {
		mode = opts.mode
	}
	active, err := b.manager.Active()
	if opts.provider != "" {
		if selected, ok := b.manager.Snapshot(opts.provider); ok {
			active = selected
			err = nil
		}
	}
	if err != nil {
		return ui.Snapshot{}, err
	}
	state := b.conversation.ExportState()
	if state.Provider.Name != "" && opts.provider == "" {
		if selected, ok := b.manager.Snapshot(state.Provider.Name); ok {
			active = selected
			active.Config.Model = state.Provider.Model
		}
	}
	snapshot := ui.Snapshot{
		SessionID: b.conversation.SessionID(), Mode: mode, Provider: active.Name, Model: active.Config.Model,
		Context: b.conversation.ContextReport(),
	}
	managerActive, _ := b.manager.Active()
	for _, item := range b.manager.List() {
		snapshot.Providers = append(snapshot.Providers, ui.ProviderItem{
			Name: item.Name, Adapter: item.Config.Adapter, BaseURL: item.Config.BaseURL, Model: item.Config.Model,
			ContextWindow: item.Config.ContextWindow, Active: item.Name == managerActive.Name,
		})
	}
	activated := b.conversation.ActivatedSkillDigests()
	for _, record := range b.skills.Records() {
		snapshot.Skills = append(snapshot.Skills, ui.SkillItem{
			Name: record.Skill.Name, Description: record.Skill.Description, Source: record.Skill.Source.String(), Status: string(record.Status),
			ShadowedBy: record.ShadowedBy, Reason: record.Reason, Activated: activated[record.Skill.Name] != "",
		})
	}
	return snapshot, nil
}

func (b *tuiBackend) Submit(ctx context.Context, operationID, prompt string, emit func(ui.Event)) error {
	b.mu.Lock()
	opts := b.opts
	b.mu.Unlock()
	cfg := b.manager.Config()
	contextReport := b.conversation.ContextReport()
	estimator := contextledger.ApproxEstimator{BytesPerToken: cfg.TokenBytesPerToken}
	estimatedInput := contextReport.InputTokens + estimator.Estimate(prompt)
	modelRuntime, routeDecision, err := b.runtime.resolveRuntimeForPrompt(b.manager, opts, prompt, estimatedInput+cfg.ReservedOutputTokens, estimatedInput, cfg.ReservedOutputTokens, true)
	if err != nil {
		return err
	}
	if routeDecision != nil {
		emit(ui.Event{OperationID: operationID, Kind: ui.EventNotice, Notice: fmt.Sprintf("Routed %s task to %s.", routeDecision.Task, routeDecision.Provider)})
	}
	modeName := cfg.PermissionMode
	if opts.mode != "" {
		modeName = opts.mode
	}
	mode, err := policy.ParseMode(modeName)
	if err != nil {
		return err
	}
	modelRuntime.PermissionMode = mode.String()
	modelRuntime.SkillCatalog = b.skills.Catalog()
	if err := b.runtime.configureMCPRuntime(ctx, cfg, &modelRuntime); err != nil {
		return err
	}
	configureTUIContextRuntime(&modelRuntime, cfg, operationID, emit)
	task := routing.Classify(prompt)
	if routeDecision != nil {
		task = routeDecision.Task
	} else if opts.task != "" {
		task = opts.task
	}
	observation := b.runtime.metricCollector().Begin(metrics.Metadata{
		RequestID: operationID, SessionID: b.conversation.SessionID(), Provider: modelRuntime.Provider.Name,
		ProviderGeneration: modelRuntime.Provider.Generation, Model: modelRuntime.Provider.Config.Model, Task: task,
		InputCostPerMillion:  modelRuntime.Provider.Config.Routing.InputCostPerMillion,
		OutputCostPerMillion: modelRuntime.Provider.Config.Routing.OutputCostPerMillion,
	})
	contextEvents := modelRuntime.ContextEvent
	modelRuntime.ContextEvent = func(event contextledger.Event) {
		observation.ObserveContextEvent(event)
		if contextEvents != nil {
			contextEvents(event)
		}
	}
	confirm := b.confirmTools(operationID, opts.approve, emit)
	executor, err := b.runtime.toolExecutorWith(cfg, opts, b.skills, b.skillSession, confirm, &tuiAuditSink{operationID: operationID, emit: emit})
	if err != nil {
		return err
	}
	executor.SessionID, executor.ProviderName = b.conversation.SessionID(), modelRuntime.Provider.Name
	executor.ProviderGeneration, executor.Model = modelRuntime.Provider.Generation, modelRuntime.Provider.Config.Model
	emit(ui.Event{OperationID: operationID, Kind: ui.EventState, State: ui.StateConnecting})
	modelEvents := func(event protocol.ModelEvent) error {
		observation.ObserveModelEvent(event)
		switch event.Kind {
		case protocol.EventResponseStart:
			emit(ui.Event{OperationID: operationID, Kind: ui.EventState, State: ui.StateWaitingFirstToken})
		case protocol.EventTextDelta:
			emit(ui.Event{OperationID: operationID, Kind: ui.EventTextDelta, Delta: event.Delta})
		case protocol.EventToolStart:
			emit(ui.Event{OperationID: operationID, Kind: ui.EventToolStart, ToolCall: event.ToolCall})
		case protocol.EventToolResult:
			emit(ui.Event{OperationID: operationID, Kind: ui.EventToolResult, ToolResult: event.ToolResult})
		}
		return nil
	}
	overallTimeout := time.Duration(cfg.MaxTurns) * modelRuntime.Timeout
	requestCtx, cancel := context.WithTimeout(ctx, overallTimeout)
	defer cancel()
	response, err := b.conversation.Run(requestCtx, prompt, modelRuntime, executor, agent.LoopOptions{MaxTurns: cfg.MaxTurns, MaxTotalTokens: cfg.MaxTotalTokens, RequestID: observation.RequestID()}, true, modelEvents)
	metric := observation.Finish(response.Usage, err)
	emit(ui.Event{OperationID: operationID, Kind: ui.EventNotice, Notice: fmt.Sprintf("Completed in %d ms; first token %d ms; tool success %.0f%%.", metric.DurationMS, metric.FirstTokenMS, metric.ToolSuccessRate*100)})
	if b.runtime.session != nil {
		if syncErr := b.runtime.session.Sync(b.conversation, b.manager, opts, err); err == nil {
			err = syncErr
		}
	}
	report := b.conversation.ContextReport()
	emit(ui.Event{OperationID: operationID, Kind: ui.EventContext, Context: &report})
	return err
}

func configureTUIContextRuntime(modelRuntime *agent.Runtime, cfg config.Config, operationID string, emit func(ui.Event)) {
	modelRuntime.Workspace = cfg.Workspace
	modelRuntime.TokenEstimator = contextledger.ApproxEstimator{BytesPerToken: cfg.TokenBytesPerToken}
	modelRuntime.OutputReserveTokens = cfg.ReservedOutputTokens
	modelRuntime.ContextRecentRounds = cfg.ContextRecentRounds
	modelRuntime.MaxProjectMapBytes = cfg.MaxProjectMapBytes
	modelRuntime.MaxToolContextBytes = cfg.MaxToolContextBytes
	modelRuntime.SkillCatalogPageBytes = cfg.SkillCatalogPageBytes
	modelRuntime.MaxSummaryBytes = cfg.MaxSummaryBytes
	modelRuntime.ContextEvent = func(event contextledger.Event) {
		if event.Kind == contextledger.EventCompression && event.Compression != nil {
			emit(ui.Event{OperationID: operationID, Kind: ui.EventNotice, Notice: fmt.Sprintf("Context compressed: %d to %d tokens, %d turns summarized.", event.Compression.BeforeTokens, event.Compression.AfterTokens, event.Compression.OmittedTurns)})
		}
	}
}

func (b *tuiBackend) confirmTools(operationID string, approve bool, emit func(ui.Event)) tool.ConfirmFunc {
	return func(ctx context.Context, request policy.Request, outcome policy.Outcome) (bool, error) {
		if approve {
			return true, nil
		}
		preview := logging.Redact(string(request.Input), os.Getenv("EYLU_API_KEY"))
		if len(preview) > 512 {
			preview = preview[:512] + "..."
		}
		response := make(chan bool, 1)
		emit(ui.Event{OperationID: operationID, Kind: ui.EventApproval, Approval: &ui.ApprovalRequest{
			Tool: request.Tool, Risk: string(outcome.Risk), Summary: preview, Reason: outcome.Reason, Warning: outcome.Warning,
			Step: request.ConfirmationStep, Total: request.ConfirmationTotal, Response: response,
		}})
		select {
		case allowed := <-response:
			return allowed, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func (b *tuiBackend) Command(ctx context.Context, line string) (string, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil
	}
	switch fields[0] {
	case "/new":
		b.mu.Lock()
		opts := b.opts
		b.mu.Unlock()
		old, current, err := b.runtime.rotateSession(b.conversation, b.manager, opts)
		if err != nil {
			return "", err
		}
		b.skillSession = skill.NewSession(b.skills, nil)
		return fmt.Sprintf("Closed session %s. New session %s.", old, current), nil
	case "/mode":
		if len(fields) != 2 {
			return "", fmt.Errorf("usage: /mode manual|plan|auto|full")
		}
		mode, err := policy.ParseMode(fields[1])
		if err != nil {
			return "", err
		}
		b.mu.Lock()
		b.opts.mode = mode.String()
		opts := b.opts
		b.mu.Unlock()
		if b.runtime.session != nil {
			if err := b.runtime.session.Sync(b.conversation, b.manager, opts, nil); err != nil {
				return "", err
			}
		}
		return "Permission mode: " + mode.String(), nil
	case "/skill":
		if len(fields) != 2 {
			return "", fmt.Errorf("usage: /skill <name>")
		}
		activation := tool.NewActivateSkill(b.skills, b.skillSession)
		input, _ := json.Marshal(map[string]string{"name": fields[1]})
		result := activation.Execute(ctx, input)
		if result.IsError {
			return "", fmt.Errorf("%s", result.Content)
		}
		if result.Metadata != nil {
			result.Metadata["trigger"] = "user"
		}
		b.conversation.RegisterSkillResult(result)
		b.mu.Lock()
		opts := b.opts
		b.mu.Unlock()
		if b.runtime.session != nil {
			if err := b.runtime.session.Sync(b.conversation, b.manager, opts, nil); err != nil {
				return "", err
			}
		}
		return result.Content, nil
	case "/provider":
		if len(fields) != 3 {
			return "", fmt.Errorf("usage: /provider use|delete <name>")
		}
		switch fields[1] {
		case "use":
			if err := b.UseProvider(ctx, fields[2]); err != nil {
				return "", err
			}
			return "Active provider: " + fields[2], nil
		case "delete":
			if err := b.DeleteProvider(ctx, fields[2]); err != nil {
				return "", err
			}
			return "Provider " + fields[2] + " deleted.", nil
		default:
			return "", fmt.Errorf("unknown provider command %s", fields[1])
		}
	case "/model":
		if len(fields) != 2 {
			return "", fmt.Errorf("usage: /model <id>")
		}
		snapshot, err := b.manager.Active()
		if err != nil {
			return "", err
		}
		if err := b.SetModel(ctx, snapshot.Name, fields[1]); err != nil {
			return "", err
		}
		return "Model: " + fields[1], nil
	default:
		return "", fmt.Errorf("unknown command %s", fields[0])
	}
}

func (b *tuiBackend) UpsertProvider(_ context.Context, form ui.ProviderForm) error {
	candidate := config.ProviderConfig{
		Adapter: form.Adapter, BaseURL: form.BaseURL, Model: form.Model, ContextWindow: form.ContextWindow,
		TimeoutSeconds: 60, Credential: config.CredentialRef{Type: "env", Env: "EYLU_API_KEY"},
	}
	if form.OriginalName != "" {
		if current, ok := b.manager.Get(form.OriginalName); ok {
			candidate.Credential = current.Credential
			candidate.Headers = current.Headers
			candidate.TimeoutSeconds = current.TimeoutSeconds
			candidate.Routing = current.Routing
		}
	}
	if form.APIKey != "" {
		ref := config.CredentialRef{Type: "keyring", Service: "eylu", Account: "provider:" + form.Name}
		if err := b.runtime.credentials.Save(ref, form.APIKey); err != nil {
			ref.Type = "memory"
			if memoryErr := b.runtime.credentials.Save(ref, form.APIKey); memoryErr != nil {
				return memoryErr
			}
		}
		candidate.Credential = ref
	}
	if err := b.manager.Upsert(form.Name, candidate, true); err != nil {
		return err
	}
	if form.OriginalName != "" && form.OriginalName != form.Name {
		if err := b.manager.Delete(form.OriginalName, form.Name); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.opts.provider = ""
	b.mu.Unlock()
	return nil
}

func (b *tuiBackend) DeleteProvider(_ context.Context, name string) error {
	replacement := ""
	active, _ := b.manager.Active()
	if active.Name == name {
		for _, item := range b.manager.List() {
			if item.Name != name {
				replacement = item.Name
				break
			}
		}
	}
	return b.manager.Delete(name, replacement)
}

func (b *tuiBackend) UseProvider(_ context.Context, name string) error {
	if err := b.manager.Use(name); err != nil {
		return err
	}
	b.mu.Lock()
	b.opts.provider = ""
	b.mu.Unlock()
	return nil
}

func (b *tuiBackend) SetModel(_ context.Context, providerName, modelID string) error {
	candidate, ok := b.manager.Get(providerName)
	if !ok {
		return fmt.Errorf("provider %q does not exist", providerName)
	}
	candidate.Model = modelID
	active, _ := b.manager.Active()
	return b.manager.Upsert(providerName, candidate, active.Name == providerName)
}

func (b *tuiBackend) FetchModels(ctx context.Context, name string) ([]string, error) {
	var snapshot provider.Snapshot
	var ok bool
	if name != "" {
		snapshot, ok = b.manager.Snapshot(name)
	}
	if !ok {
		var err error
		snapshot, err = b.manager.Active()
		if err != nil {
			return nil, err
		}
	}
	key := os.Getenv("EYLU_API_KEY")
	if key == "" {
		var err error
		key, err = b.runtime.credentials.Resolve(snapshot.Config.Credential)
		if err != nil {
			return nil, err
		}
	}
	timeout := snapshot.Config.Timeout(30 * time.Second)
	listCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return provider.NewModelLister(&http.Client{Timeout: timeout}).List(listCtx, snapshot.Config.BaseURL, key, snapshot.Config.Headers)
}
