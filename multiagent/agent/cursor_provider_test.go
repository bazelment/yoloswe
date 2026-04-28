package agent

import (
	"context"
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

// TestCursorProvider_RejectsEffort verifies Cursor fails fast when
// cfg.Effort is set, since the cursor SDK has no reasoning-effort knob.
// The check must happen before any subprocess is spawned, so this test
// can run without a working cursor binary.
func TestCursorProvider_RejectsEffort(t *testing.T) {
	t.Parallel()

	for _, level := range []string{"low", "medium", "high", "max", "auto"} {
		level := level
		t.Run(level, func(t *testing.T) {
			t.Parallel()

			p := NewCursorProvider()
			defer p.Close()

			_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(level))
			require.Error(t, err, "cursor should reject effort=%q", level)
			assert.ErrorIs(t, err, ErrEffortUnsupported)
			assert.Contains(t, err.Error(), "cursor")
			assert.Contains(t, err.Error(), level)
		})
	}
}

// TestCursorProvider_RejectsInvalidEffortFirst ensures a typo surfaces
// ErrInvalidEffort rather than the more generic ErrEffortUnsupported,
// so the user sees "you misspelled it" not "wrong provider."
func TestCursorProvider_RejectsInvalidEffortFirst(t *testing.T) {
	t.Parallel()

	p := NewCursorProvider()
	defer p.Close()

	_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort("turbo"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEffort)
	assert.NotErrorIs(t, err, ErrEffortUnsupported)
}

func TestCursorProvider_Close(t *testing.T) {
	p := NewCursorProvider()
	err := p.Close()
	assert.NoError(t, err)

	// Events channel should be closed after Close()
	_, ok := <-p.Events()
	assert.False(t, ok, "events channel should be closed")
}
