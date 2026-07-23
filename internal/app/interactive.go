package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/tool"
	"Eylu/internal/ui"
)

var errQuit = errors.New("quit")

type inputLineResult struct {
	line string
	err  error
}

func (r *runtime) runInteractive(ctx context.Context, opts chatOptions) (returnErr error) {
	manager, err := r.prepareManager(ctx, opts)
	if err != nil {
		return err
	}
	conversation, err := r.openConversation(ctx, manager, &opts)
	if err != nil {
		return err
	}
	probed, err := r.probeStartupModelLimits(ctx, manager, opts)
	if err != nil {
		return err
	}
	conversation.ApplyProviderSnapshot(probed)
	useTUI := !opts.noTUI && isTerminal(r.stdout) && !strings.EqualFold(os.Getenv("TERM"), "dumb") && r.output == "text"
	return r.runInteractiveFrontend(ctx, conversation, manager, opts, useTUI)
}

func (r *runtime) runInteractiveFrontend(ctx context.Context, conversation *agent.Conversation, manager *provider.Manager, opts chatOptions, useTUI bool) (returnErr error) {
	detachMCP := r.attachMCPConversation(conversation)
	defer detachMCP()
	defer func() {
		r.closeSearchTasks()
		if r.session != nil {
			if syncErr := r.session.Sync(conversation, manager, opts, returnErr); returnErr == nil {
				returnErr = syncErr
			}
		}
		returnErr = r.finishInteractive(conversation, returnErr)
	}()
	if useTUI {
		return r.runTUI(ctx, conversation, manager, opts)
	}
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	return r.runLineInteractive(ctx, conversation, manager, opts, interrupts)
}

