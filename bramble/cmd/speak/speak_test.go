package speak

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/voice/tts"
)

func TestRunSpeakWithDepsSkipsBlankInput(t *testing.T) {
	t.Parallel()

	providerCalled := false
	playerCalled := false
	deps := speakDeps{
		input: strings.NewReader(" \n\t "),
		newProvider: func(string) (tts.TextToSpeech, error) {
			providerCalled = true
			return &recordingTTS{}, nil
		},
		newPlayer: func() audioPlayer {
			playerCalled = true
			return &recordingPlayer{}
		},
	}

	err := runSpeakWithDeps(context.Background(), speakConfig{timeout: time.Second}, deps)

	require.NoError(t, err)
	require.False(t, providerCalled)
	require.False(t, playerCalled)
}

func TestRunSpeakWithDepsSynthesizesTrimmedInputAndPlaysAudio(t *testing.T) {
	t.Parallel()

	synth := &recordingTTS{
		audio: &tts.Audio{
			Data:   []byte("mp3-data"),
			Format: tts.AudioFormatMP3,
		},
	}
	player := &recordingPlayer{}
	deps := speakDeps{
		input: strings.NewReader("\n hello remote \t\n"),
		newProvider: func(apiKey string) (tts.TextToSpeech, error) {
			require.Equal(t, "api-key", apiKey)
			return synth, nil
		},
		newPlayer: func() audioPlayer {
			return player
		},
	}
	cfg := speakConfig{
		apiKey:  "api-key",
		voice:   "voice-id",
		timeout: time.Second,
		speed:   1.25,
	}

	err := runSpeakWithDeps(context.Background(), cfg, deps)

	require.NoError(t, err)
	require.Equal(t, "hello remote", synth.text)
	require.Equal(t, tts.SynthOpts{Voice: "voice-id", Speed: 1.25}, synth.opts)
	require.Equal(t, []byte("mp3-data"), player.data)
	require.True(t, player.closed)
}

func TestRunSpeakWithDepsReturnsInputError(t *testing.T) {
	t.Parallel()

	readErr := errors.New("read failed")
	deps := speakDeps{
		input: errReader{err: readErr},
		newProvider: func(string) (tts.TextToSpeech, error) {
			t.Fatal("provider should not be created after input failure")
			return nil, nil
		},
		newPlayer: func() audioPlayer {
			t.Fatal("player should not be created after input failure")
			return nil
		},
	}

	err := runSpeakWithDeps(context.Background(), speakConfig{timeout: time.Second}, deps)

	require.ErrorIs(t, err, readErr)
	require.ErrorContains(t, err, "read stdin")
}

type recordingTTS struct {
	audio *tts.Audio
	err   error
	text  string
	opts  tts.SynthOpts
}

func (r *recordingTTS) Synthesize(_ context.Context, text string, opts tts.SynthOpts) (*tts.Audio, error) {
	r.text = text
	r.opts = opts
	return r.audio, r.err
}

type recordingPlayer struct {
	data   []byte
	closed bool
}

func (r *recordingPlayer) PlayMP3(_ context.Context, data []byte) error {
	r.data = append([]byte(nil), data...)
	return nil
}

func (r *recordingPlayer) Close() error {
	r.closed = true
	return nil
}

type errReader struct {
	err error
}

func (r errReader) Read([]byte) (int, error) {
	return 0, r.err
}

var _ io.Reader = errReader{}
