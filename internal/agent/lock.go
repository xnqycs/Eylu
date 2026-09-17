package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrConversationBusy reports that another request is already running on this
// conversation.
//
// A conversation has exactly one writer: the running request owns every commit to
// its transcript, driver state and ledger. A second request is refused with this
// error instead of interleaving with the first.
var ErrConversationBusy = errors.New("conversation already has a running request")

// ownershipGrace bounds how long a session-level operation waits for a running
// request before it cancels it.
const ownershipGrace = 5 * time.Second

// runGate enforces the single-writer invariant of one Conversation.
//
// It is deliberately separate from the state mutex: the state mutex is only held
// for short reads and commits, while the gate is held for the whole request so a
// rotation, a restore or a second request cannot interleave with it.
type runGate struct {
	mu     sync.Mutex
	active bool
	done   chan struct{}
	cancel context.CancelFunc
}

// begin claims the writer slot. It never blocks: a caller that finds the
// conversation busy is told so.
func (g *runGate) begin() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active {
		return ErrConversationBusy
	}
	g.active = true
	g.done = make(chan struct{})
	return nil
}

// attach registers the cancel function of the active request, so a session-level
// operation can stop it when it overruns the grace period.
func (g *runGate) attach(cancel context.CancelFunc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active {
		g.cancel = cancel
	}
}

// release ends the active request and wakes every waiter.
func (g *runGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finishLocked()
}

func (g *runGate) finishLocked() {
	if g.done != nil {
		close(g.done)
		g.done = nil
	}
	g.active = false
	g.cancel = nil
}

// idle reports whether no request is running.
func (g *runGate) idle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.active
}

// acquire claims the writer slot for an operation that changes the conversation as
// a whole, waiting for the current owner to finish first.
//
// Waiting and claiming are one protocol, and that is the whole point: releasing
// the wait and taking the slot happen under the same lock, so no request can start
// in the gap. Observing "no request is running" and then acting on it is not the
// same as holding the conversation, and the difference is a request that commits
// its turn into a session the rotation is about to replace.
//
// A request that does not finish within the grace period is cancelled first, so a
// whole-conversation operation can never wait forever on a model or a tool that
// will not return.
func (g *runGate) acquire() {
	for {
		g.mu.Lock()
		if !g.active {
			g.active = true
			g.done = make(chan struct{})
			g.cancel = nil
			g.mu.Unlock()
			return
		}
		done := g.done
		g.mu.Unlock()
		select {
		case <-done:
		case <-time.After(ownershipGrace):
			g.mu.Lock()
			cancel := g.cancel
			g.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			<-done
		}
	}
}
