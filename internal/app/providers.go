package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"Eylu/internal/config"
	"Eylu/internal/driver/openai_responses"
	"Eylu/internal/logging"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
)

type providerOptions struct {
	adapter         string
	baseURL         string
	model           string
	apiKey          string
	contextWindow   int
	timeout         time.Duration
	activate        bool
	routingTasks    []string
	routingPriority int
	inputCost       float64
	outputCost      float64
}

func (r *runtime) providersCommand(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{Use: "providers", Short: "manage AI providers"}
	cmd.AddCommand(
		r.providersListCommand(),
		r.providerUpsertCommand("add", false),
		r.providerUpsertCommand("edit", true),
		r.providerDeleteCommand(),
		r.providerUseCommand(),
		r.providerModelsCommand(ctx),
	)
	return cmd
}

func (r *runtime) providersListCommand() *cobra.Command {
	return &cobra.Command{
		Use: "list", Args: cobra.NoArgs, Short: "list configured providers",
		RunE: func(*cobra.Command, []string) error {
			loaded, manager, err := r.loadManager()
			if err != nil {
				return err
			}
			items := manager.List()
			if r.output != "text" {
				return json.NewEncoder(r.stdout).Encode(map[string]any{"active_provider": loaded.Config.ActiveProvider, "providers": items})
			}
			if len(items) == 0 {
				fmt.Fprintln(r.stdout, "No providers configured. Run: eylu providers add <name> --base-url <url> --model <id>")
				return nil
			}
			for _, item := range items {
				marker := " "
				if item.Name == loaded.Config.ActiveProvider {
					marker = "*"
				}
				fmt.Fprintf(r.stdout, "%s %s\t%s\t%s\t%s\ttasks=%s\tpriority=%d\n", marker, item.Name, item.Config.Adapter, item.Config.Model, item.Config.BaseURL, strings.Join(item.Config.Routing.Tasks, ","), item.Config.Routing.Priority)
			}
			return nil
		},
	}
}

func (r *runtime) providerUpsertCommand(verb string, editing bool) *cobra.Command {
	var opts providerOptions
	opts.adapter = openai_responses.Name
	cmd := &cobra.Command{
		Use: verb + " <name>", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, manager, err := r.loadManager()
			if err != nil {
				return err
			}
			name := args[0]
			candidate := config.ProviderConfig{}
			if editing {
				current, ok := manager.Get(name)
				if !ok {
					return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", name)}
				}
				candidate = current
			}
			if cmd.Flags().Changed("adapter") || !editing {
				candidate.Adapter = opts.adapter
			}
			if cmd.Flags().Changed("base-url") || !editing {
				candidate.BaseURL = opts.baseURL
			}
			if cmd.Flags().Changed("model") || !editing {
				candidate.Model = opts.model
			}
			if cmd.Flags().Changed("context-window") || !editing {
				candidate.ContextWindow = opts.contextWindow
			}
			if cmd.Flags().Changed("timeout") || !editing {
				candidate.TimeoutSeconds = int(opts.timeout / time.Second)
			}
			if cmd.Flags().Changed("routing-task") || !editing {
				candidate.Routing.Tasks = append([]string(nil), opts.routingTasks...)
			}
			if cmd.Flags().Changed("routing-priority") || !editing {
				candidate.Routing.Priority = opts.routingPriority
			}
			if cmd.Flags().Changed("input-cost") || !editing {
				candidate.Routing.InputCostPerMillion = opts.inputCost
			}
			if cmd.Flags().Changed("output-cost") || !editing {
				candidate.Routing.OutputCostPerMillion = opts.outputCost
			}
			if cmd.Flags().Changed("api-key") || !editing {
				candidate.APIKey = opts.apiKey
			}
			if err := config.ValidateProvider(name, candidate); err != nil {
				return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
			}
			if err := manager.Upsert(name, candidate, opts.activate); err != nil {
				return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
			}
			r.rememberProviderAPIKeys(manager.Config())
			fmt.Fprintf(r.stdout, "Provider %s saved.\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.adapter, "adapter", opts.adapter, "driver adapter")
	cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "API base URL")
	cmd.Flags().StringVar(&opts.model, "model", "", "model ID")
	cmd.Flags().StringVar(&opts.apiKey, "api-key", "", "API key to store in the provider configuration")
	cmd.Flags().IntVar(&opts.contextWindow, "context-window", 0, "model context window")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 60*time.Second, "request timeout")
	cmd.Flags().BoolVar(&opts.activate, "activate", true, "make provider active")
	cmd.Flags().StringSliceVar(&opts.routingTasks, "routing-task", nil, "routing task: general, coding, review, debugging, testing, or documentation")
	cmd.Flags().IntVar(&opts.routingPriority, "routing-priority", 0, "automatic routing priority")
	cmd.Flags().Float64Var(&opts.inputCost, "input-cost", 0, "input cost per million tokens")
	cmd.Flags().Float64Var(&opts.outputCost, "output-cost", 0, "output cost per million tokens")
	return cmd
}

