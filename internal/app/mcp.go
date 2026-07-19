package app

import (
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
	}{Workspace: cfg.Workspace, Servers: cfg.MCPServers})
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
	manager, diagnostics, err := mcpclient.Open(ctx, cfg.MCPServers, cfg.Workspace)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize MCP runtime: " + err.Error(), Cause: err}
	}
	r.mcp, r.mcpKey = manager, key
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(r.stderr, "[mcp] server=%s error=%s\n", diagnostic.Server, diagnostic.Message)
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

func (r *runtime) mcpCommand(ctx context.Context) *cobra.Command {
	command := &cobra.Command{Use: "mcp", Short: "inspect configured MCP stdio servers"}
	command.AddCommand(&cobra.Command{Use: "list", Short: "connect and list MCP servers", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		manager, err := r.loadMCP(ctx, loaded.Config)
		if err != nil {
			return err
		}
		servers := manager.Servers()
		if r.output != "text" {
			return json.NewEncoder(r.stdout).Encode(map[string]any{"servers": servers, "fingerprint": manager.Fingerprint()})
		}
		if len(servers) == 0 {
			fmt.Fprintln(r.stdout, "No connected MCP servers.")
			return nil
		}
		for _, server := range servers {
			fmt.Fprintf(r.stdout, "%s\t%s@%s\tprotocol=%s\ttools=%d\tresources=%d\n", server.Name, server.Implementation, server.Version, server.ProtocolVersion, server.Tools, server.Resources)
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "inspect <name>", Short: "show MCP instructions, tools, and resources", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		manager, err := r.loadMCP(ctx, loaded.Config)
		if err != nil {
			return err
		}
		for _, serverContext := range manager.Contexts() {
			if serverContext.Server != args[0] {
				continue
			}
			if r.output != "text" {
				return json.NewEncoder(r.stdout).Encode(serverContext)
			}
			fmt.Fprintf(r.stdout, "Server: %s\nProtocol: %s\nInstructions:\n%s\nResources:\n%s\nTools:\n", serverContext.Server, serverContext.ProtocolVersion, serverContext.Instructions, serverContext.ResourceCatalog)
			for _, definition := range serverContext.ToolDefinitions {
				fmt.Fprintf(r.stdout, "%s\t%s\n", definition.Name, definition.Description)
			}
			return nil
		}
		return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("connected MCP server %q was not found", args[0])}
	}})
	return command
}
