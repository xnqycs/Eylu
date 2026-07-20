package driver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

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
	BaseURL           string
	APIKey            string
	Headers           map[string]string
	ReasoningEffort   string
	ParallelToolCalls bool
	Stream            bool
	Model             protocol.ModelRequest
}

type EmitFunc func(protocol.ModelEvent) error

const (
	toolCallDeltaMinBatchBytes = 24
	toolCallDeltaMaxBatchBytes = 256
	toolCallDeltaMaxDelay      = 250 * time.Millisecond
)

type StreamDeltaBuffer struct {
	pending strings.Builder
	started time.Time
}

func (b *StreamDeltaBuffer) Push(delta string, now time.Time) (string, bool) {
	if delta == "" {
		return "", false
	}
	if b.pending.Len() == 0 {
		b.started = now
	}
	b.pending.WriteString(delta)
	size := b.pending.Len()
	ready := size >= toolCallDeltaMaxBatchBytes ||
		(size >= toolCallDeltaMinBatchBytes && (now.Sub(b.started) >= toolCallDeltaMaxDelay || strings.Contains(delta, `\n`)))
	if !ready {
		return "", false
	}
	return b.Flush(), true
}

func (b *StreamDeltaBuffer) Flush() string {
	if b.pending.Len() == 0 {
		return ""
	}
	batch := strings.Clone(b.pending.String())
	b.pending.Reset()
	b.started = time.Time{}
	return batch
}

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

// StreamingHTTPClient preserves the shared transport and client policies while
// removing the total timeout that would otherwise interrupt response body reads.
func StreamingHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	if client.Timeout == 0 {
		return client
	}
	streamClient := *client
	streamClient.Timeout = 0
	return &streamClient
}
