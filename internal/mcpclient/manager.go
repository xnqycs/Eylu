package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/config"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

const (
	maxToolsPerServer     = 256
	maxResourcesPerServer = 512
	maxInstructionsBytes  = 64 << 10
	maxSchemaBytes        = 256 << 10
	maxResourceCatalog    = 128 << 10
	maxToolDescription    = 16 << 10
	maxLocalToolNameBytes = 64
)

type Diagnostic struct {
	Server  string `json:"server"`
	Message string `json:"message"`
}

type Context struct {
	Server          string                    `json:"server"`
	ProtocolVersion string                    `json:"protocol_version"`
	Instructions    string                    `json:"instructions,omitempty"`
	ToolDefinitions []protocol.ToolDefinition `json:"tool_definitions,omitempty"`
	ResourceCatalog string                    `json:"resource_catalog,omitempty"`
}

type ServerInfo struct {
	Name            string `json:"name"`
	Implementation  string `json:"implementation"`
	Version         string `json:"version"`
	ProtocolVersion string `json:"protocol_version"`
	Tools           int    `json:"tools"`
	Resources       int    `json:"resources"`
}

type Manager struct {
	servers     []*serverRuntime
	tools       []tool.Tool
	contexts    []Context
	infos       []ServerInfo
	fingerprint string
}

type serverRuntime struct {
	name      string
	session   *sdkmcp.ClientSession
	readOnly  map[string]bool
	resources []*sdkmcp.Resource
}

func Open(ctx context.Context, servers map[string]config.MCPServerConfig, workspace string) (*Manager, []Diagnostic, error) {
	manager := &Manager{}
	diagnostics := make([]Diagnostic, 0)
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	usedToolNames := make(map[string]string)
	for _, name := range names {
		serverConfig := servers[name]
		if serverConfig.Disabled {
			continue
		}
		serverToolNames := cloneToolNames(usedToolNames)
		server, serverContext, info, serverTools, err := connectServer(ctx, name, serverConfig, workspace, serverToolNames)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{Server: name, Message: err.Error()})
			continue
		}
		usedToolNames = serverToolNames
		manager.servers = append(manager.servers, server)
		manager.contexts = append(manager.contexts, serverContext)
		manager.infos = append(manager.infos, info)
		manager.tools = append(manager.tools, serverTools...)
	}
	encoded, err := json.Marshal(manager.contexts)
	if err != nil {
		_ = manager.Close()
		return nil, diagnostics, err
	}
	digest := sha256.Sum256(encoded)
	manager.fingerprint = hex.EncodeToString(digest[:])
	return manager, diagnostics, nil
}

func connectServer(ctx context.Context, name string, serverConfig config.MCPServerConfig, workspace string, usedToolNames map[string]string) (*serverRuntime, Context, ServerInfo, []tool.Tool, error) {
	workingDirectory, err := resolveWorkingDirectory(workspace, serverConfig.WorkingDirectory)
	if err != nil {
		return nil, Context{}, ServerInfo{}, nil, err
	}
	command := exec.Command(serverConfig.Command, serverConfig.Args...)
	command.Dir = workingDirectory
	command.Env = serverEnvironment(serverConfig.Environment)
	timeout := time.Duration(serverConfig.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	connectContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "eylu", Version: "1.0.0"}, &sdkmcp.ClientOptions{Capabilities: &sdkmcp.ClientCapabilities{}})
	session, err := client.Connect(connectContext, &sdkmcp.CommandTransport{Command: command, TerminateDuration: 2 * time.Second}, nil)
	if err != nil {
		return nil, Context{}, ServerInfo{}, nil, fmt.Errorf("connect MCP server: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = session.Close()
		}
	}()
	initialized := session.InitializeResult()
	if initialized == nil || initialized.ServerInfo == nil {
		return nil, Context{}, ServerInfo{}, nil, errors.New("MCP server returned incomplete initialization metadata")
	}
	runtime := &serverRuntime{name: name, session: session, readOnly: make(map[string]bool)}
	for _, readOnly := range serverConfig.ReadOnlyTools {
		runtime.readOnly[readOnly] = true
	}
	contextValue := Context{Server: name, ProtocolVersion: initialized.ProtocolVersion, Instructions: limitUTF8(initialized.Instructions, maxInstructionsBytes)}
	info := ServerInfo{Name: name, Implementation: initialized.ServerInfo.Name, Version: initialized.ServerInfo.Version, ProtocolVersion: initialized.ProtocolVersion}
	tools := make([]tool.Tool, 0)
	for remoteTool, listErr := range session.Tools(connectContext, nil) {
		if listErr != nil {
			return nil, Context{}, ServerInfo{}, nil, fmt.Errorf("list MCP tools: %w", listErr)
		}
		if len(contextValue.ToolDefinitions) >= maxToolsPerServer {
			return nil, Context{}, ServerInfo{}, nil, fmt.Errorf("MCP tool limit exceeds %d", maxToolsPerServer)
		}
		definition, localName, convertErr := convertToolDefinition(name, remoteTool)
		if convertErr != nil {
			return nil, Context{}, ServerInfo{}, nil, convertErr
		}
		if previous := usedToolNames[localName]; previous != "" {
			return nil, Context{}, ServerInfo{}, nil, fmt.Errorf("MCP tool name %q collides with %s", localName, previous)
		}
		usedToolNames[localName] = name + ":" + remoteTool.Name
		contextValue.ToolDefinitions = append(contextValue.ToolDefinitions, definition)
		tools = append(tools, &remoteToolAdapter{server: runtime, remote: remoteTool, definition: definition, readOnly: runtime.readOnly[remoteTool.Name]})
	}
	for resource, listErr := range session.Resources(connectContext, nil) {
		if listErr != nil {
			return nil, Context{}, ServerInfo{}, nil, fmt.Errorf("list MCP resources: %w", listErr)
		}
		if len(runtime.resources) >= maxResourcesPerServer {
			return nil, Context{}, ServerInfo{}, nil, fmt.Errorf("MCP resource limit exceeds %d", maxResourcesPerServer)
		}
		runtime.resources = append(runtime.resources, resource)
	}
	if len(runtime.resources) > 0 {
		resourceTool, definition, convertErr := newResourceTool(runtime)
		if convertErr != nil {
			return nil, Context{}, ServerInfo{}, nil, convertErr
		}
		if previous := usedToolNames[definition.Name]; previous != "" {
			return nil, Context{}, ServerInfo{}, nil, fmt.Errorf("MCP resource tool name %q collides with %s", definition.Name, previous)
		}
		usedToolNames[definition.Name] = name + ":resources"
		tools = append(tools, resourceTool)
		contextValue.ToolDefinitions = append(contextValue.ToolDefinitions, definition)
		catalog, marshalErr := json.Marshal(runtime.resources)
		if marshalErr != nil {
			return nil, Context{}, ServerInfo{}, nil, marshalErr
		}
		contextValue.ResourceCatalog = limitUTF8(string(catalog), maxResourceCatalog)
	}
	info.Tools = len(contextValue.ToolDefinitions)
	info.Resources = len(runtime.resources)
	closeOnError = false
	return runtime, contextValue, info, tools, nil
}