func (r *runtime) runLineInteractive(ctx context.Context, conversation *agent.Conversation, manager *provider.Manager, opts chatOptions, interrupts <-chan os.Signal) error {
	reader := bufio.NewReader(r.stdin)
	r.inputMu.Lock()
	r.inputReader = reader
	r.inputInterrupts = interrupts
	r.inputMu.Unlock()
	defer func() {
		r.inputMu.Lock()
		if r.inputReader == reader {
			r.inputReader = nil
			r.inputRead = nil
			r.inputInterrupts = nil
		}
		r.inputMu.Unlock()
	}()
	fmt.Fprintf(r.stdout, "Eylu session %s\n", conversation.SessionID())
	if err := printInteractiveHistory(r.stdout, conversationHistory(conversation.ExportState())); err != nil {
		return err
	}
	fmt.Fprintln(r.stdout, "Type /help for commands.")
	for {
		fmt.Fprint(r.stdout, "> ")
		line, readErr := r.readInteractiveLineWithInterrupts(ctx, reader, interrupts)
		line = strings.TrimSpace(line)
		if errors.Is(readErr, errQuit) {
			return nil
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if line == "" {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "/") {
			commandErr := r.handleSlashCommand(ctx, reader, line, conversation, manager, &opts)
			if errors.Is(commandErr, errQuit) {
				return nil
			}
			if commandErr != nil {
				r.printError(commandErr)
			}
		} else {
			commandErr := r.sendInteractive(ctx, conversation, manager, line, opts, interrupts)
			if errors.Is(commandErr, errQuit) {
				return nil
			}
			if commandErr != nil {
				r.printError(commandErr)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func printInteractiveHistory(writer io.Writer, history []ui.HistoryItem) error {
	visible := make([]ui.HistoryItem, 0, len(history))
	for _, item := range history {
		if item.Kind == ui.HistoryTool && item.ToolCall != nil && item.ToolCall.Name == "todolist" {
			continue
		}
		visible = append(visible, item)
	}
	if len(visible) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(writer, "Restored conversation history:"); err != nil {
		return err
	}
	for _, item := range visible {
		switch item.Kind {
		case ui.HistoryMessage:
			if _, err := fmt.Fprintf(writer, "[%s]\n%s\n\n", item.Role, item.Text); err != nil {
				return err
			}
		case ui.HistoryTool:
			name, callID := "unknown_tool", ""
			if item.ToolCall != nil {
				name, callID = item.ToolCall.Name, item.ToolCall.ID
			}
			status := "interrupted"
			truncated := false
			if item.ToolResult != nil {
				status = "done"
				if item.ToolResult.IsError {
					status = "failed"
				}
				truncated = item.ToolResult.Truncated
				if callID == "" {
					callID = item.ToolResult.CallID
				}
			}
			if _, err := fmt.Fprintf(writer, "[tool] %s status=%s call_id=%s truncated=%t\n\n", name, status, callID, truncated); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runtime) finishInteractive(conversation *agent.Conversation, runErr error) error {
	if runErr != nil || r.output != "text" {
		return runErr
	}
	fmt.Fprintf(r.stdout, "Resume this session with:\neylu --resume %s\n", conversation.SessionID())
	return nil
}

func (r *runtime) sendInteractive(ctx context.Context, conversation *agent.Conversation, manager *provider.Manager, prompt string, opts chatOptions, interrupts <-chan os.Signal) error {
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- r.sendPrompt(requestCtx, conversation, manager, prompt, opts)
	}()
	return r.waitForInteractiveResult(cancel, result, interrupts)
}

func (r *runtime) waitForInteractiveResult(cancel context.CancelFunc, result <-chan error, interrupts <-chan os.Signal) error {
	cancelled := false
	for {
		select {
		case err := <-result:
			return err
		case <-interrupts:
			if cancelled {
				return errQuit
			}
			cancelled = true
			cancel()
			fmt.Fprintln(r.stderr, "Cancelling current request. Press Ctrl-C again to exit.")
		}
	}
}

func (r *runtime) handleSlashCommand(ctx context.Context, reader *bufio.Reader, line string, conversation *agent.Conversation, manager *provider.Manager, opts *chatOptions) error {
	fields := strings.Fields(line)
	command := fields[0]
	switch command {
	case "/help":
		fmt.Fprintln(r.stdout, "/new  /compact  /agents [filter]  /tasks  /context  /skills  /skill <name>  /providers  /provider add|edit|delete|use  /model [id]  /effort [level]  /gradient [on|off]  /mode manual|plan|auto|full  /quit")
		return nil
	case "/quit":
		return errQuit
	case "/new":
		old, current, err := r.rotateSession(ctx, conversation, manager, *opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(r.stdout, "Closed session %s. New session %s.\n", old, current)
		return nil
	case "/context":
		return contextledger.RenderText(r.stdout, conversation.ContextReport())
	case "/compact":
		if len(fields) != 1 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /compact"}
		}
		modelRuntime, err := r.resolveCompactionRuntime(ctx, manager, conversation, *opts, "")
		if err != nil {
			return err
		}
		event, err := conversation.Compact(ctx, modelRuntime)
		if err != nil {
			return err
		}
		if r.session != nil {
			if err := r.session.Sync(conversation, manager, *opts, nil); err != nil {
				return err
			}
		}
		if event.Noop {
			fmt.Fprintln(r.stdout, "Context is already compact.")
		} else {
			fmt.Fprintln(r.stdout, formatCompactionCompletion(event))
		}
		return nil
	case "/tasks":
		renderTodoListText(r.stdout, conversation.TodoList())
		return nil
	case "/agents":
		filter := strings.ToLower(strings.TrimSpace(strings.Join(fields[1:], " ")))
		tasks := r.agentTaskManager(manager.Config().MaxParallelAgents).Snapshots(conversation.SessionID())
		sort.SliceStable(tasks, func(i, j int) bool {
			leftActive, rightActive := activeAgentStatus(string(tasks[i].Status)), activeAgentStatus(string(tasks[j].Status))
			if leftActive != rightActive {
				return leftActive
			}
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		})
		matched := 0
		for _, task := range tasks {
			title := agentTaskTitle(task.Prompt)
			searchable := strings.ToLower(strings.Join([]string{task.ID, task.SubagentType, string(task.Status), title}, " "))
			if filter != "" && !strings.Contains(searchable, filter) {
				continue
			}
			fmt.Fprintf(r.stdout, "%s  %-7s  %-18s  %s\n", shortAgentTaskID(task.ID), task.SubagentType, task.Status, title)
			matched++
		}
		if matched == 0 {
			fmt.Fprintln(r.stdout, "No agents in this session.")
		}
		return nil
	case "/providers":
		r.printProviders(manager)
		return nil
	case "/skills":
		return r.handleSkillsSlash(ctx, conversation, manager.Config(), *opts, r.synchronousInputInterrupts())
	case "/skill":
		if len(fields) != 2 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /skill <name>"}
		}
		if err := r.activateSkillSlash(ctx, conversation, manager.Config(), *opts, fields[1], r.synchronousInputInterrupts()); err != nil {
			return err
		}
		if r.session != nil {
			return r.session.Sync(conversation, manager, *opts, nil)
		}
		return nil
	case "/provider":
		if err := r.handleProviderSlash(ctx, reader, fields, manager, opts); err != nil {
			return err
		}
		if active, activeErr := manager.Active(); activeErr == nil {
			probed, probeErr := r.probeProviderModelLimits(ctx, manager, active.Name)
			if probeErr != nil {
				return probeErr
			}
			conversation.ApplyProviderSnapshot(probed)
		}
		if r.session != nil {
			return r.session.Sync(conversation, manager, *opts, nil)
		}
		return nil
	case "/model":
		if err := r.handleModelSlash(ctx, fields, manager); err != nil {
			return err
		}
		if len(fields) == 2 {
			active, err := manager.Active()
			if err != nil {
				return err
			}
			probed, err := r.probeProviderModelLimits(ctx, manager, active.Name)
			if err != nil {
				return err
			}
			confirmed, err := r.confirmModelContextWindow(ctx, reader, manager, probed)
			if err != nil {
				return err
			}
			conversation.ApplyProviderSnapshot(confirmed)
		}
		if r.session != nil {
			return r.session.Sync(conversation, manager, *opts, nil)
		}
		return nil
	case "/effort":
		selected, err := selectedEffortProvider(manager, conversation, *opts)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
		}
		available := config.SupportedReasoningEfforts(selected.Config.Model)
		if len(fields) == 1 {
			fmt.Fprintf(r.stdout, "Reasoning effort: %s; available: %s\n", config.EffectiveReasoningEffort(selected.Config.ReasoningEffort), strings.Join(available, ", "))
			return nil
		}
		if len(fields) != 2 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("usage: /effort <%s>", strings.Join(available, "|"))}
		}
		active, _ := manager.Active()
		updated, err := updateProviderReasoningEffort(manager, selected.Name, fields[1], active.Name == selected.Name)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
		}
		conversation.ApplyProviderSnapshot(updated)
		if r.session != nil {
			if err := r.session.Sync(conversation, manager, *opts, nil); err != nil {
				return err
			}
		}
		fmt.Fprintf(r.stdout, "Reasoning effort: %s\n", config.EffectiveReasoningEffort(updated.Config.ReasoningEffort))
		return nil
	case "/gradient":
		if len(fields) == 1 {
			fmt.Fprintf(r.stdout, "Gradient: %s; available: On, Off\n", gradientStateLabel(manager.Config().GradientEnabled))
			return nil
		}
		if len(fields) != 2 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /gradient on|off"}
		}
		enabled, err := updateGradientSetting(manager, fields[1])
		if err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
		}
		fmt.Fprintf(r.stdout, "Gradient: %s\n", gradientStateLabel(enabled))
		return nil
	case "/mode":
		if len(fields) != 2 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /mode manual|plan|auto|full"}
		}
		mode, err := policy.ParseMode(fields[1])
		if err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		opts.mode = mode.String()
		if r.session != nil {
			if err := r.session.Sync(conversation, manager, *opts, nil); err != nil {
				return err
			}
		}
		fmt.Fprintf(r.stdout, "Permission mode: %s\n", opts.mode)
		return nil
	default:
		return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("unknown command %s", command)}
	}
}

