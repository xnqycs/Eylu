package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/mcpclient"
	"Eylu/internal/protocol"
)

func (r *runtime) configureMCPRuntime(ctx context.Context, cfg config.Config, modelRuntime *agent.Runtime) error {
	manager, err := r.loadMCP(ctx, cfg)
	if err != nil {
		return err
	}
	modelRuntime.MCPFingerprint = manager.Fingerprint()
	modelRuntime.MCPToolServers = make(map[string]string)
	for _, serverContext := range manager.Contexts() {
		modelRuntime.MCPContexts = append(modelRuntime.MCPContexts, agent.MCPContext{
			Server: serverContext.Server, Instructions: serverContext.Instructions, ResourceCatalog: serverContext.ResourceCatalog,
		})
		for _, definition := range serverContext.ToolDefinitions {
			modelRuntime.MCPToolServers[definition.Name] = serverContext.Server
		}
	}
	return nil
}

func (r *runtime) loadMCP(ctx context.Context, cfg config.Config) (*mcpclient.Manager, error) {
	encoded, err := json.Marshal(struct {
		Workspace string                            `json:"workspace"`
		Servers   map[string]config.MCPServerConfig `json:"servers"`
	}{Workspace: r.workspace, Servers: cfg.MCPServers})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	key := hex.EncodeToString(digest[:])
	if r.mcp != nil && r.mcpKey == key {
		return r.mcp, nil
	}
	if err := r.closeMCP(); err != nil {
		return nil, err
	}
	manager, diagnostics, err := mcpclient.Open(ctx, cfg.MCPServers, r.workspace)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize MCP runtime: " + err.Error(), Cause: err}
	}
	r.mcp, r.mcpKey = manager, key
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(r.stderr, "[mcp] server=%s error=%s\n", r.redact(diagnostic.Server), r.redact(diagnostic.Message))
	}
	return manager, nil
}

func (r *runtime) closeMCP() error {
	if r.mcp == nil {
		return nil
	}
	manager := r.mcp
	r.mcp, r.mcpKey = nil, ""
	return manager.Close()
}

type mcpCommandBackend interface {
	Servers() ([]mcpclient.ServerInfo, string)
	Inspect(string) (mcpclient.ServerDetail, error)
	Reconnect(context.Context, string) error
	Tools(string) ([]mcpclient.ToolInfo, error)
	Tool(string, string) (mcpclient.ToolInfo, error)
	Login(context.Context, string) error
	Logout(context.Context, string) error
	Resources(string) ([]mcpclient.ResourceInfo, error)
	Resource(context.Context, string, string) (any, error)
	Prompts(string) ([]mcpclient.PromptInfo, error)
	Prompt(context.Context, string, string, map[string]string) (any, error)
}

type mcpBackendLoader func(context.Context) (mcpCommandBackend, error)
type mcpToggleServerFunc func(context.Context, string, bool) error

type managerMCPCommandBackend struct {
	manager *mcpclient.Manager
}

func (b *managerMCPCommandBackend) Servers() ([]mcpclient.ServerInfo, string) {
	return b.manager.List(), b.manager.Fingerprint()
}

func (b *managerMCPCommandBackend) Inspect(name string) (mcpclient.ServerDetail, error) {
	return b.manager.Inspect(name)
}

func (b *managerMCPCommandBackend) Reconnect(ctx context.Context, name string) error {
	return b.manager.Reconnect(ctx, name)
}

func (b *managerMCPCommandBackend) Tools(name string) ([]mcpclient.ToolInfo, error) {
	return b.manager.ServerTools(name)
}

func (b *managerMCPCommandBackend) Tool(server, name string) (mcpclient.ToolInfo, error) {
	return b.manager.Tool(server, name)
}

func (b *managerMCPCommandBackend) Login(ctx context.Context, name string) error {
	return b.manager.Login(ctx, name)
}

