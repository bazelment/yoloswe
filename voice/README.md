# Voice

Speech-to-text package with a provider-agnostic streaming interface. Currently implements Deepgram's WebSocket API for real-time transcription with voice activity detection.

## Packages

| Package | Purpose |
|---------|---------|
| `stt` | Core interfaces (`StreamingTranscriber`, `Session`) and types (`Event`, `AudioConfig`) |
| `stt/deepgram` | Deepgram WebSocket streaming implementation |
| `stt/logging` | JSONL event logger for session analysis and latency measurement |
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

## Thread Safety

`SendAudio` is safe to call concurrently with `Events()`. `SendAudio` and `Close` must not be called concurrently with each other. Cancelling the context passed to `StartSession` closes the connection.
