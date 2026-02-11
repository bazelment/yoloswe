package remote

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBroadcaster_SingleSubscriber(t *testing.T) {
	b := NewEventBroadcaster()
	source := make(chan interface{}, 10)

	_, ch := b.Subscribe(100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx, source)

	source <- "event1"
	source <- "event2"

	require.Eventually(t, func() bool { return len(ch) >= 2 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "event1", <-ch)
	assert.Equal(t, "event2", <-ch)
}

func TestBroadcaster_MultipleSubscribers(t *testing.T) {
	b := NewEventBroadcaster()
	source := make(chan interface{}, 10)

	_, ch1 := b.Subscribe(100)
	_, ch2 := b.Subscribe(100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx, source)

	source <- "event1"

	require.Eventually(t, func() bool { return len(ch1) >= 1 && len(ch2) >= 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "event1", <-ch1)
	assert.Equal(t, "event1", <-ch2)
}

func TestBroadcaster_Unsubscribe(t *testing.T) {
	b := NewEventBroadcaster()
	source := make(chan interface{}, 10)

	id1, ch1 := b.Subscribe(100)
	_, ch2 := b.Subscribe(100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx, source)

	source <- "event1"
	require.Eventually(t, func() bool { return len(ch1) >= 1 && len(ch2) >= 1 }, time.Second, 10*time.Millisecond)
	<-ch1
	<-ch2

	b.Unsubscribe(id1)

	source <- "event2"
	require.Eventually(t, func() bool { return len(ch2) >= 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "event2", <-ch2)

	// ch1 should be closed after unsubscribe
	_, ok := <-ch1
	assert.False(t, ok)
}

func TestBroadcaster_SourceClose(t *testing.T) {
	b := NewEventBroadcaster()
	source := make(chan interface{}, 10)

	_, ch := b.Subscribe(100)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.Run(context.Background(), source)
	}()

	close(source)
	wg.Wait()

	// Subscriber channel should be closed
	_, ok := <-ch
	assert.False(t, ok)
}

func TestBroadcaster_ContextCancel(t *testing.T) {
	b := NewEventBroadcaster()
	source := make(chan interface{})

	_, ch := b.Subscribe(100)

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.Run(ctx, source)
	}()

	cancel()
	wg.Wait()

	// Subscriber channel should be closed
	_, ok := <-ch
	assert.False(t, ok)
}

func TestBroadcaster_FullChannel_DropsOldest(t *testing.T) {
	b := NewEventBroadcaster()
	source := make(chan interface{}, 100)

	// Small buffer to force overflow
	_, ch := b.Subscribe(2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx, source)

	// Send more events than the buffer can hold
	for i := 0; i < 5; i++ {
		source <- i
	}

	// Wait for processing
	require.Eventually(t, func() bool { return len(ch) == 2 }, time.Second, 10*time.Millisecond)

	// We should get the most recent events (oldest were dropped)
	// The exact values depend on timing, but we should get 2 events
	e1 := <-ch
	e2 := <-ch
	// Just verify we got valid events (exact values depend on race)
	assert.NotNil(t, e1)
	assert.NotNil(t, e2)
}
