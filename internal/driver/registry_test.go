package driver

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"Eylu/internal/protocol"
)

// stubDriver is the smallest thing the registry and the capability lookup can be
// asked about.
type stubDriver struct {
	name         string
	capabilities Capabilities
	targeted     map[CapabilityTarget]Capabilities
}

func (d *stubDriver) Name() string               { return d.name }
func (d *stubDriver) Capabilities() Capabilities { return d.capabilities }
func (d *stubDriver) Generate(context.Context, Request, EmitFunc) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{}, nil
}
func (d *stubDriver) CapabilitiesFor(target CapabilityTarget) Capabilities {
	return d.targeted[target]
}

// The registry is keyed by the driver's own name, and an unknown name is an error
// rather than a nil driver the caller would have to check for.
func TestRegistryResolvesByDriverNameAndRefusesAnUnknownOne(t *testing.T) {
	first := &stubDriver{name: "first"}
	second := &stubDriver{name: "second"}
	registry := NewRegistry(first, second)

	for _, want := range []string{"first", "second"} {
		got, err := registry.Get(want)
		if err != nil || got.Name() != want {
			t.Fatalf("Get(%q) = %#v, %v", want, got, err)
		}
	}
	if _, err := registry.Get("missing"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Get(missing) = %v, want an error naming it", err)
	}

	// Registering under a name that is taken replaces it, which is what lets a
	// later registration win for one provider without rebuilding the registry.
	replacement := &stubDriver{name: "first"}
	registry.Register(replacement)
	if got, err := registry.Get("first"); err != nil || got != ModelDriver(replacement) {
		t.Fatalf("after re-registering: %#v, %v", got, err)
	}
	// An empty registry is still a registry.
	if _, err := NewRegistry().Get("anything"); err == nil {
		t.Fatal("an empty registry resolved a name")
	}
}

// plainDriver answers the untargeted question only, which is what a driver that
// never specialised per target looks like.
type plainDriver struct{ capabilities Capabilities }

func (d *plainDriver) Name() string               { return "plain" }
func (d *plainDriver) Capabilities() Capabilities { return d.capabilities }
func (d *plainDriver) Generate(context.Context, Request, EmitFunc) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{}, nil
}

// The capability lookup prefers the targeted answer when the driver has one, and
// falls back to the untargeted one when it does not. Getting this wrong would make
// a request believe a provider supports something it does not.
func TestCapabilitiesForPrefersTheTargetedAnswer(t *testing.T) {
	target := CapabilityTarget{Provider: "work", Protocol: "responses", Model: "test-model"}
	targeted := &stubDriver{
		name:         "targeted",
		capabilities: Capabilities{ToolCalling: true},
		targeted:     map[CapabilityTarget]Capabilities{target: {ToolCalling: true, ParallelTools: true}},
	}
	if got := CapabilitiesFor(targeted, target); !got.ParallelTools {
		t.Fatalf("targeted capabilities = %#v, want the targeted answer", got)
	}
	// A target the driver does not know answers with the zero value, not with the
	// untargeted capabilities: the driver said it knows this target, and it does
	// not claim anything about it.
	if got := CapabilitiesFor(targeted, CapabilityTarget{Model: "other"}); got.ToolCalling || got.ParallelTools {
		t.Fatalf("unknown target = %#v, want none", got)
	}
	// A driver that does not implement the targeted form at all answers with its
	// own capabilities: the fallback is the interface, not the zero value.
	plain := &plainDriver{capabilities: Capabilities{Reasoning: true}}
	if got := CapabilitiesFor(plain, target); !got.Reasoning {
		t.Fatalf("untargeted capabilities = %#v", got)
	}
}

