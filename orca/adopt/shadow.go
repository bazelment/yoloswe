package adopt

// Shadow mode records the effects the adopted controller and dispatcher would
// send to a live run. The wrappers deliberately implement the same interfaces
// as the real transports, but never call their mutating methods.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

const maxShadowArgumentBytes = 2048

var errShadowRecorderUnavailable = errors.New("shadow recorder is unavailable")

// Action is one intercepted side effect.
//
//nolint:govet // JSONL shape is intentionally grouped by report semantics.
type Action struct {
	At         time.Time       `json:"time"`
	Lane       string          `json:"lane"`
	Kind       string          `json:"action"`
	Arguments  json.RawMessage `json:"arguments"`
	DecisionID string          `json:"decision_id"`
	EventID    string          `json:"event_id"`
	Reason     string          `json:"reason"`
}

// ActionRecorder appends actions as JSONL. It is safe for controller and
// dispatcher goroutines to share.
//
//nolint:govet // mutex, file and sequence are kept together.
type ActionRecorder struct {
	mu   sync.Mutex
	file *os.File
	path string
	seq  int64
	err  error
}

func NewActionRecorder(outDir string) (*ActionRecorder, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(outDir, "actions.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &ActionRecorder{file: f, path: path}, nil
}

func (r *ActionRecorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.file.Close()
	if err != nil && r.err == nil {
		r.err = err
	}
	return err
}

// Err returns the first recording failure, if any.
func (r *ActionRecorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *ActionRecorder) remember(err error) error {
	if err == nil {
		return nil
	}
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	return err
}

// Record writes one action, clipping serialized arguments at 2 KiB.
func (r *ActionRecorder) Record(laneID, kind string, args any, reason string) error {
	if r == nil || r.file == nil {
		return errShadowRecorderUnavailable
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return r.remember(fmt.Errorf("shadow recorder marshal: %w", err))
	}
	if len(raw) > maxShadowArgumentBytes {
		clipped := raw[:maxShadowArgumentBytes]
		for {
			for len(clipped) > 0 && !utf8.Valid(clipped) {
				clipped = clipped[:len(clipped)-1]
			}
			truncated, marshalErr := json.Marshal(map[string]any{"truncated": true, "text": string(clipped)})
			if marshalErr != nil {
				return r.remember(fmt.Errorf("shadow recorder truncate: %w", marshalErr))
			}
			if len(truncated) <= maxShadowArgumentBytes || len(clipped) == 0 {
				raw = truncated
				break
			}
			clipped = clipped[:len(clipped)-1]
		}
	}
	a := Action{At: time.Now().UTC(), Lane: strings.TrimSpace(laneID), Kind: kind, Arguments: raw, Reason: clip(reason, maxShadowArgumentBytes)}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	a.DecisionID = fmt.Sprintf("shadow-action-%d", r.seq)
	a.EventID = a.DecisionID
	b, err := json.Marshal(a)
	if err != nil {
		recordErr := fmt.Errorf("shadow recorder marshal action: %w", err)
		if r.err == nil {
			r.err = recordErr
		}
		return recordErr
	}
	if _, err := r.file.Write(append(b, '\n')); err != nil {
		recordErr := fmt.Errorf("shadow recorder write: %w", err)
		if r.err == nil {
			r.err = recordErr
		}
		return recordErr
	}
	return nil
}

// ErrShadowUnknown means the observation requires a remote command and is unavailable.
var ErrShadowUnknown = errors.New("shadow read is unknown: read-only probe unavailable")

// ShadowHTTPClient guards the shared Nebula transport, including stream clients
// copied from it. Refusals never reach the underlying RoundTripper.
func ShadowHTTPClient(base *http.Client, recorder *ActionRecorder, clocks ...clock.Clock) *http.Client {
	client := http.Client{Timeout: 30 * time.Second}
	if base != nil {
		client = *base
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var clk clock.Clock = clock.RealClock{}
	if len(clocks) > 0 && clocks[0] != nil {
		clk = clocks[0]
	}
	client.Transport = &shadowTransport{base: transport, recorder: recorder, clock: clk, lastExec: map[string]time.Time{}}
	return &client
}

type shadowTransport struct {
	clock    clock.Clock
	lastExec map[string]time.Time
	base     http.RoundTripper
	recorder *ActionRecorder
	mu       sync.Mutex
}

func (t *shadowTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		return t.base.RoundTrip(req)
	}
	if req.Method == http.MethodPost && req.Body != nil {
		parts := strings.Split(req.URL.EscapedPath(), "/")
		if len(parts) == 5 && parts[1] == "api" && parts[2] == "code-sessions" && parts[3] != "" && parts[4] == "exec" && req.URL.RawQuery == "" {
			raw, err := io.ReadAll(io.LimitReader(req.Body, 16*1024+1))
			_ = req.Body.Close()
			var body struct {
				Command []string `json:"command"`
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err == nil && len(raw) <= 16*1024 && dec.Decode(&body) == nil && dec.Decode(new(any)) == io.EOF {
				command, allowed := shadowReadOnlyCommand(body.Command)
				if allowed && t.reserveExec(parts[3]) {
					if err := t.recorder.Record("", "exec_read", map[string]any{"session_id": parts[3], "command": command}, "allowlisted read-only exec"); err != nil {
						return nil, err
					}
					encoded, err := json.Marshal(struct {
						Command []string `json:"command"`
					}{command})
					if err != nil {
						return nil, err
					}
					forward := req.Clone(req.Context())
					forward.Body = io.NopCloser(bytes.NewReader(encoded))
					forward.ContentLength = int64(len(encoded))
					forward.GetBody = nil
					return t.base.RoundTrip(forward)
				}
			}
		}
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	path := req.URL.EscapedPath()
	if err := t.recorder.Record("", "http_refused", map[string]string{
		"method": req.Method, "path": path, "path_class": shadowPathClass(path),
	}, "shadow transport refuses writes, unknown exec commands and rate-limited reads"); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("shadow transport refused %s %s", req.Method, path)
}

func shadowPathClass(path string) string {
	parts := strings.Split(path, "/")
	// Normalize resource ids while preserving action suffixes (events/stream).
	for i := 2; i+1 < len(parts); i++ {
		switch parts[i] {
		case "code-sessions", "code-terminals", "code-runs", "code-repositories", "terminals", "posts", "mail":
			parts[i+1] = "{id}"
			i++
		}
	}
	return strings.Join(parts, "/")
}

// RecordingDriver is a LaneDriver that forwards reads and records writes.
//
//nolint:govet // wrapper state is grouped by transport and launch tracking.
type RecordingDriver struct {
	inner          lane.LaneDriver
	recorder       *ActionRecorder
	mu             sync.Mutex
	seq            int
	launches       map[string]lane.StartPhaseResult
	syntheticLanes map[string]bool
	seen           map[string]bool
}

func NewRecordingDriver(inner lane.LaneDriver, recorder *ActionRecorder) *RecordingDriver {
	return &RecordingDriver{inner: inner, recorder: recorder, launches: make(map[string]lane.StartPhaseResult), seen: make(map[string]bool), syntheticLanes: make(map[string]bool)}
}

func (d *RecordingDriver) ReportInstructions(runID, laneID, phase string, round int) string {
	if d.inner == nil {
		return ""
	}
	return d.inner.ReportInstructions(runID, laneID, phase, round)
}
func (d *RecordingDriver) PhaseSkill(phase string) string {
	if d.inner == nil {
		return ""
	}
	return d.inner.PhaseSkill(phase)
}
func (d *RecordingDriver) SpawnSession(_ context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	d.mu.Lock()
	d.seq++
	n := d.seq
	d.mu.Unlock()
	res := lane.SpawnResult{StartedAt: time.Now().UTC(), SessionID: fmt.Sprintf("shadow-session-%s-%d", req.LaneID, n), TerminalID: fmt.Sprintf("shadow-terminal-%s-%d", req.LaneID, n), Branch: req.Branch, BaseBranch: req.BaseBranch}
	if err := d.recorder.Record(req.LaneID, "spawn", req, "controller/dispatcher launch intent"); err != nil {
		return lane.SpawnResult{}, err
	}
	d.mu.Lock()
	d.syntheticLanes[req.RunID+"/"+req.LaneID] = true
	d.launches[req.RunID+"/"+req.LaneID+"/"+req.Phase] = lane.StartPhaseResult{StartedAt: res.StartedAt, SessionID: res.SessionID, TerminalID: res.TerminalID}
	d.mu.Unlock()
	return res, nil
}
func (d *RecordingDriver) StartPhaseTerminal(_ context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	d.mu.Lock()
	d.seq++
	n := d.seq
	d.mu.Unlock()
	res := lane.StartPhaseResult{StartedAt: time.Now().UTC(), SessionID: req.SessionID, TerminalID: fmt.Sprintf("shadow-terminal-%s-%d", req.LaneID, n)}
	if res.SessionID == "" {
		res.SessionID = fmt.Sprintf("shadow-session-%s-%d", req.LaneID, n)
	}
	if err := d.recorder.Record(req.LaneID, "start_phase", req, "controller/dispatcher phase launch intent"); err != nil {
		return lane.StartPhaseResult{}, err
	}
	d.mu.Lock()
	d.syntheticLanes[req.RunID+"/"+req.LaneID] = true
	d.launches[req.RunID+"/"+req.LaneID+"/"+req.Phase] = res
	d.mu.Unlock()
	return res, nil
}
func (d *RecordingDriver) FindPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (*lane.StartPhaseResult, error) {
	d.mu.Lock()
	res, ok := d.launches[req.RunID+"/"+req.LaneID+"/"+req.Phase]
	d.mu.Unlock()
	if ok {
		return &res, nil
	}
	if d.syntheticLane(req.RunID, req.LaneID) || shadowID(req.SessionID) || shadowID(req.KnownTerminalID) || d.inner == nil {
		return nil, nil
	}
	return d.inner.FindPhaseTerminal(ctx, req)
}
func (d *RecordingDriver) Steer(_ context.Context, req lane.SteerRequest) error {
	return d.recorder.Record(req.LaneID, "steer", req, "controller/dispatcher steering intent")
}
func (d *RecordingDriver) Stop(_ context.Context, req lane.StopRequest) error {
	return d.recorder.Record(req.LaneID, "stop", req, "controller/dispatcher stop intent")
}
func (d *RecordingDriver) syntheticLane(runID, laneID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.syntheticLanes[runID+"/"+laneID]
}

func shadowID(id string) bool {
	return strings.HasPrefix(id, "shadow-session-") || strings.HasPrefix(id, "shadow-terminal-")
}

func (d *RecordingDriver) recordRead(runID, laneID, kind, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := runID + "/" + laneID + "/" + kind
	if d.seen[key] {
		return nil
	}
	if err := d.recorder.Record(laneID, "read_degraded", map[string]string{"kind": kind, "state": "unknown"}, reason); err != nil {
		return err
	}
	d.seen[key] = true
	return nil
}

func (d *RecordingDriver) ReadReport(ctx context.Context, runID, laneID, terminalID string) (*lane.PhaseReport, error) {
	if d.syntheticLane(runID, laneID) || shadowID(terminalID) || d.inner == nil {
		return nil, nil
	}
	rep, err := d.inner.ReadReport(ctx, runID, laneID, terminalID)
	if rep == nil {
		if recordErr := d.recordRead(runID, laneID, "report_fallback", "No report from GET/local files; remote exec fallback is unavailable, so report-driven phase advancement waits."); recordErr != nil {
			return nil, recordErr
		}
	}
	return rep, err
}
func (d *RecordingDriver) LaneGitState(ctx context.Context, ln lane.Lane) (lane.GitState, error) {
	if d.syntheticLane(ln.RunID, ln.ID) || shadowID(ln.SessionID) || shadowID(ln.TerminalID) {
		return lane.GitState{}, ErrShadowUnknown
	}
	var result lane.GitState
	err := ErrShadowUnknown
	if reader, ok := d.inner.(lane.GitStateReader); ok {
		result, err = reader.LaneGitState(ctx, ln)
	}
	if err != nil {
		if recordErr := d.recordRead(ln.RunID, ln.ID, "git_state", "Read-only git probe unavailable: "+err.Error()); recordErr != nil {
			return lane.GitState{}, recordErr
		}
	}
	return result, err
}
func (d *RecordingDriver) LaneSessionHealth(ctx context.Context, ln lane.Lane) (lane.SessionHealth, error) {
	if d.syntheticLane(ln.RunID, ln.ID) || shadowID(ln.SessionID) || shadowID(ln.TerminalID) {
		return lane.SessionHealth{}, ErrShadowUnknown
	}
	var result lane.SessionHealth
	err := ErrShadowUnknown
	if reader, ok := d.inner.(lane.SessionHealthReader); ok {
		result, err = reader.LaneSessionHealth(ctx, ln)
	}
	if err != nil {
		if recordErr := d.recordRead(ln.RunID, ln.ID, "session_health", "Read-only health probe unavailable: "+err.Error()); recordErr != nil {
			return lane.SessionHealth{}, recordErr
		}
	}
	return result, err
}
func (d *RecordingDriver) TerminalAgentState(ctx context.Context, terminalID string) (string, error) {
	if shadowID(terminalID) {
		return "shadow-launched", nil
	}
	if r, ok := d.inner.(lane.TerminalStateReader); ok {
		return r.TerminalAgentState(ctx, terminalID)
	}
	return "", nil
}
func (d *RecordingDriver) ReportOnlyNoWork(ctx context.Context, ln lane.Lane) (bool, error) {
	if d.syntheticLane(ln.RunID, ln.ID) || shadowID(ln.SessionID) || shadowID(ln.TerminalID) {
		return false, ErrShadowUnknown
	}
	if r, ok := d.inner.(lane.ReportOnlyChecker); ok {
		return r.ReportOnlyNoWork(ctx, ln)
	}
	return false, nil
}
func (d *RecordingDriver) Watch(ctx context.Context, runID string) <-chan lane.Event {
	if d.inner == nil {
		ch := make(chan lane.Event)
		close(ch)
		return ch
	}
	return d.inner.Watch(ctx, runID)
}
func (d *RecordingDriver) ReleaseSession(_ context.Context, sessionID string, force ...bool) error {
	return d.recorder.Record("", "release", map[string]any{"session_id": sessionID, "force": force}, "controller session release intent")
}
func (d *RecordingDriver) UpdatePost(_ context.Context, postID, content string) error {
	return d.recorder.Record("", "post_update", map[string]string{"post_id": postID, "content": content}, "closeout post update intent")
}

var _ lane.LaneDriver = (*RecordingDriver)(nil)
var _ lane.SessionReleaser = (*RecordingDriver)(nil)
var _ lane.PostPublisher = (*RecordingDriver)(nil)

// RecordingMailbox forwards Watch reads and records every send or owner ask.
type RecordingMailbox struct {
	inner    mailbox.Mailbox
	recorder *ActionRecorder
	mu       sync.Mutex
	seq      int64
}

func NewRecordingMailbox(inner mailbox.Mailbox, recorder *ActionRecorder) *RecordingMailbox {
	return &RecordingMailbox{inner: inner, recorder: recorder}
}
func (m *RecordingMailbox) SendLane(_ context.Context, laneID, text string) error {
	return m.recorder.Record(laneID, "mail_send", map[string]string{"text": text}, "mail send intent")
}
func (m *RecordingMailbox) AskOwner(_ context.Context, q mailbox.Question) (string, error) {
	if err := m.recorder.Record(q.LaneID, "ask_owner", q, "owner question intent"); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("shadow-ask-%d", m.seq)
	m.mu.Unlock()
	return id, nil
}
func (m *RecordingMailbox) Watch(ctx context.Context) (<-chan store.Event, error) {
	if m.inner == nil {
		ch := make(chan store.Event)
		close(ch)
		return ch, nil
	}
	return m.inner.Watch(ctx)
}
func (m *RecordingMailbox) Answer(_ context.Context, id, body string) error {
	return m.recorder.Record("", "mail_answer", map[string]string{"id": id, "body": body}, "mail answer intent")
}
func (m *RecordingMailbox) Reply(_ context.Context, laneID, id, body string) error {
	return m.recorder.Record(laneID, "mail_reply", map[string]string{"id": id, "body": body}, "mail reply intent")
}
func (m *RecordingMailbox) Dismiss(_ context.Context, id, reason string) error {
	return m.recorder.Record("", "mail_dismiss", map[string]string{"id": id, "reason": reason}, "mail dismiss intent")
}

// SetAdoptCutoff preserves the optional adopt cutoff contract while intercepting mutations.
func (m *RecordingMailbox) SetAdoptCutoff(cutoff time.Time, st store.Store) {
	if setter, ok := m.inner.(mailbox.AdoptCutoffSetter); ok {
		setter.SetAdoptCutoff(cutoff, st)
	}
}

func (m *RecordingMailbox) SyncAdoptCutoff(ctx context.Context) error {
	if setter, ok := m.inner.(mailbox.AdoptCutoffSetter); ok {
		return setter.SyncAdoptCutoff(ctx)
	}
	return nil
}

var _ mailbox.Mailbox = (*RecordingMailbox)(nil)
var _ mailbox.Responder = (*RecordingMailbox)(nil)
var _ mailbox.Dismisser = (*RecordingMailbox)(nil)
var _ mailbox.AdoptCutoffSetter = (*RecordingMailbox)(nil)

func (t *shadowTransport) reserveExec(session string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock.Now()
	if last, ok := t.lastExec[session]; ok && now.Sub(last) < time.Minute {
		return false
	}
	t.lastExec[session] = now
	return true
}

var shadowRef = regexp.MustCompile(`^[A-Za-z0-9_./@^~:+-]+$`)
var shadowReportName = regexp.MustCompile(`^phase-report\.[A-Za-z0-9_-]+\.md$`)

func shadowReadOnlyCommand(command []string) ([]string, bool) {
	if nebula.IsReadOnlyLaneProbe(command) {
		return command, true
	}
	if len(command) == 0 {
		return nil, false
	}
	args := command[1:]
	switch command[0] {
	case "git":
		if len(args) == 0 {
			return nil, false
		}
		switch args[0] {
		case "status", "rev-parse", "log":
			if model.ValidateReadOnlyCommand("git", args) != nil {
				return nil, false
			}
		case "diff":
			if len(args) < 2 || args[1] != "--stat" || model.ValidateReadOnlyCommand("git", args) != nil {
				return nil, false
			}
		case "rev-list":
			if len(args) != 3 || args[1] != "--count" || !shadowRef.MatchString(args[2]) || strings.HasPrefix(args[2], "-") {
				return nil, false
			}
		default:
			return nil, false
		}
		safe := []string{"git", "--no-optional-locks"}
		if args[0] == "log" || args[0] == "diff" {
			safe = append(safe, args[0], "--no-ext-diff", "--no-textconv")
			safe = append(safe, args[1:]...)
		} else {
			safe = append(safe, args...)
		}
		return safe, true
	case "df":
		if len(args) == 0 || (len(args) == 2 && (args[0] == "-P" || args[0] == "-h") && (args[1] == "." || args[1] == "/")) {
			return command, true
		}
	case "pgrep":
		if len(args) == 2 && (args[0] == "-af" || args[0] == "-f") && args[1] != "" && !strings.HasPrefix(args[1], "-") {
			return command, true
		}
	case "cat":
		if len(args) == 1 && !strings.ContainsAny(args[0], "\\\x00\n\r") && filepath.Clean(args[0]) == args[0] && !strings.HasPrefix(args[0], "-") {
			parts := strings.Split(filepath.ToSlash(args[0]), "/")
			for _, part := range parts {
				if part == ".." {
					return nil, false
				}
			}
			if len(parts) >= 2 && parts[len(parts)-2] == ".git" && shadowReportName.MatchString(parts[len(parts)-1]) {
				return command, true
			}
		}
	}
	return nil, false
}
