// Package remote provides the gRPC server, client proxies, and event
// broadcasting for Bramble's remote execution mode.
package remote

import (
	"context"
	"log"
	"sync"
)

// EventBroadcaster fans out events from a single source channel to multiple
// subscriber channels. Each subscriber has its own buffered channel; if a
// subscriber falls behind, the oldest event is dropped.
type EventBroadcaster struct {
	subscribers map[int]chan interface{}
	mu          sync.RWMutex
	nextID      int
}

// NewEventBroadcaster creates a new broadcaster.
func NewEventBroadcaster() *EventBroadcaster {
	return &EventBroadcaster{
		subscribers: make(map[int]chan interface{}),
	}
}

// Subscribe creates a new subscriber channel with the given buffer size.
// Returns the subscriber ID (for Unsubscribe) and the read-only channel.
func (b *EventBroadcaster) Subscribe(bufSize int) (int, <-chan interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextID
	b.nextID++

	ch := make(chan interface{}, bufSize)
	b.subscribers[id] = ch
	return id, ch
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *EventBroadcaster) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ch, ok := b.subscribers[id]; ok {
		delete(b.subscribers, id)
		close(ch)
	}
}

// Run reads from source and fans out each event to all subscribers.
// It blocks until source is closed or ctx is cancelled.
func (b *EventBroadcaster) Run(ctx context.Context, source <-chan interface{}) {
	for {
		select {
		case <-ctx.Done():
			b.closeAll()
			return
		case event, ok := <-source:
			if !ok {
				b.closeAll()
				return
			}
			b.broadcast(event)
		}
	}
}

// broadcast sends an event to all current subscribers.
// If a subscriber's channel is full, the oldest event is dropped.
func (b *EventBroadcaster) broadcast(event interface{}) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for id, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			// Channel full — drop oldest then send
			select {
			case <-ch:
				log.Printf("WARNING: broadcaster dropping oldest event for subscriber %d (channel full)", id)
			default:
			}
			select {
			case ch <- event:
			default:
				log.Printf("WARNING: broadcaster could not deliver event to subscriber %d", id)
			}
		}
	}
}

// closeAll closes all subscriber channels. Called when the source is done.
func (b *EventBroadcaster) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for id, ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, id)
	}
}