func (r *runtime) askUser(ctx context.Context, request protocol.AskRequest) (protocol.AskResponse, error) {
	reader := r.currentInputReader()
	if reader == nil {
		reader = bufio.NewReader(r.stdin)
	}
	response := protocol.AskResponse{Answers: make(map[string][]string, len(request.Questions))}
	for index, question := range request.Questions {
		fmt.Fprintf(r.stderr, "\n[%d/%d] %s\n%s\n", index+1, len(request.Questions), question.Header, question.Question)
		for optionIndex, option := range question.Options {
			fmt.Fprintf(r.stderr, "  %d. %s - %s\n", optionIndex+1, option.Label, option.Description)
		}
		fmt.Fprintln(r.stderr, "  o. Other - Enter a custom answer")
		for {
			if err := ctx.Err(); err != nil {
				return protocol.AskResponse{}, err
			}
			if question.Multiple {
				fmt.Fprint(r.stderr, "Select one or more choices (comma-separated): ")
			} else {
				fmt.Fprint(r.stderr, "Select one choice: ")
			}
			line, err := r.readInteractiveLine(ctx, reader)
			if err != nil && !errors.Is(err, io.EOF) {
				return protocol.AskResponse{}, err
			}
			answers, custom, valid := parseAskSelection(strings.TrimSpace(line), question)
			if valid && custom {
				fmt.Fprint(r.stderr, "Custom answer: ")
				customValue, customErr := r.readInteractiveLine(ctx, reader)
				if customErr != nil && !errors.Is(customErr, io.EOF) {
					return protocol.AskResponse{}, customErr
				}
				customValue = strings.TrimSpace(customValue)
				if customValue == "" {
					valid = false
				} else {
					answers = append(answers, customValue)
				}
			}
			if valid && len(answers) > 0 {
				response.Answers[question.ID] = answers
				break
			}
			if errors.Is(err, io.EOF) {
				return protocol.AskResponse{}, tool.ErrAskDismissed
			}
			fmt.Fprintln(r.stderr, "Invalid selection. Try again.")
		}
	}
	return response, nil
}

