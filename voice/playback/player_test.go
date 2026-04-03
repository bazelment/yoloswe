package playback

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlayMP3_EmptyData(t *testing.T) {
	t.Parallel()

	p := NewPlayer()
	defer p.Close()

	err := p.PlayMP3(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty audio data")
}

func TestPlayMP3_EmptySlice(t *testing.T) {
	t.Parallel()

	p := NewPlayer()
	defer p.Close()

	err := p.PlayMP3(context.Background(), []byte{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty audio data")
}

func TestNewPlayer(t *testing.T) {
	t.Parallel()

	p := NewPlayer()
	require.NotNil(t, p)
	assert.NoError(t, p.Close())
}

func TestPlayer_CloseMultipleTimes(t *testing.T) {
	t.Parallel()

	p := NewPlayer()
	assert.NoError(t, p.Close())
	assert.NoError(t, p.Close())
}