// The delta buffer exists to bound how often a streamed tool-call argument is
// forwarded. It has to release on size, on age and on a boundary the model wrote
// itself, and it must never hold a fragment back once the stream ends.
func TestStreamDeltaBufferReleasesOnSizeAgeAndContent(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	// An empty delta is not a batch and does not start the clock.
	var buffer StreamDeltaBuffer
	if batch, ready := buffer.Push("", start); ready || batch != "" {
		t.Fatalf("an empty delta released %q", batch)
	}
	if batch, ready := buffer.Push("small", start); ready || batch != "" {
		t.Fatalf("a small delta released %q", batch)
	}
	if batch := buffer.Flush(); batch != "small" {
		t.Fatalf("flush = %q, want the held fragment", batch)
	}
	if batch := buffer.Flush(); batch != "" {
		t.Fatalf("a second flush released %q", batch)
	}

	// Size alone releases, at the upper bound.
	var sized StreamDeltaBuffer
	large := strings.Repeat("x", toolCallDeltaMaxBatchBytes)
	if batch, ready := sized.Push(large, start); !ready || batch != large {
		t.Fatalf("a full batch released %q (ready %t)", batch, ready)
	}

	// Age releases once the fragment is big enough to be worth batching.
	var aged StreamDeltaBuffer
	fragment := strings.Repeat("y", toolCallDeltaMinBatchBytes)
	if _, ready := aged.Push(fragment, start); ready {
		t.Fatal("a fresh fragment was released before its delay")
	}
	if batch, ready := aged.Push("z", start.Add(toolCallDeltaMaxDelay)); !ready || batch != fragment+"z" {
		t.Fatalf("an aged fragment released %q (ready %t)", batch, ready)
	}

	// So does a newline the model wrote, which is where an argument boundary is.
	var bounded StreamDeltaBuffer
	if _, ready := bounded.Push(fragment, start); ready {
		t.Fatal("a fresh fragment was released before its boundary")
	}
	if batch, ready := bounded.Push(`\n`, start); !ready || batch != fragment+`\n` {
		t.Fatalf("a newline-terminated fragment released %q (ready %t)", batch, ready)
	}

	// A fragment below the minimum is not released by age: the delay is a batching
	// decision, and a batch smaller than the minimum would be forwarded one delta
	// at a time anyway.
	var belowMinimum StreamDeltaBuffer
	if _, ready := belowMinimum.Push("ab", start); ready {
		t.Fatal("a fragment below the minimum was released")
	}
	if batch, ready := belowMinimum.Push("c", start.Add(time.Hour)); ready || batch != "" {
		t.Fatalf("a fragment below the minimum was released after an hour: %q", batch)
	}
	if batch := belowMinimum.Flush(); batch != "abc" {
		t.Fatalf("flush = %q", batch)
	}
}

// The two HTTP clients have one difference and it is deliberate: the streaming one
// has no total timeout, because a total timeout would interrupt the body read of a
// response that is streaming. Everything else about the client is preserved.
func TestHTTPClientsDifferOnlyInTheTotalTimeout(t *testing.T) {
	if got := DefaultHTTPClient(30).Timeout; got != 30*time.Second {
		t.Fatalf("default client timeout = %s", got)
	}
	if got := durationSeconds(0); got != 0 {
		t.Fatalf("durationSeconds(0) = %s", got)
	}

	original := &http.Client{Timeout: 45 * time.Second, Transport: http.DefaultTransport}
	streaming := StreamingHTTPClient(original)
	if streaming == original {
		t.Fatal("the streaming client is the original, so the original lost its timeout")
	}
	if streaming.Timeout != 0 {
		t.Fatalf("streaming timeout = %s, want none", streaming.Timeout)
	}
	if streaming.Transport != original.Transport {
		t.Fatal("the streaming client lost the shared transport")
	}
	if original.Timeout != 45*time.Second {
		t.Fatalf("the original client was mutated: %s", original.Timeout)
	}

	// A client that already has no timeout is returned as it is, so a caller that
	// configured its own transport keeps the same client.
	forever := &http.Client{Transport: http.DefaultTransport}
	if got := StreamingHTTPClient(forever); got != forever {
		t.Fatal("a client without a timeout was copied")
	}
	// A nil client means the default one, which has no timeout either.
	if got := StreamingHTTPClient(nil); got == nil || got.Timeout != 0 {
		t.Fatalf("StreamingHTTPClient(nil) = %#v", got)
	}
}
