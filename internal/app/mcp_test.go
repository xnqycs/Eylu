package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/mcpclient"
)

type fakeMCPCommandBackend struct {
	calls []string
}

func (f *fakeMCPCommandBackend) Servers() ([]mcpclient.ServerInfo, string) {
	f.calls = append(f.calls, "list")
	return []mcpclient.ServerInfo{{Name: "alpha", Status: "connected", Transport: "stdio", ProtocolVersion: "2025-11-25", Tools: 1, Resources: 1, Prompts: 1}}, "fingerprint"
}

func (f *fakeMCPCommandBackend) Inspect(name string) (mcpclient.ServerDetail, error) {
	f.calls = append(f.calls, "inspect:"+name)
	return mcpclient.ServerDetail{ServerInfo: mcpclient.ServerInfo{Name: name, Status: "connected"}, Instructions: "stored-secret"}, nil
}

func (f *fakeMCPCommandBackend) Reconnect(_ context.Context, name string) error {
	f.calls = append(f.calls, "reconnect:"+name)
	return nil
}

func (f *fakeMCPCommandBackend) Tools(name string) ([]mcpclient.ToolInfo, error) {
	f.calls = append(f.calls, "tools:"+name)
	return []mcpclient.ToolInfo{{Name: "search", LocalName: "mcp__alpha__search", Description: "stored-secret"}}, nil
}

