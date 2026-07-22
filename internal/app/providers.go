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
	catalogProvider string
	contextWindow   int
	timeout         time.Duration
	activate        bool
	routingTasks    []string
	routingPriority int
	inputCost       float64
	outputCost      float64
	webPermission   string
	webSearch       string
	webFetch        string
	searchFallback  string
	fetchFallback   string
	searchClient    string
	fetchClient     string
	searchDelegate  string
	fetchDelegate   string
	allowedDomains  []string
	blockedDomains  []string
	webMaxUses      int
	webContextSize  string
	trustedFetch    bool
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
			patch := config.ProviderPatch{}
			if cmd.Flags().Changed("adapter") || !editing {
				patch.Adapter = config.SetValue(opts.adapter)
			}
			if cmd.Flags().Changed("base-url") || !editing {
				patch.BaseURL = config.SetValue(opts.baseURL)
			}
			if cmd.Flags().Changed("model") || !editing {
				patch.Model = config.SetValue(opts.model)
			}
			if cmd.Flags().Changed("api-key") {
				patch.APIKey = config.SetValue(opts.apiKey)
			}
			if cmd.Flags().Changed("catalog-provider") {
				if opts.catalogProvider == "" {
					patch.CatalogProvider = config.RemoveValue[string]()
				} else {
					patch.CatalogProvider = config.SetValue(opts.catalogProvider)
				}
			}
			if cmd.Flags().Changed("context-window") {
				patch.ContextWindow = config.SetValue(opts.contextWindow)
			}
			if cmd.Flags().Changed("timeout") {
				patch.TimeoutSeconds = config.SetValue(int(opts.timeout / time.Second))
			}
			if cmd.Flags().Changed("routing-task") {
				patch.RoutingTasks = config.SetValue(append([]string(nil), opts.routingTasks...))
			}
			if cmd.Flags().Changed("routing-priority") {
				patch.RoutingPriority = config.SetValue(opts.routingPriority)
			}
			if cmd.Flags().Changed("input-cost") {
				patch.InputCost = config.SetValue(opts.inputCost)
			}
			if cmd.Flags().Changed("output-cost") {
				patch.OutputCost = config.SetValue(opts.outputCost)
			}
			if providerWebFlagsChanged(cmd) {
				web := candidate.WebTools
				if cmd.Flags().Changed("web-permission") {
					web.Permission = opts.webPermission
				}
				applyWebToolFlags(cmd, "search", opts.webSearch, opts.searchFallback, opts.searchClient, opts.searchDelegate, &web.Search)
				applyWebToolFlags(cmd, "fetch", opts.webFetch, opts.fetchFallback, opts.fetchClient, opts.fetchDelegate, &web.Fetch)
				for _, item := range []*config.WebToolConfig{&web.Search, &web.Fetch} {
					if cmd.Flags().Changed("web-allowed-domain") {
						item.AllowedDomains = append([]string(nil), opts.allowedDomains...)
					}
					if cmd.Flags().Changed("web-blocked-domain") {
						item.BlockedDomains = append([]string(nil), opts.blockedDomains...)
					}
					if cmd.Flags().Changed("web-max-uses") {
						item.MaxUses = opts.webMaxUses
					}
					if cmd.Flags().Changed("web-context-size") {
						item.ContextSize = opts.webContextSize
					}
				}
				if cmd.Flags().Changed("web-fetch-trusted-network-boundary") {
					web.Fetch.TrustedNetworkBoundary = opts.trustedFetch
				}
				patch.WebTools = config.SetValue(web)
			}
			candidate = config.ApplyProviderPatch(candidate, patch)
			if err := config.ValidateProvider(name, candidate); err != nil {
				return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
			}
			if err := manager.UpsertPatch(name, patch, opts.activate); err != nil {
				return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
			}
			r.rememberProviderAPIKeys(manager.Config())
			probed, err := r.probeProviderModelLimits(cmd.Context(), manager, name)
			if err != nil {
				return err
			}
			modelSelected := !editing || cmd.Flags().Changed("model")
			if modelSelected && !cmd.Flags().Changed("context-window") && isTerminal(r.stdin) {
				reader := bufio.NewReader(r.stdin)
				if _, err := r.confirmModelContextWindow(cmd.Context(), reader, manager, probed); err != nil {
					return err
				}
			}
			fmt.Fprintf(r.stdout, "Provider %s saved.\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.adapter, "adapter", opts.adapter, "driver adapter")
	cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "API base URL")
	cmd.Flags().StringVar(&opts.model, "model", "", "model ID")
	cmd.Flags().StringVar(&opts.apiKey, "api-key", "", "API key to store in the provider configuration")
	cmd.Flags().StringVar(&opts.catalogProvider, "catalog-provider", "", "provider ID used for model metadata and capability resolution")
	cmd.Flags().IntVar(&opts.contextWindow, "context-window", 0, "model context window")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 60*time.Second, "request timeout")
	cmd.Flags().BoolVar(&opts.activate, "activate", true, "make provider active")
	cmd.Flags().StringSliceVar(&opts.routingTasks, "routing-task", nil, "routing task: general, coding, review, debugging, testing, or documentation")
	cmd.Flags().IntVar(&opts.routingPriority, "routing-priority", 0, "automatic routing priority")
	cmd.Flags().Float64Var(&opts.inputCost, "input-cost", 0, "input cost per million tokens")
	cmd.Flags().Float64Var(&opts.outputCost, "output-cost", 0, "output cost per million tokens")
	cmd.Flags().StringVar(&opts.webPermission, "web-permission", "allow", "web permission: ask, allow, or deny")
	cmd.Flags().StringVar(&opts.webSearch, "web-search", "auto", "web search mode: off, auto, hosted, delegated, or client")
	cmd.Flags().StringVar(&opts.webFetch, "web-fetch", "auto", "web fetch mode: off, auto, hosted, delegated, or client")
	cmd.Flags().StringVar(&opts.searchFallback, "web-search-fallback", "", "web search fallback: delegated or client")
	cmd.Flags().StringVar(&opts.fetchFallback, "web-fetch-fallback", "", "web fetch fallback: delegated or client")
	cmd.Flags().StringVar(&opts.searchClient, "web-search-client-tool", "", "MCP tool used for client web search")
	cmd.Flags().StringVar(&opts.fetchClient, "web-fetch-client-tool", "", "MCP tool used for client web fetch")
	cmd.Flags().StringVar(&opts.searchDelegate, "web-search-delegated-provider", "", "provider used for delegated web search")
	cmd.Flags().StringVar(&opts.fetchDelegate, "web-fetch-delegated-provider", "", "provider used for delegated web fetch")
	cmd.Flags().StringSliceVar(&opts.allowedDomains, "web-allowed-domain", nil, "allowed web domain (repeatable)")
	cmd.Flags().StringSliceVar(&opts.blockedDomains, "web-blocked-domain", nil, "blocked web domain (repeatable)")
	cmd.Flags().IntVar(&opts.webMaxUses, "web-max-uses", 5, "maximum uses per web tool and user submission")
	cmd.Flags().StringVar(&opts.webContextSize, "web-context-size", "medium", "web context size: low, medium, or high")
	cmd.Flags().BoolVar(&opts.trustedFetch, "web-fetch-trusted-network-boundary", false, "trust the configured MCP fetch tool network boundary")
	return cmd
}

