// Command voicetest is a standalone CLI for testing speech-to-text transcription.
//
// Usage:
//
//	voicetest [--provider deepgram] [--file input.wav] [--log output.jsonl]
//
// Without --file, it captures audio from the default microphone.
// With --file, it streams the WAV file as if it were live audio.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/bazelment/yoloswe/logging/klogfmt"
	"github.com/bazelment/yoloswe/voice/stt"
	"github.com/bazelment/yoloswe/voice/stt/deepgram"
	"github.com/bazelment/yoloswe/voice/stt/logging"
)

func main() {
	klogfmt.Init()
	provider := flag.String("provider", "deepgram", "STT provider (deepgram)")
	file := flag.String("file", "", "WAV file to transcribe (omit for live mic)")
	logFile := flag.String("log", "", "JSONL log file path")
	flag.Parse()

	if err := run(*provider, *file, *logFile); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(providerName, file, logPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Create provider.
	var streamer stt.StreamingTranscriber
	switch providerName {
	case "deepgram":
		apiKey := os.Getenv("DEEPGRAM_API_KEY")
		streamer = deepgram.NewStreamingProvider(apiKey)
	default:
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	// Optional JSONL logger.
	var logger *logging.Logger
	if logPath != "" {
		f, err := os.Create(logPath)
		if err != nil {
			return fmt.Errorf("create log file: %w", err)
		}
		defer f.Close()
		logger = logging.New(f, time.Now())
		logger.LogSessionStart(providerName)
		defer logger.LogSessionEnd(providerName)
	}

	cfg := stt.DefaultAudioConfig()
	opts := stt.StreamOpts{
		VAD:     true,
		Interim: true,
		Model:   "nova-3",
	}

	sess, err := streamer.StartSession(ctx, cfg, opts)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	defer sess.Close()

	// Start audio source.
	if file != "" {
		errCh := make(chan error, 1)
		go func() {
			errCh <- streamFile(ctx, sess, file, cfg)
		}()
		// Print events, and stop if the audio goroutine exits with an error.
		fmt.Fprintln(os.Stderr, "Listening for transcription events... (Ctrl+C to stop)")
		for {
			select {
			case fileErr := <-errCh:
				if fileErr != nil {
					return fileErr
				}
				// Drain remaining events until channel closes.
				for evt := range sess.Events() {
					printEvent(evt)
					if logger != nil {
						logger.LogEvent(providerName, evt)
					}
				}
				return nil
			case evt, ok := <-sess.Events():
				if !ok {
					return nil
				}
				printEvent(evt)
				if logger != nil {
					logger.LogEvent(providerName, evt)
				}
			case <-ctx.Done():
				return nil
			}
		}
	}

	fmt.Fprintln(os.Stderr, "Live mic capture not yet implemented. Use --file to stream a WAV file.")
	return fmt.Errorf("mic capture requires Phase 1 audio library spike")
}

func streamFile(ctx context.Context, sess stt.Session, path string, cfg stt.AudioConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	pcm, err := stt.PCMFromWAV(data)
	if err != nil {
		return fmt.Errorf("decode WAV: %w", err)
	}

	chunkSize := cfg.ChunkBytes()
	chunks := stt.ChunkPCM(pcm, chunkSize)

	// Stream chunks at real-time pace.
	ticker := time.NewTicker(cfg.ChunkSize)
	defer ticker.Stop()

	for _, chunk := range chunks {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := sess.SendAudio(chunk); err != nil {
				return fmt.Errorf("send audio: %w", err)
			}
		}
	}

	// Signal end of audio — give the provider time to finalize.
	time.Sleep(2 * time.Second)
	sess.Close()
	return nil
}

func printEvent(evt stt.Event) {
	switch evt.Type {
	case stt.EventSpeechStart:
		fmt.Println("[speech-start]")
	case stt.EventPartialText:
		fmt.Printf("[partial] %q\n", evt.Text)
	case stt.EventFinalText:
		fmt.Printf("[final] %q (confidence: %.2f)\n", evt.Text, evt.Confidence)
	case stt.EventSpeechEnd:
		fmt.Println("[speech-end]")
	case stt.EventError:
		fmt.Fprintf(os.Stderr, "[error] %v\n", evt.Error)
	}
}
