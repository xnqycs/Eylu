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
	"strings"
	"time"

	"Eylu/internal/agent"
	contextledger "Eylu/internal/context"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
)

var errQuit = errors.New("quit")

func (r *runtime) runInteractive(ctx context.Context, opts chatOptions) error {
	manager, err := r.prepareManager(ctx, opts)
	if err != nil {
		return err
	}
	conversation, err := r.openConversation(ctx, manager, &opts)
	if err != nil {
		return err
	}
	if !opts.noTUI && isTerminal(r.stdout) && !strings.EqualFold(os.Getenv("TERM"), "dumb") && r.output == "text" {
		return r.runTUI(ctx, conversation, manager, opts)
	}
	reader := bufio.NewReader(r.stdin)
	r.inputReader = reader
	defer func() { r.inputReader = nil }()
	fmt.Fprintf(r.stdout, "Eylu session %s\nType /help for commands.\n", conversation.SessionID())
	for {
		fmt.Fprint(r.stdout, "> ")
		line, readErr := reader.ReadString('\n')
		line = strings.TrimSpace(line)
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
			err = r.handleSlashCommand(ctx, reader, line, conversation, manager, &opts)
			if errors.Is(err, errQuit) {
				return nil
			}
			if err != nil {
				r.printError(err)
			}
		} else {
			err = r.sendInteractive(ctx, conversation, manager, line, opts)
			if errors.Is(err, errQuit) {
				return nil
			}
			if err != nil {
				r.printError(err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func (r *runtime) sendInteractive(ctx context.Context, conversation *agent.Conversation, manager *provider.Manager, prompt string, opts chatOptions) error {
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	result := make(chan error, 1)
	go func() {
		result <- r.sendPrompt(requestCtx, conversation, manager, prompt, opts)
	}()
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
		fmt.Fprintln(r.stdout, "/new  /context  /skills  /skill <name>  /providers  /provider add|edit|delete|use  /model [id]  /mode manual|plan|auto|full  /quit")
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
	case "/providers":
		r.printProviders(manager)
		return nil
	case "/skills":
		return r.handleSkillsSlash(conversation, manager.Config(), *opts)
	case "/skill":
		if len(fields) != 2 {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "usage: /skill <name>"}
		}
		if err := r.activateSkillSlash(conversation, manager.Config(), *opts, fields[1]); err != nil {
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
		if r.session != nil {
			return r.session.Sync(conversation, manager, *opts, nil)
		}
		return nil
	case "/model":
		if err := r.handleModelSlash(ctx, fields, manager); err != nil {
			return err
		}
		if r.session != nil {
			return r.session.Sync(conversation, manager, *opts, nil)
		}
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
		current.BaseURL = promptLine(reader, r.stdout, "API base URL", current.BaseURL)
		current.Model = promptLine(reader, r.stdout, "Model ID", current.Model)
		current.Adapter = promptLine(reader, r.stdout, "Adapter", current.Adapter)
		if err := manager.Upsert(fields[2], current, true); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		fmt.Fprintf(r.stdout, "Provider %s updated.\n", fields[2])
		return nil
	default:
		return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("unknown provider command %s", fields[1])}
	}
}

func (r *runtime) handleModelSlash(ctx context.Context, fields []string, manager *provider.Manager) error {
	snapshot, err := manager.Active()
	if err != nil {
		return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	if len(fields) == 2 {
		candidate := snapshot.Config
		candidate.Model = fields[1]
		if err := manager.Upsert(snapshot.Name, candidate, true); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
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