func providerWebFlagsChanged(cmd *cobra.Command) bool {
	for _, name := range []string{
		"web-permission", "web-search", "web-fetch", "web-search-fallback", "web-fetch-fallback",
		"web-search-client-tool", "web-fetch-client-tool", "web-search-delegated-provider", "web-fetch-delegated-provider",
		"web-allowed-domain", "web-blocked-domain", "web-max-uses", "web-context-size", "web-fetch-trusted-network-boundary",
	} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func applyWebToolFlags(cmd *cobra.Command, name, mode, fallback, client, delegated string, target *config.WebToolConfig) {
	if cmd.Flags().Changed("web-" + name) {
		enabled := mode != "off"
		target.Enabled = &enabled
		if enabled {
			target.Execution = mode
		} else {
			target.Execution = ""
		}
	}
	if cmd.Flags().Changed("web-" + name + "-fallback") {
		target.Fallback = fallback
	}
	if cmd.Flags().Changed("web-" + name + "-client-tool") {
		target.ClientTool = client
	}
	if cmd.Flags().Changed("web-" + name + "-delegated-provider") {
		target.DelegatedProvider = delegated
	}
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
		if active, activeErr := manager.Active(); activeErr == nil {
			if _, probeErr := r.probeProviderModelLimits(cmd.Context(), manager, active.Name); probeErr != nil {
				return probeErr
			}
		}
		fmt.Fprintf(r.stdout, "Provider %s deleted.\n", args[0])
		return nil
	}
	cmd.Flags().StringVar(&replacement, "replacement", "", "replacement active provider")
	return cmd
}

