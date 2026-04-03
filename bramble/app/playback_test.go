package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	assert.Equal(t, PlaybackModeRedirect, mode)
}

func TestDetectPlaybackMode_SSHClient(t *testing.T) {
	t.Setenv("SSH_CLIENT", "1.2.3.4 5678 22")

	mode := DetectPlaybackMode()
	assert.Equal(t, PlaybackModeRedirect, mode)
}

func TestNewPlaybackHandler_FileMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	handler := NewPlaybackHandler(PlaybackModeFile, dir)

	_, ok := handler.(*FilePlayback)
	assert.True(t, ok)
}

func TestNewPlaybackHandler_DirectMode(t *testing.T) {
	t.Parallel()

	handler := NewPlaybackHandler(PlaybackModeDirect, "")
	dp, ok := handler.(*DirectPlayback)
	assert.True(t, ok)
	if dp != nil {
		dp.Close()
	}
}

func TestNewPlaybackHandler_LocalAliasForDirect(t *testing.T) {
	t.Parallel()

	handler := NewPlaybackHandler(PlaybackModeLocal, "")
	dp, ok := handler.(*DirectPlayback)
	assert.True(t, ok)
	if dp != nil {
		dp.Close()
	}
}

func TestNewPlaybackHandler_RedirectReturnsNil(t *testing.T) {
	t.Parallel()

	handler := NewPlaybackHandler(PlaybackModeRedirect, "")
	assert.Nil(t, handler)
}

func TestNewPlaybackHandler_AutoDetect(t *testing.T) {
	t.Parallel()

	// Auto mode should return some handler without panicking (or nil for redirect).
	handler := NewPlaybackHandler(PlaybackModeAuto, t.TempDir())
	// Clean up if it's a DirectPlayback.
	if dp, ok := handler.(*DirectPlayback); ok {
		dp.Close()
	}
}

func TestDirectPlayback_UnsupportedFormat(t *testing.T) {
	t.Parallel()

	dp := NewDirectPlayback()
	defer dp.Close()

	_, err := dp.Play(context.Background(), []byte("data"), "wav")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only supports mp3")
}

func TestRedirectTextWriter_WritesFiles(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "redirect")
	r := &RedirectTextWriter{Dir: dir}

	err := r.WriteText("test report one")
	require.NoError(t, err)

	// Verify latest file.
	latest, err := os.ReadFile(filepath.Join(dir, "voice-report-latest.txt"))
	require.NoError(t, err)
	assert.Equal(t, "test report one\n", string(latest))

	// Verify log file contains the entry.
	logData, err := os.ReadFile(filepath.Join(dir, "voice-report-log.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(logData), "test report one")

	// Write a second report and verify latest is overwritten.
	err = r.WriteText("test report two")
	require.NoError(t, err)

	latest, err = os.ReadFile(filepath.Join(dir, "voice-report-latest.txt"))
	require.NoError(t, err)
	assert.Equal(t, "test report two\n", string(latest))

	// Log file should contain both entries.
	logData, err = os.ReadFile(filepath.Join(dir, "voice-report-log.txt"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	assert.Len(t, lines, 2)
}

func TestRedirectTextWriter_CreatesDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "nested", "deep", "redirect")
	r := &RedirectTextWriter{Dir: dir}

	err := r.WriteText("hello")
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}
