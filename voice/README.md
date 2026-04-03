# Voice

Speech-to-text and text-to-speech packages with provider-agnostic interfaces. Currently implements Deepgram's WebSocket API for real-time transcription and ElevenLabs HTTP API for speech synthesis.

## Packages

| Package | Purpose |
|---------|---------|
| `stt` | Core STT interfaces (`StreamingTranscriber`, `Session`) and types (`Event`, `AudioConfig`) |
| `stt/deepgram` | Deepgram WebSocket streaming implementation |
| `stt/logging` | JSONL event logger for session analysis and latency measurement |
| `tts` | Core TTS interface (`TextToSpeech`) and types (`Audio`, `SynthOpts`) |
| `tts/elevenlabs` | ElevenLabs HTTP API implementation |
| `cmd/voicetest` | CLI tool for testing transcription end-to-end |

## How It Works

```
Audio source (WAV file or mic)
        |
  ChunkPCM() splits into 20ms frames
        |
        v
  ┌─────────────────┐
  │ Session          │
  │  SendAudio()  ───┼──► WebSocket ──► Deepgram API
  │  Events()     ◄──┼──  speech-start, partial, final, speech-end
  │  Close()         │
  └─────────────────┘
        |
        v
  Event channel: EventSpeechStart → EventPartialText → EventFinalText → EventSpeechEnd
```

1. Create a `StreamingTranscriber` (e.g., `deepgram.NewStreamingProvider`)
2. Call `StartSession` with audio config and options (VAD, interim results, language)
3. Send audio chunks via `SendAudio`
4. Consume typed events from `Events()` channel
5. Call `Close` when done

## Event Types

| Type | Meaning |
|------|---------|
| `EventSpeechStart` | Voice activity detected |
| `EventPartialText` | Interim transcription (still being refined) |
| `EventFinalText` | Confirmed final transcription |
| `EventSpeechEnd` | Voice activity ended |
| `EventError` | Transcription error |

## Audio Utilities

The `stt` package includes WAV encoding/decoding and PCM helpers:

- `EncodeWAV` / `DecodeWAV` — read and write 16-bit mono PCM WAV files
- `GenerateSineWAV` / `GenerateSilenceWAV` — generate test audio
- `ChunkPCM` — split raw PCM into fixed-size chunks for streaming
- `PCMFromWAV` — extract raw PCM bytes from WAV data

Default audio config: 16kHz sample rate, 16-bit depth, mono, 20ms chunks.

## voicetest CLI

```bash
# Build
bazel build //voice/cmd/voicetest

# Stream a WAV file through Deepgram
DEEPGRAM_API_KEY=... voicetest --file input.wav --log output.jsonl

# Flags
#   --provider deepgram   STT provider (default: deepgram)
#   --file input.wav      WAV file to stream (real-time paced)
#   --log output.jsonl    Optional JSONL log for session analysis
```

Output:
```
[speech-start]
[partial] "hello"
[partial] "hello world"
[final] "hello world" (confidence: 0.95)
[speech-end]
```

## Text-to-Speech (TTS)

The `tts` package provides a provider-agnostic interface for speech synthesis:

```go
provider, err := elevenlabs.NewProvider("") // reads ELEVENLABS_API_KEY from env
audio, err := provider.Synthesize(ctx, "Hello world", tts.SynthOpts{
    Voice: "custom-voice-id",  // optional, uses default voice if empty
    Speed: 1.0,                // speech rate multiplier
})
// audio.Data contains MP3 bytes, audio.Format is "mp3"
```

### Adding a new TTS provider

1. Create a new package under `tts/` (e.g., `tts/google/`)
2. Implement the `tts.TextToSpeech` interface
3. The `Synthesize` method should return `*tts.Audio` with the audio bytes and format

### Voice Reporting in Bramble

When `--enable-voice-reports` is set, bramble synthesizes a spoken summary whenever a session completes, fails, or is stopped. Configuration:

- `--enable-voice-reports` — enable voice reports (default: false)
- `--elevenlabs-api-key` — API key (or set `ELEVENLABS_API_KEY` env var)
- `--tts-voice` — ElevenLabs voice ID
- `--voice-report-mode` — `local` (system audio player), `file` (save to `~/.bramble/voice-reports/`), or `auto` (detect)

For SSH sessions, use `--voice-report-mode file` to save reports locally for later playback.

## Thread Safety

`SendAudio` is safe to call concurrently with `Events()`. `SendAudio` and `Close` must not be called concurrently with each other. Cancelling the context passed to `StartSession` closes the connection.
