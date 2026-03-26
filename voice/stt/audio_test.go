package stt_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/voice/stt"
)

func TestEncodeDecodeWAV_RoundTrip(t *testing.T) {
	t.Parallel()
	original := []int16{0, 1000, -1000, 32767, -32768, 0}
	sampleRate := 16000

	var buf bytes.Buffer
	require.NoError(t, stt.EncodeWAV(&buf, original, sampleRate))

	decoded, sr, err := stt.DecodeWAV(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, sampleRate, sr)
	assert.Equal(t, original, decoded)
}

func TestDecodeWAV_NotRIFF(t *testing.T) {
	t.Parallel()
	_, _, err := stt.DecodeWAV(bytes.NewReader([]byte("NOT_RIFF_DATA_HERE")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a RIFF file")
}

func TestDecodeWAV_EmptyInput(t *testing.T) {
	t.Parallel()
	_, _, err := stt.DecodeWAV(bytes.NewReader(nil))
	require.Error(t, err)
}

func TestDecodeWAV_TruncatedHeader(t *testing.T) {
	t.Parallel()
	_, _, err := stt.DecodeWAV(bytes.NewReader([]byte("RI")))
	require.Error(t, err)
}

func TestGenerateSineWAV(t *testing.T) {
	t.Parallel()
	wav := stt.GenerateSineWAV(440.0, 100*time.Millisecond, 16000)
	require.NotEmpty(t, wav)

	samples, sr, err := stt.DecodeWAV(bytes.NewReader(wav))
	require.NoError(t, err)
	assert.Equal(t, 16000, sr)
	// 100ms at 16kHz = 1600 samples
	assert.Equal(t, 1600, len(samples))

	// Verify it's not silence — at least some samples should be non-zero.
	hasNonZero := false
	for _, s := range samples {
		if s != 0 {
			hasNonZero = true
			break
		}
	}
	assert.True(t, hasNonZero, "sine wave should have non-zero samples")
}

func TestGenerateSilenceWAV(t *testing.T) {
	t.Parallel()
	wav := stt.GenerateSilenceWAV(50*time.Millisecond, 16000)
	require.NotEmpty(t, wav)

	samples, sr, err := stt.DecodeWAV(bytes.NewReader(wav))
	require.NoError(t, err)
	assert.Equal(t, 16000, sr)
	assert.Equal(t, 800, len(samples))

	for i, s := range samples {
		assert.Equal(t, int16(0), s, "sample %d should be zero", i)
	}
}

func TestChunkPCM(t *testing.T) {
	t.Parallel()

	t.Run("evenly divisible", func(t *testing.T) {
		t.Parallel()
		data := make([]byte, 640*3)
		chunks := stt.ChunkPCM(data, 640)
		assert.Len(t, chunks, 3)
		for _, c := range chunks {
			assert.Len(t, c, 640)
		}
	})

	t.Run("remainder", func(t *testing.T) {
		t.Parallel()
		data := make([]byte, 640*2+100)
		chunks := stt.ChunkPCM(data, 640)
		assert.Len(t, chunks, 3)
		assert.Len(t, chunks[0], 640)
		assert.Len(t, chunks[1], 640)
		assert.Len(t, chunks[2], 100)
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		chunks := stt.ChunkPCM(nil, 640)
		assert.Empty(t, chunks)
	})

	t.Run("zero chunk size", func(t *testing.T) {
		t.Parallel()
		chunks := stt.ChunkPCM([]byte{1, 2, 3}, 0)
		assert.Nil(t, chunks)
	})
}

func TestPCMFromWAV(t *testing.T) {
	t.Parallel()
	original := []int16{100, -200, 300}
	var buf bytes.Buffer
	require.NoError(t, stt.EncodeWAV(&buf, original, 16000))

	pcm, err := stt.PCMFromWAV(buf.Bytes())
	require.NoError(t, err)
	assert.Len(t, pcm, 6) // 3 samples * 2 bytes
}

func TestDefaultAudioConfig(t *testing.T) {
	t.Parallel()
	cfg := stt.DefaultAudioConfig()
	assert.Equal(t, 16000, cfg.SampleRate)
	assert.Equal(t, 16, cfg.BitDepth)
	assert.Equal(t, 1, cfg.Channels)
	assert.Equal(t, 20*time.Millisecond, cfg.ChunkSize)
}

func TestAudioConfig_ChunkBytes(t *testing.T) {
	t.Parallel()
	cfg := stt.DefaultAudioConfig()
	// 20ms at 16kHz, 16-bit mono = 320 samples * 2 bytes = 640 bytes
	assert.Equal(t, 640, cfg.ChunkBytes())
}
