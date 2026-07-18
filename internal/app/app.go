package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/driver/openai_responses"
	"Eylu/internal/logging"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
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
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	configPath  string
	workspace   string
	output      string
	credentials *provider.CredentialStore
}

func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	r := &runtime{stdin: stdin, stdout: stdout, stderr: stderr, credentials: provider.NewCredentialStore()}
	root := r.rootCommand(ctx)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		r.printError(err)
		return exitCode(err)
	}
	return exitOK
}

func (r *runtime) rootCommand(ctx context.Context) *cobra.Command {
	root := &cobra.Command{
		Use:           "eylu",
		Short:         "Eylu terminal programming agent",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&r.configPath, "config", "", "config file path")
	root.PersistentFlags().StringVar(&r.workspace, "workspace", "", "workspace directory")
	root.PersistentFlags().StringVar(&r.output, "output", "text", "output format: text or json")
	root.AddCommand(r.chatCommand(ctx), r.providersCommand(ctx))
	return root
}

type chatOptions struct {
	provider string
	model    string
	baseURL  string
	adapter  string
	timeout  time.Duration
}

func (r *runtime) chatCommand(ctx context.Context) *cobra.Command {
	var opts chatOptions
	cmd := &cobra.Command{
		Use:   "chat [prompt]",
		Short: "send a prompt to the active model",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := ""
			if len(args) == 1 {
				prompt = args[0]
			} else if !isTerminal(r.stdin) {
				raw, err := io.ReadAll(io.LimitReader(r.stdin, 1<<20))
				if err != nil {
					return err
				}
				prompt = strings.TrimSpace(string(raw))
			}
			if prompt == "" {
				return &protocol.Error{Code: protocol.ErrConfig, Message: "prompt is required; use eylu chat \"your request\""}
			}
			return r.runChat(ctx, prompt, opts)
		},
	}
	cmd.Flags().StringVar(&opts.provider, "provider", "", "provider name")
	cmd.Flags().StringVar(&opts.model, "model", "", "model ID override")
	cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "API base URL override")
	cmd.Flags().StringVar(&opts.adapter, "adapter", openai_responses.Name, "model driver adapter")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 0, "request timeout")
	return cmd
}

func (r *runtime) runChat(ctx context.Context, prompt string, opts chatOptions) error {
	loaded, manager, err := r.loadManager()
	if err != nil {
		return err
	}
	if len(loaded.Config.Providers) == 0 {
		if opts.baseURL != "" && opts.model != "" {
			loaded.Config.Providers["runtime"] = config.ProviderConfig{
				Adapter: opts.adapter, BaseURL: opts.baseURL, Model: opts.model,
				Credential: config.CredentialRef{Type: "env", Env: "EYLU_API_KEY"},
			}
			loaded.Config.ActiveProvider = "runtime"
			manager, err = provider.NewManager(loaded.Path, loaded.Config, nil)
			if err != nil {
				return err
			}
		} else if isTerminal(r.stdin) {
			if err := r.onboard(ctx, manager); err != nil {
				return err
			}
		} else {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "no provider configured; run eylu providers add or pass --base-url and --model"}
		}
	}
	var snapshot provider.Snapshot
	if opts.provider != "" {
		providerConfig, ok := manager.Get(opts.provider)
		if !ok {
			return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", opts.provider)}
		}
		snapshot = provider.Snapshot{Name: opts.provider, Config: providerConfig}
	} else {
		snapshot, err = manager.Active()
		if err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
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
		return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	apiKey := os.Getenv("EYLU_API_KEY")
	if apiKey == "" {
		apiKey, err = r.credentials.Resolve(providerConfig.Credential)
		if err != nil {
			return &protocol.Error{Code: protocol.ErrCredential, Message: "provider credential is unavailable", Cause: err}
		}
	}
	requestTimeout := providerConfig.Timeout(60 * time.Second)
	if opts.timeout > 0 {
		requestTimeout = opts.timeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	registry := driver.NewRegistry(openai_responses.New(&http.Client{Timeout: requestTimeout}))
	modelDriver, err := registry.Get(providerConfig.Adapter)
	if err != nil {
		return &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
	}
	request := protocol.ModelRequest{
		ProtocolVersion: protocol.Version,
		Model:           providerConfig.Model,
		Turns: []protocol.Turn{{
			ID: uuid.NewString(), Role: protocol.RoleUser, CreatedAt: time.Now().UTC(),
			Parts: []protocol.Part{{Kind: protocol.PartText, Text: prompt}},
		}},
	}
	response, err := modelDriver.Generate(requestCtx, driver.Request{
		BaseURL: providerConfig.BaseURL,
		APIKey:  apiKey,
		Headers: providerConfig.Headers,
		Model:   request,
	}, nil)
	if err != nil {
		return err
	}
	if r.output == "json" {
		return json.NewEncoder(r.stdout).Encode(response)
	}
	for _, part := range response.Turn.Parts {
		if part.Kind == protocol.PartText {
			fmt.Fprint(r.stdout, part.Text)
		}
	}
	fmt.Fprintln(r.stdout)
	return nil
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
	if r.output == "json" {
		_ = json.NewEncoder(r.stderr).Encode(map[string]any{"error": map[string]any{"code": typed.Code, "message": message, "retryable": typed.Retryable}})
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

func absoluteWorkspace(path string) string {
	if path == "" {
		path, _ = os.Getwd()
	}
	path, _ = filepath.Abs(path)
	return path
}