func (r *runtime) providerUseCommand() *cobra.Command {
	return &cobra.Command{
		Use: "use <name>", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, manager, err := r.loadManager()
			if err != nil {
				return err
			}
			if err := manager.Use(args[0]); err != nil {
				return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
			}
			if _, err := r.probeProviderModelLimits(cmd.Context(), manager, args[0]); err != nil {
				return err
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
	reader := r.currentInputReader()
	if reader == nil {
		reader = bufio.NewReader(r.stdin)
	}
	fmt.Fprintln(r.stdout, "Eylu provider setup")
	name, err := r.promptLine(ctx, reader, r.stdout, "Provider name", "default")
	if err != nil {
		return err
	}
	baseURL, err := r.promptLine(ctx, reader, r.stdout, "API base URL", "https://api.openai.com/v1")
	if err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "API key for %s: ", hostOnly(baseURL))
	secret, err := r.readSecret(ctx, r.stdin, reader)
	fmt.Fprintln(r.stdout)
	if err != nil {
		if errors.Is(err, errQuit) || errors.Is(err, context.Canceled) {
			return err
		}
		return &protocol.Error{Code: protocol.ErrCredential, Message: "read API key", Cause: err}
	}
	model := ""
	models, listErr := provider.NewModelLister(&http.Client{Timeout: 20 * time.Second}).List(ctx, baseURL, secret, nil)
	if listErr == nil && len(models) > 0 {
		fmt.Fprintln(r.stdout, "Available models:")
		for index, item := range models {
			fmt.Fprintf(r.stdout, "  %d. %s\n", index+1, item)
		}
		choice, promptErr := r.promptLine(ctx, reader, r.stdout, "Model number or model ID", models[0])
		if promptErr != nil {
			return promptErr
		}
		if number, parseErr := strconv.Atoi(choice); parseErr == nil && number > 0 && number <= len(models) {
			model = models[number-1]
		} else {
			model = choice
		}
	} else {
		if listErr != nil {
			fmt.Fprintf(r.stderr, "Model discovery failed: %s\n", logging.Redact(listErr.Error(), secret))
		}
		model, err = r.promptLine(ctx, reader, r.stdout, "Model ID", "")
		if err != nil {
			return err
		}
	}
	if model == "" {
		return &protocol.Error{Code: protocol.ErrConfig, Message: "model ID is required"}
	}
	patch := config.ProviderPatch{Adapter: config.SetValue(openai_responses.Name), BaseURL: config.SetValue(baseURL), APIKey: config.SetValue(secret), Model: config.SetValue(model)}
	if err := manager.UpsertPatch(name, patch, true); err != nil {
		return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error(), Cause: err}
	}
	r.rememberProviderAPIKeys(manager.Config())
	probed, err := r.probeProviderModelLimits(ctx, manager, name)
	if err != nil {
		return err
	}
	if _, err := r.confirmModelContextWindow(ctx, reader, manager, probed); err != nil {
		return err
	}
	return nil
}

func (r *runtime) promptLine(ctx context.Context, reader *bufio.Reader, out ioWriter, label, fallback string) (string, error) {
	if fallback != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, fallback)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	value, err := r.readInteractiveLineWithInterrupts(ctx, reader, r.synchronousInputInterrupts())
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	return value, nil
}

type ioWriter interface {
	Write([]byte) (int, error)
}

func (r *runtime) readSecret(ctx context.Context, input any, reader *bufio.Reader) (string, error) {
	interrupts := r.synchronousInputInterrupts()
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		return readTerminalSecret(ctx, file, interrupts)
	}
	value, err := r.readInteractiveLineWithInterrupts(ctx, reader, interrupts)
	return strings.TrimSpace(value), err
}

func readTerminalSecret(ctx context.Context, file *os.File, interrupts <-chan os.Signal) (string, error) {
	state, err := term.MakeRaw(int(file.Fd()))
	if err != nil {
		return "", err
	}
	defer func() { _ = term.Restore(int(file.Fd()), state) }()

	secret := make([]byte, 0, 64)
	for {
		result := make(chan inputLineResult, 1)
		go func() {
			buffer := make([]byte, 1)
			count, readErr := file.Read(buffer)
			result <- inputLineResult{line: string(buffer[:count]), err: readErr}
		}()
		select {
		case read := <-result:
			if len(read.line) > 0 {
				switch read.line[0] {
				case '\r', '\n':
					return strings.TrimSpace(string(secret)), nil
				case '\b', 0x7f:
					if len(secret) > 0 {
						secret = secret[:len(secret)-1]
					}
				case 0x03:
					return "", errQuit
				case 0x04:
					if len(secret) == 0 {
						return "", io.EOF
					}
					return strings.TrimSpace(string(secret)), nil
				default:
					secret = append(secret, read.line[0])
				}
			}
			if read.err != nil {
				if errors.Is(read.err, io.EOF) && len(secret) > 0 {
					return strings.TrimSpace(string(secret)), nil
				}
				return "", read.err
			}
		case <-ctx.Done():
			return "", ctx.Err()
		case <-interrupts:
			return "", errQuit
		}
	}
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