func (r *runtime) readInteractiveLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	return r.readInteractiveLineWithInterrupts(ctx, reader, nil)
}

func (r *runtime) synchronousInputInterrupts() <-chan os.Signal {
	r.inputMu.Lock()
	defer r.inputMu.Unlock()
	return r.inputInterrupts
}

func (r *runtime) currentInputReader() *bufio.Reader {
	r.inputMu.Lock()
	defer r.inputMu.Unlock()
	return r.inputReader
}

func (r *runtime) readInteractiveLineWithInterrupts(ctx context.Context, reader *bufio.Reader, interrupts <-chan os.Signal) (string, error) {
	r.inputMu.Lock()
	pending := r.inputRead
	if pending == nil {
		pending = make(chan inputLineResult, 1)
		r.inputRead = pending
		go func() {
			line, err := reader.ReadString('\n')
			pending <- inputLineResult{line: line, err: err}
		}()
	}
	r.inputMu.Unlock()

	select {
	case result := <-pending:
		r.inputMu.Lock()
		if r.inputRead == pending {
			r.inputRead = nil
		}
		r.inputMu.Unlock()
		return result.line, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-interrupts:
		return "", errQuit
	}
}

func parseAskSelection(value string, question protocol.AskQuestion) ([]string, bool, bool) {
	parts := strings.Split(value, ",")
	if !question.Multiple && len(parts) != 1 {
		return nil, false, false
	}
	answers := make([]string, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	custom := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.EqualFold(part, "o") {
			if custom {
				return nil, false, false
			}
			custom = true
			continue
		}
		selected, err := strconv.Atoi(part)
		if err != nil || selected < 1 || selected > len(question.Options) {
			return nil, false, false
		}
		if _, duplicate := seen[selected]; duplicate {
			return nil, false, false
		}
		seen[selected] = struct{}{}
		answers = append(answers, question.Options[selected-1].Label)
	}
	return answers, custom, true
}

func renderTodoListText(writer io.Writer, list protocol.TodoList) {
	if len(list.Items) == 0 {
		fmt.Fprintln(writer, "No tasks.")
		return
	}
	for _, item := range list.Items {
		marker := "[ ]"
		switch item.Status {
		case protocol.TodoInProgress:
			marker = "[>]"
		case protocol.TodoCompleted:
			marker = "[x]"
		case protocol.TodoCancelled:
			marker = "[-]"
		}
		fmt.Fprintf(writer, "%s %s\n", marker, item.Content)
	}
}

func (r *runtime) printProviders(manager *provider.Manager) {
	active, _ := manager.Active()
	for _, item := range manager.List() {
		marker := " "
		if item.Name == active.Name {
			marker = "*"
		}
		fmt.Fprintf(r.stdout, "%s %s\t%s\t%s\n", marker, item.Name, item.Config.Adapter, item.Config.Model)
	}
}

func (r *runtime) handleProviderSlash(ctx context.Context, reader *bufio.Reader, fields []string, manager *provider.Manager, opts *chatOptions) error {
	if len(fields) < 2 {
		return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /provider add|edit|delete|use [name]"}
	}
	switch fields[1] {
	case "add":
		return r.onboard(ctx, manager)
	case "use":
		if len(fields) != 3 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /provider use <name>"}
		}
		if err := manager.Use(fields[2]); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		opts.provider = ""
		fmt.Fprintf(r.stdout, "Active provider: %s\n", fields[2])
		return nil
	case "delete":
		if len(fields) != 3 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /provider delete <name>"}
		}
		if err := manager.Delete(fields[2], ""); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		fmt.Fprintf(r.stdout, "Provider %s deleted.\n", fields[2])
		return nil
	case "edit":
		if len(fields) != 3 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /provider edit <name>"}
		}
		current, ok := manager.Get(fields[2])
		if !ok {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", fields[2])}
		}
		baseURL, err := r.promptLine(ctx, reader, r.stdout, "API base URL", current.BaseURL)
		if err != nil {
			return err
		}
		model, err := r.promptLine(ctx, reader, r.stdout, "Model ID", current.Model)
		if err != nil {
			return err
		}
		adapter, err := r.promptLine(ctx, reader, r.stdout, "Adapter", current.Adapter)
		if err != nil {
			return err
		}
		patch := config.ProviderPatch{BaseURL: config.SetValue(baseURL), Model: config.SetValue(model), Adapter: config.SetValue(adapter)}
		resetFrom := ""
		if currentEffort := config.EffectiveReasoningEffort(current.ReasoningEffort); config.ValidateReasoningEffort(model, currentEffort) != nil {
			patch.ReasoningEffort = config.SetValue(config.ReasoningEffortAuto)
			resetFrom = currentEffort
		}
		if err := manager.UpsertPatch(fields[2], patch, true); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		if resetFrom != "" {
			fmt.Fprintf(r.stdout, "Reasoning effort reset from %s to auto for %s.\n", resetFrom, model)
		}
		if model != current.Model {
			probed, probeErr := r.probeProviderModelLimits(ctx, manager, fields[2])
			if probeErr != nil {
				return probeErr
			}
			if _, confirmErr := r.confirmModelContextWindow(ctx, reader, manager, probed); confirmErr != nil {
				return confirmErr
			}
		}
		fmt.Fprintf(r.stdout, "Provider %s updated.\n", fields[2])
		return nil
	default:
		return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("unknown provider command %s", fields[1])}
	}
}

