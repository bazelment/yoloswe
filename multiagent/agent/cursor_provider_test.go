package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorProvider_Name(t *testing.T) {
	p := NewCursorProvider()
	assert.Equal(t, "cursor", p.Name())
}

func TestCursorProvider_EventsChannel(t *testing.T) {
	p := NewCursorProvider()
	ch := p.Events()
	require.NotNil(t, ch)
	// Should be the same channel each time
	assert.Equal(t, (<-chan AgentEvent)(p.events), ch)
}

func TestCursorProvider_Close(t *testing.T) {
	p := NewCursorProvider()
	err := p.Close()
	assert.NoError(t, err)

	// Events channel should be closed after Close()
	_, ok := <-p.Events()
	assert.False(t, ok, "events channel should be closed")
}