func (f *fakeMCPCommandBackend) Tool(server, name string) (mcpclient.ToolInfo, error) {
	f.calls = append(f.calls, "tool:"+server+":"+name)
	return mcpclient.ToolInfo{Name: name, LocalName: "mcp__" + server + "__" + name, InputSchema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (f *fakeMCPCommandBackend) Login(_ context.Context, name string) error {
	f.calls = append(f.calls, "login:"+name)
	return nil
}

func (f *fakeMCPCommandBackend) Logout(_ context.Context, name string) error {
	f.calls = append(f.calls, "logout:"+name)
	return nil
}

func (f *fakeMCPCommandBackend) Resources(name string) ([]mcpclient.ResourceInfo, error) {
	f.calls = append(f.calls, "resources:"+name)
	return []mcpclient.ResourceInfo{{URI: "fixture://readme", Name: "readme", MIMEType: "text/plain"}}, nil
}

func (f *fakeMCPCommandBackend) Resource(_ context.Context, server, uri string) (any, error) {
	f.calls = append(f.calls, "resource:"+server+":"+uri)
	return map[string]any{"uri": uri, "text": "stored-secret"}, nil
}

func (f *fakeMCPCommandBackend) Prompts(name string) ([]mcpclient.PromptInfo, error) {
	f.calls = append(f.calls, "prompts:"+name)
	return []mcpclient.PromptInfo{{Name: "review", Description: "review code"}}, nil
}

func (f *fakeMCPCommandBackend) Prompt(_ context.Context, server, name string, arguments map[string]string) (any, error) {
	encoded, _ := json.Marshal(arguments)
	f.calls = append(f.calls, fmt.Sprintf("prompt:%s:%s:%s", server, name, encoded))
	return map[string]any{"description": "stored-secret", "messages": []any{}}, nil
}

func TestMCPCommandRegistersManagementSurface(t *testing.T) {
	runtime := &runtime{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, output: "text"}
	command := runtime.mcpCommand(context.Background())
	want := []string{"disable", "enable", "inspect", "list", "login", "logout", "prompt", "prompts", "reconnect", "resource", "resources", "tool", "tools"}
	got := make([]string, 0, len(command.Commands()))
	for _, child := range command.Commands() {
		got = append(got, child.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("commands=%v want=%v", got, want)
	}
}

func TestMCPCommandsRouteArgumentsAndRedactJSON(t *testing.T) {
	tests := []struct {
		args     []string
		wantCall string
	}{
		{args: []string{"list"}, wantCall: "list"},
		{args: []string{"inspect", "alpha"}, wantCall: "inspect:alpha"},
		{args: []string{"reconnect", "alpha"}, wantCall: "reconnect:alpha"},
		{args: []string{"tools", "alpha"}, wantCall: "tools:alpha"},
		{args: []string{"tool", "alpha", "search"}, wantCall: "tool:alpha:search"},
		{args: []string{"login", "alpha"}, wantCall: "login:alpha"},
		{args: []string{"logout", "alpha"}, wantCall: "logout:alpha"},
		{args: []string{"resources", "alpha"}, wantCall: "resources:alpha"},
		{args: []string{"resource", "alpha", "fixture://readme"}, wantCall: "resource:alpha:fixture://readme"},
		{args: []string{"prompts", "alpha"}, wantCall: "prompts:alpha"},
		{args: []string{"prompt", "alpha", "review", "--arguments", `{"language":"go"}`}, wantCall: `prompt:alpha:review:{"language":"go"}`},
	}
	for _, testCase := range tests {
		t.Run(strings.Join(testCase.args, "_"), func(t *testing.T) {
			backend := &fakeMCPCommandBackend{}
			var stdout bytes.Buffer
			runtime := &runtime{stdout: &stdout, stderr: &bytes.Buffer{}, output: "json", apiKeys: []string{"stored-secret"}}
			command := runtime.mcpCommandWithBackend(context.Background(), func(context.Context) (mcpCommandBackend, error) { return backend, nil }, nil)
			command.SetArgs(testCase.args)
			command.SetOut(&stdout)
			command.SetErr(runtime.stderr)
			if err := command.Execute(); err != nil {
				t.Fatalf("Execute() error=%v", err)
			}
			if len(backend.calls) != 1 || backend.calls[0] != testCase.wantCall {
				t.Fatalf("calls=%v want=%q", backend.calls, testCase.wantCall)
			}
			if strings.Contains(stdout.String(), "stored-secret") || !json.Valid(bytes.TrimSpace(stdout.Bytes())) {
				t.Fatalf("output must be valid redacted JSON: %q", stdout.String())
			}
		})
	}
}

func TestMCPEnableDisableUseConfigurationStore(t *testing.T) {
	for _, testCase := range []struct {
		verb    string
		enabled bool
	}{
		{verb: "enable", enabled: true},
		{verb: "disable", enabled: false},
	} {
		t.Run(testCase.verb, func(t *testing.T) {
			var gotName string
			var gotEnabled bool
			var stdout bytes.Buffer
			runtime := &runtime{stdout: &stdout, stderr: &bytes.Buffer{}, output: "json"}
			command := runtime.mcpCommandWithBackend(context.Background(), nil, func(_ context.Context, name string, enabled bool) error {
				gotName, gotEnabled = name, enabled
				return nil
			})
			command.SetArgs([]string{testCase.verb, "alpha"})
			if err := command.Execute(); err != nil {
				t.Fatalf("Execute() error=%v", err)
			}
			if gotName != "alpha" || gotEnabled != testCase.enabled {
				t.Fatalf("name=%q enabled=%t", gotName, gotEnabled)
			}
		})
	}
}

func TestMCPPromptRejectsNonStringArguments(t *testing.T) {
	runtime := &runtime{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, output: "json"}
	command := runtime.mcpCommandWithBackend(context.Background(), func(context.Context) (mcpCommandBackend, error) {
		return &fakeMCPCommandBackend{}, nil
	}, nil)
	command.SetArgs([]string{"prompt", "alpha", "review", "--arguments", `{"count":1}`})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "arguments") {
		t.Fatalf("error=%v", err)
	}
}

func TestMCPListAndToggleConfiguredDisabledServer(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "config.toml")
	configText := "version = 1\n\n[mcp_servers.alpha]\ntransport = \"stdio\"\nenabled = false\ncommand = \"missing-mcp-command\"\n"
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"--config", configPath, "--workspace", workspace, "--output", "json", "mcp", "list"}
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("list code=%d stderr=%q", code, stderr.String())
	}
	var listed struct {
		Servers []mcpclient.ServerInfo `json:"servers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v output=%q", err, stdout.String())
	}
	if len(listed.Servers) != 1 || listed.Servers[0].Name != "alpha" || listed.Servers[0].Status != "disabled" {
		t.Fatalf("servers=%#v", listed.Servers)
	}

	stdout.Reset()
	stderr.Reset()
	args[len(args)-1] = "enable"
	args = append(args, "alpha")
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("enable code=%d stderr=%q", code, stderr.String())
	}
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "enabled = true") {
		t.Fatalf("enabled config=%q", updated)
	}

	stdout.Reset()
	stderr.Reset()
	args[len(args)-2] = "disable"
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("disable code=%d stderr=%q", code, stderr.String())
	}
	updated, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "enabled = false") {
		t.Fatalf("disabled config=%q", updated)
	}
}
