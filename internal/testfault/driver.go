package testfault

import (
	"context"
	"errors"
	"time"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// DriverFaults wraps a driver.ModelDriver. It injects transport failures and
// provider timeouts and can rewrite the response the request sees, all without
// the driver or the loop holding a test-only branch.
type DriverFaults struct {
	Delegate driver.ModelDriver
	Fault    *Fault
	// Err, when set, replaces the error the schedule would return, so a case can
	// inject a realistic provider failure or error code.
	Err error
	// Hang, when positive, makes a faulted call wait for its context to be done
	// before reporting that error. It is how a provider timeout is injected: the
	// request's own deadline decides when the wait ends, and the fault never
	// fabricates a deadline the caller did not set.
	Hang time.Duration
	// Rewrite, when set, sees every successful response before it is returned. It
	// is how a case makes a reported usage inexact or flips a stop reason.
	Rewrite func(*protocol.ModelResponse)
}

var _ driver.ModelDriver = (*DriverFaults)(nil)
var _ driver.TargetCapabilityDriver = (*DriverFaults)(nil)

// Name reports the delegate's name so nothing downstream has to know it is
// talking to a fault.
func (d *DriverFaults) Name() string {
	if d.Delegate == nil {
		return "testfault"
	}
	return d.Delegate.Name()
}

// Capabilities forwards the delegate's capabilities.
func (d *DriverFaults) Capabilities() driver.Capabilities {
	if d.Delegate == nil {
		return driver.Capabilities{}
	}
	return d.Delegate.Capabilities()
}

// CapabilitiesFor forwards the targeted capability query. Implementing it keeps
// the wrapper transparent: driver.CapabilitiesFor would otherwise fall back to
// the untargeted capabilities.
func (d *DriverFaults) CapabilitiesFor(target driver.CapabilityTarget) driver.Capabilities {
	if d.Delegate == nil {
		return driver.Capabilities{}
	}
	return driver.CapabilitiesFor(d.Delegate, target)
}

// Generate fails on schedule, optionally after waiting for the context to be
// done, and otherwise forwards to the delegate.
func (d *DriverFaults) Generate(ctx context.Context, request driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	if err := d.Fault.Fail(); err != nil {
		if d.Hang > 0 {
			hung, cancel := context.WithTimeout(ctx, d.Hang)
			defer cancel()
			<-hung.Done()
			return protocol.ModelResponse{}, hung.Err()
		}
		if d.Err != nil {
			return protocol.ModelResponse{}, d.Err
		}
		return protocol.ModelResponse{}, err
	}
	if d.Delegate == nil {
		return protocol.ModelResponse{}, errors.New("testfault: the driver has no delegate")
	}
	response, err := d.Delegate.Generate(ctx, request, emit)
	if err != nil {
		return response, err
	}
	if d.Rewrite != nil {
		d.Rewrite(&response)
	}
	return response, nil
}
