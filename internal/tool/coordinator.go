package tool

import (
	"context"
	"sync"
)

// ResourceCoordinator coordinates conflicting work across executors that
// belong to the same session.
type ResourceCoordinator struct {
	mu      sync.Mutex
	nextID  uint64
	waiters []*resourceWaiter
	active  map[uint64]ConcurrencySpec
	changed chan struct{}
}

type resourceWaiter struct {
	id   uint64
	spec ConcurrencySpec
}

func NewResourceCoordinator() *ResourceCoordinator {
	return &ResourceCoordinator{
		active:  make(map[uint64]ConcurrencySpec),
		changed: make(chan struct{}),
	}
}

// Acquire waits for the resource claims and returns an idempotent release
// function. Earlier conflicting waiters retain priority.
func (c *ResourceCoordinator) Acquire(ctx context.Context, spec ConcurrencySpec) (func(), error) {
	if c == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	spec = normalizeConcurrencySpec(spec)

	c.mu.Lock()
	c.nextID++
	waiter := &resourceWaiter{id: c.nextID, spec: spec}
	c.waiters = append(c.waiters, waiter)
	for {
		index := c.waiterIndexLocked(waiter.id)
		if index >= 0 && c.canAcquireLocked(index) {
			c.waiters = append(c.waiters[:index], c.waiters[index+1:]...)
			c.active[waiter.id] = waiter.spec
			c.signalLocked()
			c.mu.Unlock()

			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					delete(c.active, waiter.id)
					c.signalLocked()
					c.mu.Unlock()
				})
			}, nil
		}

		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			c.mu.Lock()
			if index := c.waiterIndexLocked(waiter.id); index >= 0 {
				c.waiters = append(c.waiters[:index], c.waiters[index+1:]...)
				c.signalLocked()
			}
			c.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
			c.mu.Lock()
		}
	}
}

func (c *ResourceCoordinator) canAcquireLocked(index int) bool {
	waiter := c.waiters[index]
	for _, active := range c.active {
		if concurrencyConflicts(waiter.spec, active) {
			return false
		}
	}
	for i := 0; i < index; i++ {
		if concurrencyConflicts(waiter.spec, c.waiters[i].spec) {
			return false
		}
	}
	return true
}

func (c *ResourceCoordinator) waiterIndexLocked(id uint64) int {
	for i, waiter := range c.waiters {
		if waiter.id == id {
			return i
		}
	}
	return -1
}

func (c *ResourceCoordinator) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}
