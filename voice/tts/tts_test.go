package tts

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTextToSpeechContract(t *testing.T) {
	t.Parallel()

	provider := fakeTTS{}
	audio, err := provider.Synthesize(context.Background(), "hello", SynthOpts{
		Voice:    "voice-1",
		Language: "en-US",
		Speed:    1.25,
	})
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if string(audio.Data) != "voice-1|en-US|1.25|hello" {
		t.Fatalf("audio data = %q", audio.Data)
	}
	if audio.Format != AudioFormatMP3 || audio.Duration != time.Second {
		t.Fatalf("audio metadata = %+v", audio)
	}
}

func TestAudioFormatConstants(t *testing.T) {
	t.Parallel()

	formats := map[string]string{
		AudioFormatMP3: "mp3",
		AudioFormatWAV: "wav",
		AudioFormatOGG: "ogg",
		AudioFormatPCM: "pcm",
	}
	for got, want := range formats {
		if got != want {
			t.Fatalf("audio format = %q, want %q", got, want)
		}
	}
}

type fakeTTS struct{}

func (fakeTTS) Synthesize(_ context.Context, text string, opts SynthOpts) (*Audio, error) {
	return &Audio{
		Data:     []byte(fmt.Sprintf("%s|%s|%.2f|%s", opts.Voice, opts.Language, opts.Speed, text)),
		Duration: time.Second,
		Format:   AudioFormatMP3,
	}, nil
}

var _ TextToSpeech = fakeTTS{}
