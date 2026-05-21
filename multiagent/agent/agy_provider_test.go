package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgyProvider_Name(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	assert.Equal(t, ProviderAgy, p.Name())
}

func TestAgyProvider_EventsChannel(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	ch := p.Events()
	require.NotNil(t, ch)
	assert.Equal(t, (<-chan AgentEvent)(p.events), ch)
}

func TestAgyProvider_RejectsExplicitEffort(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(EffortHigh))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEffortUnsupported)
	assert.Contains(t, err.Error(), ProviderAgy)
}