func (m *Manager) Tools() []tool.Tool    { return append([]tool.Tool(nil), m.tools...) }
func (m *Manager) Contexts() []Context   { return append([]Context(nil), m.contexts...) }
func (m *Manager) Servers() []ServerInfo { return append([]ServerInfo(nil), m.infos...) }
func (m *Manager) Fingerprint() string   { return m.fingerprint }

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var closeErrors []error
	for index := len(m.servers) - 1; index >= 0; index-- {
		if err := m.servers[index].session.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close MCP server %s: %w", m.servers[index].name, err))
		}
	}
	m.servers = nil
	return errors.Join(closeErrors...)
}

func resolveWorkingDirectory(workspace, configured string) (string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve MCP workspace: %w", err)
	}
	candidate := root
	if configured != "" {
		if filepath.IsAbs(configured) {
			candidate = filepath.Clean(configured)
		} else {
			candidate = filepath.Join(root, configured)
		}
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || pathEscapes(relative) {
		return "", errors.New("MCP working directory escapes the workspace")
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("MCP working directory is unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("MCP working directory is not a directory")
	}
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve MCP working directory: %w", err)
	}
	realRelative, err := filepath.Rel(realRoot, realCandidate)
	if err != nil || pathEscapes(realRelative) {
		return "", errors.New("MCP working directory resolves outside the workspace")
	}
	return realCandidate, nil
}

func pathEscapes(relative string) bool {
	return filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func serverEnvironment(names []string) []string {
	allowed := append([]string{"PATH", "SystemRoot", "WINDIR", "HOME", "USERPROFILE", "TMP", "TEMP"}, names...)
	seen := make(map[string]bool, len(allowed))
	environment := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if seen[name] {
			continue
		}
		seen[name] = true
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func convertToolDefinition(server string, remote *sdkmcp.Tool) (protocol.ToolDefinition, string, error) {
	if remote == nil || strings.TrimSpace(remote.Name) == "" {
		return protocol.ToolDefinition{}, "", errors.New("MCP server returned a tool without a name")
	}
	localName := localToolName(server, remote.Name)
	schema, err := json.Marshal(remote.InputSchema)
	if err != nil || !json.Valid(schema) || len(schema) > maxSchemaBytes {
		return protocol.ToolDefinition{}, "", fmt.Errorf("MCP tool %s has an invalid or oversized input schema", remote.Name)
	}
	if string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	description := limitUTF8(fmt.Sprintf("MCP server %s tool %s. %s", server, remote.Name, remote.Description), maxToolDescription)
	return protocol.ToolDefinition{Name: localName, Description: strings.TrimSpace(description), InputSchema: schema}, localName, nil
}

func sanitizeName(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); index++ {
		current := value[index]
		if current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || current >= '0' && current <= '9' || current == '_' || current == '-' {
			result.WriteByte(current)
		} else {
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "unnamed"
	}
	return result.String()
}

func localToolName(server, remote string) string {
	safeServer, safeRemote := sanitizeName(server), sanitizeName(remote)
	name := "mcp__" + safeServer + "__" + safeRemote
	needsSuffix := safeServer != server || safeRemote != remote || len(name) > maxLocalToolNameBytes
	if !needsSuffix {
		return name
	}
	digest := sha256.Sum256([]byte(server + "\x00" + remote))
	suffix := "_" + hex.EncodeToString(digest[:6])
	prefixBytes := maxLocalToolNameBytes - len(suffix)
	if len(name) > prefixBytes {
		name = name[:prefixBytes]
	}
	return name + suffix
}

func cloneToolNames(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for name, owner := range source {
		clone[name] = owner
	}
	return clone
}

func limitUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "\n[truncated]"
}
