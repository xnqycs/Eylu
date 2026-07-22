package webtool

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

func TestLocalWebToolDefaultsPermissionToAllow(t *testing.T) {
	item := NewLocalTool(ResolvedTool{Definition: protocol.ToolDefinition{Kind: protocol.ToolWebSearch, Name: "web_search"}}, nil, nil, "", NewUsageBudget())
	outcome, overridden := item.OverridePolicy(json.RawMessage(`{"query":"Eylu"}`))
	if !overridden || outcome.Decision != policy.DecisionAllow || outcome.Confirmations != 0 {
		t.Fatalf("outcome=%#v overridden=%t", outcome, overridden)
	}
}

func TestDelegatedWebToolBatchExecutesConcurrently(t *testing.T) {
	var ready atomic.Int32
	var release sync.Once
	gate := make(chan struct{})
	delegate := func(_ context.Context, _ ResolvedTool, input json.RawMessage) protocol.ToolResult {
		if ready.Add(1) == 2 {
			release.Do(func() { close(gate) })
		}
		select {
		case <-gate:
			return protocol.ToolResult{Content: string(input)}
		case <-time.After(time.Second):
			return protocol.ToolResult{Content: "parallel execution timed out", IsError: true}
		}
	}
	resolved := ResolvedTool{
		Definition: protocol.ToolDefinition{Kind: protocol.ToolWebSearch, Name: "web_search", MaxUses: 2, InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)},
		Execution:  protocol.ExecutionDelegated,
	}
	item := NewLocalTool(resolved, nil, delegate, "allow", NewUsageBudget())
	executor := &tool.Executor{Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}, MaxParallelTools: 2, Timeout: 2 * time.Second}
	results := executor.ExecuteConcurrent(context.Background(), "request", []protocol.ToolCall{
		{ID: "one", Name: "web_search", Arguments: json.RawMessage(`{"query":"one"}`)},
		{ID: "two", Name: "web_search", Arguments: json.RawMessage(`{"query":"two"}`)},
	})
	if ready.Load() != 2 || len(results) != 2 || results[0].IsError || results[1].IsError {
		t.Fatalf("ready=%d results=%#v", ready.Load(), results)
	}
}

func TestInputValuesNormalizesBatchAndEnforcesLimit(t *testing.T) {
	values, err := InputValues(protocol.ToolWebSearch, json.RawMessage(`{"queries":[" one ","two","one"]}`))
	if err != nil || len(values) != 2 || values[0] != "one" || values[1] != "two" {
		t.Fatalf("values=%#v err=%v", values, err)
	}
	tooMany := make([]string, MaxBatchQueries+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("query-%d", index)
	}
	raw, _ := json.Marshal(map[string]any{"queries": tooMany})
	if _, err := InputValues(protocol.ToolWebSearch, raw); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("limit error=%v", err)
	}
	if _, err := InputValues(protocol.ToolWebSearch, json.RawMessage(`{"query":"one","queries":["two"]}`)); err == nil {
		t.Fatal("query and queries were accepted together")
	}
}

func TestValidateURLDomainAndAddressPolicy(t *testing.T) {
	publicLookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	tests := []struct {
		name    string
		url     string
		allowed []string
		blocked []string
		lookup  func(context.Context, string, string) ([]net.IP, error)
		wantErr string
	}{
		{name: "exact allowed", url: "https://example.com/a", allowed: []string{"example.com"}, lookup: publicLookup},
		{name: "subdomain allowed", url: "https://docs.example.com", allowed: []string{"example.com"}, lookup: publicLookup},
		{name: "blocked wins", url: "https://docs.example.com", allowed: []string{"example.com"}, blocked: []string{"docs.example.com"}, lookup: publicLookup, wantErr: "blocked"},
		{name: "outside allowlist", url: "https://example.net", allowed: []string{"example.com"}, lookup: publicLookup, wantErr: "outside allowed_domains"},
		{name: "credentials", url: "https://user:secret@example.com", lookup: publicLookup, wantErr: "credentials"},
		{name: "loopback IPv4", url: "http://127.0.0.1", lookup: publicLookup, wantErr: "special-purpose"},
		{name: "private IPv4", url: "http://10.0.0.7", lookup: publicLookup, wantErr: "special-purpose"},
		{name: "loopback IPv6", url: "http://[::1]", lookup: publicLookup, wantErr: "special-purpose"},
		{name: "DNS private", url: "https://public.example", lookup: func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("192.168.1.10")}, nil
		}, wantErr: "resolves to a special-purpose"},
		{name: "scheme", url: "file:///etc/passwd", lookup: publicLookup, wantErr: "absolute HTTP(S)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateURL(context.Background(), test.url, test.allowed, test.blocked, test.lookup)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("err=%v want substring %q", err, test.wantErr)
			}
		})
	}
}
