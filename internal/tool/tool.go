package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type Tool interface {
	Definition() protocol.ToolDefinition
	Risk() policy.Risk
	Execute(context.Context, json.RawMessage) protocol.ToolResult
}

// ParallelSafe is an explicit opt-in for tools whose executions do not mutate
// shared state and may run beside other calls from the same model response.
type ParallelSafe interface {
	ParallelSafe() bool
}

type ConcurrencyMode string

const (
	ConcurrencyShared    ConcurrencyMode = "shared"
	ConcurrencyClaimed   ConcurrencyMode = "claimed"
	ConcurrencyExclusive ConcurrencyMode = "exclusive"
)

type ResourceKind string

const (
	ResourceFile ResourceKind = "file"
	ResourceTree ResourceKind = "tree"
)

type ResourceAccess string

const (
	ResourceRead  ResourceAccess = "read"
	ResourceWrite ResourceAccess = "write"
)

type ResourceClaim struct {
	Kind   ResourceKind   `json:"kind"`
	Path   string         `json:"path"`
	Access ResourceAccess `json:"access"`
}

type ConcurrencySpec struct {
	Mode   ConcurrencyMode `json:"mode"`
	Claims []ResourceClaim `json:"claims,omitempty"`
}

// ConcurrencyClassifier lets a tool classify each call after policy checks.
// Tools without this interface use the fail-closed compatibility rules.
type ConcurrencyClassifier interface {
	ClassifyConcurrency(json.RawMessage, policy.Outcome) ConcurrencySpec
}

// ExecutorTimeoutPolicy lets interactive tools rely on the parent request
// context instead of the per-tool execution deadline.
type ExecutorTimeoutPolicy interface {
	UseExecutorTimeout() bool
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry(tools ...Tool) *Registry {
	registry := &Registry{tools: make(map[string]Tool)}
	for _, item := range tools {
		if err := registry.Register(item); err != nil {
			panic(err)
		}
	}
	return registry
}

func (r *Registry) Register(item Tool) error {
	if item == nil {
		return fmt.Errorf("tool is nil")
	}
	definition := item.Definition()
	if definition.Name == "" {
		return fmt.Errorf("tool name is required")
	}
	if !json.Valid(definition.InputSchema) {
		return fmt.Errorf("tool %q has invalid input schema", definition.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[definition.Name]; exists {
		return fmt.Errorf("tool %q is already registered", definition.Name)
	}
	r.tools[definition.Name] = item
	return nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, ok := r.tools[name]
	return item, ok
}

func (r *Registry) Definitions() []protocol.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	definitions := make([]protocol.ToolDefinition, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, r.tools[name].Definition())
	}
	return definitions
}

func decodeStrict(input json.RawMessage, target any) error {
	decoder := json.NewDecoder(newJSONReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