func (b *managerMCPCommandBackend) Logout(ctx context.Context, name string) error {
	return b.manager.Logout(ctx, name)
}

func (b *managerMCPCommandBackend) Resources(name string) ([]mcpclient.ResourceInfo, error) {
	return b.manager.Resources(name)
}

func (b *managerMCPCommandBackend) Resource(ctx context.Context, server, uri string) (any, error) {
	return b.manager.ReadResource(ctx, server, uri)
}

func (b *managerMCPCommandBackend) Prompts(name string) ([]mcpclient.PromptInfo, error) {
	return b.manager.Prompts(name)
}

func (b *managerMCPCommandBackend) Prompt(ctx context.Context, server, name string, arguments map[string]string) (any, error) {
	return b.manager.GetPrompt(ctx, server, name, arguments)
}

func mcpServerNotFound(name string) error {
	return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("configured MCP server %q was not found", name)}
}

func mcpManagerCapabilityUnavailable(capability string) error {
	return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("the MCP runtime does not expose %s yet", capability)}
}

func (r *runtime) mcpCommand(ctx context.Context) *cobra.Command {
	loader := func(loadContext context.Context) (mcpCommandBackend, error) {
		loaded, _, err := r.loadManager()
		if err != nil {
			return nil, err
		}
		manager, err := r.loadMCP(loadContext, loaded.Config)
		if err != nil {
			return nil, err
		}
		return &managerMCPCommandBackend{manager: manager}, nil
	}
	toggle := func(toggleContext context.Context, name string, enabled bool) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		if _, ok := loaded.Config.MCPServers[name]; !ok {
			return mcpServerNotFound(name)
		}
		updated, err := loaded.Store.SetMCPServerEnabled(name, enabled)
		if err != nil {
			return err
		}
		if err := r.closeMCP(); err != nil {
			return err
		}
		if enabled {
			_, err = r.loadMCP(toggleContext, updated)
		}
		return err
	}
	return r.mcpCommandWithBackend(ctx, loader, toggle)
}

