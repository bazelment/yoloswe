package codereview

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

// phaseEvent is one machine-parsable progress line on stdout. Monitor streams
// stdout, so these lines are the push notifications. There is no progress
// file to tail: issue 247's multi-hour rounds were the orchestrator re-reading
// a tee of stderr while a completed review sat unread.
type phaseEvent struct {
	Event string `json:"event"`
	Phase string `json:"phase"`
}

// terminalEvent is the last stdout line of a review. It is the readiness
// signal: verdict and envelope path ride on it, and the envelope file is
// already renamed into place before this is written. An orchestrator that
// stats the envelope to see if the review is done is polling.
type terminalEvent struct {
	Envelope string `json:"envelope"`
	Event    string `json:"event"`
	Error    string `json:"error,omitempty"`
	Resume   string `json:"resume,omitempty"`
	Status   string `json:"status"`
	Verdict  string `json:"verdict"`
	Issues   int    `json:"issues"`
}

// emitPhase writes one phase line (reading_diff, analyzing, writing_envelope)
// to stdout and returns. The write is the notification.
func emitPhase(phase string) {
	writeStdoutJSON(phaseEvent{Event: "phase", Phase: phase})
}

// emitTerminal writes the done/error line. Partial reviews use done: the
// envelope is ready and still holds findings. status=error is the only error
// event — a partial reported as "error:" is what made an orchestrator record
// "no envelope" while findings were on disk (kernel#8682 r1).
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

// writeEnvelopeAtomic publishes env at path by writing a temp file in the
// same directory and renaming it over the destination. O_TRUNC on the
// destination made a partial envelope observable mid-write; a stat of that
// path then looked like readiness. Rename is the publication, and the
// terminal event is what the orchestrator waits for — the path is not a
// progress channel.
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

// deliverEnvelope writes the envelope (stdout, or --envelope-file via atomic
// rename) and then the terminal event. The terminal line is always last, so
// a reader that treats "the last stdout line" as readiness cannot observe a
// half-written file. wrote reports whether a complete envelope was flushed;
// the caller uses it to suppress a second synthesized envelope.
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
