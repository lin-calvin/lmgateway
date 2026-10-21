// Package hook is a minimal in-process callback registry. A producer Emits a
// value and every registered callback is invoked synchronously, each isolated so
// a panicking subscriber cannot break the producer. Subscribers must be
// non-blocking (buffer/offload heavy work themselves, e.g. via webhook.Sender).
package hook

import (
	"log"
	"sync"
)

// Registry fans a value out to registered callbacks.
type Registry[T any] struct {
	mu   sync.RWMutex
	subs []func(T)
}

// New returns an empty registry.
func New[T any]() *Registry[T] { return &Registry[T]{} }

// Register adds a callback. nil is ignored.
func (r *Registry[T]) Register(fn func(T)) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.subs = append(r.subs, fn)
	r.mu.Unlock()
}

// Emit invokes every callback with v, in registration order. It is synchronous:
// callers must not register blocking subscribers.
func (r *Registry[T]) Emit(v T) {
	r.mu.RLock()
	subs := r.subs
	r.mu.RUnlock()
	for _, fn := range subs {
		func() {
			defer func() {
				if p := recover(); p != nil {
					log.Printf("[hook] subscriber panic: %v", p)
				}
			}()
			fn(v)
		}()
	}
}

// Len reports the number of registered callbacks.
func (r *Registry[T]) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}