func (r *runtime) confirmModelContextWindow(ctx context.Context, reader *bufio.Reader, manager *provider.Manager, snapshot provider.Snapshot) (provider.Snapshot, error) {
	detected := snapshot.Limits.ContextWindow
	for {
		fmt.Fprintf(r.stdout, "Detected context window for %s: %d tokens (source: %s). Is this correct? [Y/n]: ", snapshot.Config.Model, detected, snapshot.Limits.Source)
		answer, err := r.readInteractiveLineWithInterrupts(ctx, reader, r.synchronousInputInterrupts())
		answer = strings.ToLower(strings.TrimSpace(answer))
		if err != nil && !errors.Is(err, io.EOF) {
			return provider.Snapshot{}, err
		}
		value := detected
		switch answer {
		case "", "y", "yes":
			if value <= 0 {
				fmt.Fprintln(r.stdout, "Enter the context window because detection did not return a positive value.")
				value = 0
			}
		case "n", "no":
			value = 0
		default:
			fmt.Fprintln(r.stdout, "Please answer y or n.")
			continue
		}
		if value <= 0 {
			input, inputErr := r.promptLine(ctx, reader, r.stdout, "Context window tokens", "")
			if inputErr != nil {
				return provider.Snapshot{}, inputErr
			}
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(input))
			if parseErr != nil || parsed <= 0 {
				fmt.Fprintln(r.stdout, "Context window must be a positive integer.")
				continue
			}
			value = parsed
		}
		active, _ := manager.Active()
		if err := manager.UpsertPatch(snapshot.Name, config.ProviderPatch{ContextWindow: config.SetValue(value)}, active.Name == snapshot.Name); err != nil {
			return provider.Snapshot{}, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
		}
		return r.probeProviderModelLimits(ctx, manager, snapshot.Name)
	}
}

func (r *runtime) handleModelSlash(ctx context.Context, fields []string, manager *provider.Manager) error {
	snapshot, err := manager.Active()
	if err != nil {
		return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	if len(fields) == 2 {
		patch := config.ProviderPatch{Model: config.SetValue(fields[1])}
		resetFrom := ""
		if currentEffort := config.EffectiveReasoningEffort(snapshot.Config.ReasoningEffort); config.ValidateReasoningEffort(fields[1], currentEffort) != nil {
			patch.ReasoningEffort = config.SetValue(config.ReasoningEffortAuto)
			resetFrom = currentEffort
		}
		if err := manager.UpsertPatch(snapshot.Name, patch, true); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		if resetFrom != "" {
			fmt.Fprintf(r.stdout, "Reasoning effort reset from %s to auto for %s.\n", resetFrom, fields[1])
		}
		fmt.Fprintf(r.stdout, "Model: %s\n", fields[1])
		return nil
	}
	key := providerAPIKey(snapshot.Config)
	listCtx, cancel := context.WithTimeout(ctx, snapshot.Config.Timeout(30*time.Second))
	defer cancel()
	models, err := provider.NewModelLister(&http.Client{Timeout: snapshot.Config.Timeout(30 * time.Second)}).List(listCtx, snapshot.Config.BaseURL, key, snapshot.Config.Headers)
	if err != nil {
		return err
	}
	for _, model := range models {
		fmt.Fprintln(r.stdout, model)
	}
	fmt.Fprintln(r.stdout, "Use /model <id> to select a model ID.")
	return nil
}