func (r *runtime) mcpCommandWithBackend(ctx context.Context, load mcpBackendLoader, toggle mcpToggleServerFunc) *cobra.Command {
	command := &cobra.Command{Use: "mcp", Short: "inspect and manage MCP servers"}
	backend := func() (mcpCommandBackend, error) {
		if load == nil {
			return nil, mcpManagerCapabilityUnavailable("command backend")
		}
		return load(ctx)
	}
	command.AddCommand(&cobra.Command{Use: "list", Short: "list configured MCP servers", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		servers, fingerprint := service.Servers()
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"servers": servers, "fingerprint": fingerprint})
		}
		if len(servers) == 0 {
			fmt.Fprintln(r.stdout, "No configured MCP servers.")
			return nil
		}
		for _, server := range servers {
			fmt.Fprintf(r.stdout, "%s\tstatus=%s\ttransport=%s\tprotocol=%s\ttools=%d\tresources=%d\tprompts=%d\n", r.redact(server.Name), server.Status, server.Transport, server.ProtocolVersion, server.Tools, server.Resources, server.Prompts)
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "inspect <name>", Short: "show MCP server details", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		detail, err := service.Inspect(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(detail)
		}
		return r.writeMCPTextValue(detail)
	}})
	command.AddCommand(mcpActionCommand("reconnect", "reconnect an MCP server", backend, func(service mcpCommandBackend, name string) error { return service.Reconnect(ctx, name) }, r))
	command.AddCommand(r.mcpToggleCommand(ctx, "enable", true, toggle), r.mcpToggleCommand(ctx, "disable", false, toggle))
	command.AddCommand(&cobra.Command{Use: "tools <server>", Short: "list MCP server tools", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		items, err := service.Tools(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "tools": items})
		}
		for _, item := range items {
			fmt.Fprintf(r.stdout, "%s\t%s\t%s\n", r.redact(item.Name), r.redact(item.LocalName), r.redact(item.Description))
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "tool <server> <name>", Short: "show an MCP tool", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		item, err := service.Tool(args[0], args[1])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "tool": item})
		}
		return r.writeMCPTextValue(item)
	}})
	command.AddCommand(mcpActionCommand("login", "authenticate an MCP server", backend, func(service mcpCommandBackend, name string) error { return service.Login(ctx, name) }, r))
	command.AddCommand(mcpActionCommand("logout", "clear MCP server authentication", backend, func(service mcpCommandBackend, name string) error { return service.Logout(ctx, name) }, r))
	command.AddCommand(&cobra.Command{Use: "resources <server>", Short: "list MCP server resources", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		items, err := service.Resources(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "resources": items})
		}
		for _, item := range items {
			fmt.Fprintf(r.stdout, "%s\t%s\t%s\n", r.redact(item.URI), r.redact(item.Name), r.redact(item.MIMEType))
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "resource <server> <uri>", Short: "read an MCP resource", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		item, err := service.Resource(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "resource": item})
		}
		return r.writeMCPTextValue(item)
	}})
	command.AddCommand(&cobra.Command{Use: "prompts <server>", Short: "list MCP server prompts", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		items, err := service.Prompts(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "prompts": items})
		}
		for _, item := range items {
			fmt.Fprintf(r.stdout, "%s\t%s\n", r.redact(item.Name), r.redact(item.Description))
		}
		return nil
	}})
	var promptArguments string
	prompt := &cobra.Command{Use: "prompt <server> <name>", Short: "get an MCP prompt", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		arguments := make(map[string]string)
		if err := json.Unmarshal([]byte(promptArguments), &arguments); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "arguments must be a JSON object with string values", Cause: err}
		}
		service, err := backend()
		if err != nil {
			return err
		}
		result, err := service.Prompt(ctx, args[0], args[1], arguments)
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "prompt": result})
		}
		return r.writeMCPTextValue(result)
	}}
	prompt.Flags().StringVar(&promptArguments, "arguments", "{}", "prompt arguments as a JSON object with string values")
	command.AddCommand(prompt)
	return command
}

func mcpActionCommand(verb, short string, load func() (mcpCommandBackend, error), action func(mcpCommandBackend, string) error, r *runtime) *cobra.Command {
	return &cobra.Command{Use: verb + " <server>", Short: short, Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := load()
		if err != nil {
			return err
		}
		if err := action(service, args[0]); err != nil {
			return err
		}
		return r.writeMCPAction(args[0], verb)
	}}
}

func (r *runtime) mcpToggleCommand(ctx context.Context, verb string, enabled bool, toggle mcpToggleServerFunc) *cobra.Command {
	return &cobra.Command{Use: verb + " <server>", Short: verb + " an MCP server", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		if toggle == nil {
			return mcpManagerCapabilityUnavailable("server enable/disable persistence")
		}
		if err := toggle(ctx, args[0], enabled); err != nil {
			return err
		}
		return r.writeMCPAction(args[0], verb)
	}}
}

func (r *runtime) writeMCPAction(server, action string) error {
	if r.output != "text" {
		return r.writeMCPJSON(map[string]any{"server": server, "action": action})
	}
	_, err := fmt.Fprintf(r.stdout, "%s\t%s\n", r.redact(server), action)
	return err
}

func (r *runtime) writeMCPJSON(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	return json.NewEncoder(r.stdout).Encode(r.redactMCPJSONValue(document))
}

func (r *runtime) redactMCPJSONValue(value any) any {
	switch typed := value.(type) {
	case string:
		return r.redact(typed)
	case []any:
		for index := range typed {
			typed[index] = r.redactMCPJSONValue(typed[index])
		}
		return typed
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			redacted[r.redact(key)] = r.redactMCPJSONValue(item)
		}
		return redacted
	default:
		return value
	}
}

func (r *runtime) writeMCPTextValue(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(r.stdout, r.redact(string(encoded)))
	return err
}
