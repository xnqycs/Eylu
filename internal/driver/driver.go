package driver

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"Eylu/internal/protocol"
)

type Capabilities struct {
	TextStreaming bool `json:"text_streaming"`
	ToolCalling   bool `json:"tool_calling"`
	ParallelTools bool `json:"parallel_tools"`
	Reasoning     bool `json:"reasoning"`
	ImageInput    bool `json:"image_input"`
	RemoteSession bool `json:"remote_session"`
}

type Request struct {
	BaseURL string
	APIKey  string
	Headers map[string]string
	Stream  bool
	Model   protocol.ModelRequest
}

type EmitFunc func(protocol.ModelEvent) error

type ModelDriver interface {
	Name() string
	Capabilities() Capabilities
	Generate(context.Context, Request, EmitFunc) (protocol.ModelResponse, error)
}

type Registry struct {
	mu      sync.RWMutex
	drivers map[string]ModelDriver
}

func NewRegistry(drivers ...ModelDriver) *Registry {
	r := &Registry{drivers: make(map[string]ModelDriver)}
	for _, d := range drivers {
		r.Register(d)
	}
	return r
}

func (r *Registry) Register(d ModelDriver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[d.Name()] = d
}

func (r *Registry) Get(name string) (ModelDriver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[name]
	if !ok {
		return nil, fmt.Errorf("unknown model driver %q", name)
	}
	return d, nil
}

func DefaultHTTPClient(timeoutSeconds int) *http.Client {
	return &http.Client{Timeout: durationSeconds(timeoutSeconds)}
}
