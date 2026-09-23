package codereview

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

// phaseEvent is a machine-parsable stdout progress event.
type phaseEvent struct {
	Event string `json:"event"`
	Phase string `json:"phase"`
}

// terminalEvent is emitted after the envelope is ready.
type terminalEvent struct {
	Envelope string `json:"envelope"`
	Event    string `json:"event"`
	Error    string `json:"error,omitempty"`
	Resume   string `json:"resume,omitempty"`
	Status   string `json:"status"`
	Verdict  string `json:"verdict"`
	Issues   int    `json:"issues"`
}

func emitPhase(phase string) {
	writeStdoutJSON(phaseEvent{Event: "phase", Phase: phase})
}

// heartbeatInterval is how often startHeartbeats publishes. review_push.py
// treats 2x interval_ms of silence as a hang. Overridable by tests.
var heartbeatInterval = 20 * time.Second

// heartbeatEvent is the push-stream liveness event.
type heartbeatEvent struct {
	Event      string `json:"event"`
	ElapsedMs  int64  `json:"elapsed_ms"`
	IntervalMs int64  `json:"interval_ms"`
}

// startHeartbeats publishes a heartbeat every heartbeatInterval until the
// returned stop is called. It runs in this process and does not depend on the
// backend. Backend start, a resume fallback that respawns the CLI, and
// print-mode backends that stream nothing all get heartbeats, so the
// watchdog's hang clock is never running while nothing is emitting.
// Heartbeats show that bramble is alive. They do not show that the backend is
// making progress. A stalled backend is killed by --idle-timeout, and that
// path still produces a terminal event.
//
// stop is idempotent. It waits for the ticker goroutine to exit. A heartbeat
// that is mid-write when stop is called completes before stop returns, and
// no heartbeat is written after that. Call it before writing_envelope so the
// terminal event is the stream's last line.
func startHeartbeats() (stop func()) {
	interval := heartbeatInterval
	start := time.Now()
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				writeStdoutJSON(heartbeatEvent{
					Event:      "heartbeat",
					ElapsedMs:  time.Since(start).Milliseconds(),
					IntervalMs: interval.Milliseconds(),
				})
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-exited
		})
	}
}

// Partial reviews emit done because their envelope contains findings.
func emitTerminal(env reviewer.ResultEnvelope, envelopePath string) {
	kind := "done"
	if env.Status == reviewer.StatusError || env.Status == "" {
		kind = "error"
	}
	ev := terminalEvent{
		Event:    kind,
		Verdict:  env.Review.Verdict,
		Envelope: envelopePath,
		Status:   string(env.Status),
		Issues:   len(env.Review.Issues),
		Error:    env.Error,
		Resume:   string(env.ResumeStatus),
	}
	writeStdoutJSON(ev)
}

func writeStdoutJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("marshal progress event", "error", err.Error())
		return
	}
	_, _ = os.Stdout.Write(append(b, '\n'))
}

// writeEnvelopeAtomic publishes an envelope only after its complete write.
func writeEnvelopeAtomic(path string, env reviewer.ResultEnvelope) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create envelope dir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".envelope-*.tmp")
	if err != nil {
		return fmt.Errorf("create envelope temp: %w", err)
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if err := reviewer.PrintJSONResult(f, env); err != nil {
		_ = f.Close()
		return fmt.Errorf("write envelope temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close envelope temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename envelope: %w", err)
	}
	cleanup = false
	return nil
}

// deliverEnvelope publishes the envelope before its terminal event.
func deliverEnvelope(env reviewer.ResultEnvelope) (wrote bool, err error) {
	emitPhase("writing_envelope")
	if envelopeFile == "" {
		if printErr := reviewer.PrintJSONResult(os.Stdout, env); printErr != nil {
			reportEnvelopePrintError(printErr)
			emitTerminal(env, "")
			return false, fmt.Errorf("failed to write JSON envelope: %w", printErr)
		}
		emitTerminal(env, "")
		return true, nil
	}
	if writeErr := writeEnvelopeAtomic(envelopeFile, env); writeErr != nil {
		// The file path is unwritable. Dump the envelope onto the push
		// stream so the terminal event is not the only copy, then say the
		// path is empty — there is no ready file to read.
		slog.Error("failed to write envelope-file; falling back to stdout", "error", writeErr.Error())
		if printErr := reviewer.PrintJSONResult(os.Stdout, env); printErr != nil {
			reportEnvelopePrintError(printErr)
			emitTerminal(env, "")
			return false, fmt.Errorf("failed to write envelope-file: %w", writeErr)
		}
		emitTerminal(env, "")
		return true, fmt.Errorf("failed to write envelope-file: %w", writeErr)
	}
	emitTerminal(env, envelopeFile)
	return true, nil
}
