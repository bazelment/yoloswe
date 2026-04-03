package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilePlayback_SavesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fp := &FilePlayback{Dir: dir}
	data := []byte("fake-audio-data")

	result, err := fp.Play(context.Background(), data, "mp3")
	require.NoError(t, err)
	require.NotEmpty(t, result.FilePath)

	// Verify file exists with correct content.
	content, err := os.ReadFile(result.FilePath)
	require.NoError(t, err)
	assert.Equal(t, data, content)

	// Verify file extension.
	assert.Equal(t, ".mp3", filepath.Ext(result.FilePath))
}

func TestFilePlayback_CreatesDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "nested", "voice-reports")
	fp := &FilePlayback{Dir: dir}

	result, err := fp.Play(context.Background(), []byte("audio"), "wav")
	require.NoError(t, err)
	require.NotEmpty(t, result.FilePath)

	// Verify directory was created.
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestDetectPlaybackMode_SSH(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "1.2.3.4 5678 9.10.11.12 22")

	mode := DetectPlaybackMode()
	assert.Equal(t, PlaybackModeFile, mode)
}

func TestNewPlaybackHandler_FileMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	handler := NewPlaybackHandler(PlaybackModeFile, dir)

	_, ok := handler.(*FilePlayback)
	assert.True(t, ok)
}

func TestNewPlaybackHandler_AutoDetect(t *testing.T) {
	t.Parallel()

	// Auto mode should return some handler without panicking.
	handler := NewPlaybackHandler(PlaybackModeAuto, t.TempDir())
	assert.NotNil(t, handler)
}
