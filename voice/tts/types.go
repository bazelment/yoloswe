package tts

import "time"

// Audio format constants.
const (
	AudioFormatMP3 = "mp3"
	AudioFormatWAV = "wav"
	AudioFormatOGG = "ogg"
	AudioFormatPCM = "pcm"
)

// Audio is the result of a synthesis request.
type Audio struct { //nolint:govet // fieldalignment: readability over packing
	// Data is the raw audio bytes.
	Data []byte
	// Duration is the approximate duration of the audio.
	Duration time.Duration
	// Format is the audio format (e.g. "mp3", "wav").
	Format string
}