func (r *runtime) providerDeleteCommand() *cobra.Command {
	var replacement string
	cmd := &cobra.Command{
		Use: "delete <name>", Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		_, manager, err := r.loadManager()
		if err != nil {
			return err
		}
		if _, ok := manager.Get(args[0]); !ok {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", args[0])}
		}
		if err := manager.Delete(args[0], replacement); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
		r.rememberProviderAPIKeys(manager.Config())
		fmt.Fprintf(r.stdout, "Provider %s deleted.\n", args[0])
		return nil
	}
	cmd.Flags().StringVar(&replacement, "replacement", "", "replacement active provider")
	return cmd
}

func (r *runtime) providerUseCommand() *cobra.Command {
	return &cobra.Command{
		Use: "use <name>", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			_, manager, err := r.loadManager()
			if err != nil {
				return err
			}
			if err := manager.Use(args[0]); err != nil {
				return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
			}
			fmt.Fprintf(r.stdout, "Active provider: %s\n", args[0])
			return nil
		},
	}
}

func (r *runtime) providerModelsCommand(ctx context.Context) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use: "models", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, manager, err := r.loadManager()
			if err != nil {
				return err
			}
			var cfg config.ProviderConfig
			if name != "" {
				var ok bool
				cfg, ok = manager.Get(name)
				if !ok {
					return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", name)}
				}
			} else {
				snapshot, activeErr := manager.Active()
				if activeErr != nil {
					return &protocol.Error{Code: protocol.ErrConfig, Message: activeErr.Error()}
				}
				cfg = snapshot.Config
			}
			key := providerAPIKey(cfg)
			listCtx, cancel := context.WithTimeout(ctx, cfg.Timeout(30*time.Second))
			defer cancel()
			models, err := provider.NewModelLister(&http.Client{Timeout: cfg.Timeout(30 * time.Second)}).List(listCtx, cfg.BaseURL, key, cfg.Headers)
			if err != nil {
				return err
			}
			if r.output != "text" {
				return json.NewEncoder(r.stdout).Encode(map[string]any{"models": models})
			}
			for _, model := range models {
				fmt.Fprintln(r.stdout, model)
			}
			if len(models) == 0 {
				fmt.Fprintln(r.stderr, "Provider returned no models; enter a model ID manually with providers edit --model.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "provider", "", "provider name")
	return cmd
}

func (r *runtime) onboard(ctx context.Context, manager *provider.Manager) error {
	reader := bufio.NewReader(r.stdin)
	fmt.Fprintln(r.stdout, "Eylu provider setup")
	name := promptLine(reader, r.stdout, "Provider name", "default")
	baseURL := promptLine(reader, r.stdout, "API base URL", "https://api.openai.com/v1")
	fmt.Fprintf(r.stdout, "API key for %s: ", hostOnly(baseURL))
	secret, err := readSecret(r.stdin, reader)
	fmt.Fprintln(r.stdout)
	if err != nil {
		return &protocol.Error{Code: protocol.ErrCredential, Message: "read API key", Cause: err}
	}
	model := ""
	models, listErr := provider.NewModelLister(&http.Client{Timeout: 20 * time.Second}).List(ctx, baseURL, secret, nil)
	if listErr == nil && len(models) > 0 {
		fmt.Fprintln(r.stdout, "Available models:")
		for index, item := range models {
			fmt.Fprintf(r.stdout, "  %d. %s\n", index+1, item)
		}
		choice := promptLine(reader, r.stdout, "Model number or model ID", models[0])
		if number, parseErr := strconv.Atoi(choice); parseErr == nil && number > 0 && number <= len(models) {
			model = models[number-1]
		} else {
			model = choice
		}
	} else {
		if listErr != nil {
			fmt.Fprintf(r.stderr, "Model discovery failed: %s\n", logging.Redact(listErr.Error(), secret))
		}
		model = promptLine(reader, r.stdout, "Model ID", "")
	}
	if model == "" {
		return &protocol.Error{Code: protocol.ErrConfig, Message: "model ID is required"}
	}
	candidate := config.ProviderConfig{Adapter: openai_responses.Name, BaseURL: baseURL, APIKey: secret, Model: model, TimeoutSeconds: 60}
	if err := manager.Upsert(name, candidate, true); err != nil {
		return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	r.rememberProviderAPIKeys(manager.Config())
	return nil
}

func promptLine(reader *bufio.Reader, out ioWriter, label, fallback string) string {
	if fallback != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, fallback)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	value, _ := reader.ReadString('\n')
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

type ioWriter interface {
	Write([]byte) (int, error)
}

func readSecret(input any, reader *bufio.Reader) (string, error) {
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		raw, err := term.ReadPassword(int(file.Fd()))
		return strings.TrimSpace(string(raw)), err
	}
	value, err := reader.ReadString('\n')
	return strings.TrimSpace(value), err
}

func isTerminal(input any) bool {
	file, ok := input.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func hostOnly(value string) string {
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	if index := strings.IndexByte(value, '/'); index >= 0 {
		value = value[:index]
	}
	return value
}
