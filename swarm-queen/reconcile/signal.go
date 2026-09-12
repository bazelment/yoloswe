// Package reconcile derives observed truth about a swarm from git, gh, tmux and
// bramble. Nothing here trusts an agent's self-report.
package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// SignalKind is what a lane is claiming by writing a file into the run dir.
type SignalKind string

const (
	// SignalDone claims a phase finished. It is a CLAIM about work, not the
	// work: a .done on a branch with no commits means the phase did not
	// complete, and must never be merged on the strength of the file alone.
	SignalDone SignalKind = "done"
	// SignalNeedsSWE is a review rejection sending the lane back to swe.
	//
	// This signal existed in live runs for weeks while being invisible to the
	// watcher and undocumented in SKILL.md; a bounced lane surfaced only via the
	// 15-minute stall path or the 60-minute timeout.
	SignalNeedsSWE SignalKind = "needs-swe"
)

// Signal is one claim file in the run directory.
type Signal struct {
	Lane  string
	Phase string
	Kind  SignalKind
	Path  string
	Round int
}

// signalSuffixes maps a filename suffix to its kind. Extend here rather than at
// call sites so an unrecognised suffix cannot be silently ignored.
var signalSuffixes = map[string]SignalKind{
	".done":      SignalDone,
	".needs-swe": SignalNeedsSWE,
}

// ParseSignalName decodes "<lane>.<phase>.<kind>" into its parts.
//
// The lane id is everything before the FIRST dot, matching watch_lanes.sh's
// `t="${stem%%.*}"`. That is why a lane id may not contain a dot: it would
// misroute every signal the lane ever emits. A first-phase signal may omit the
// phase segment entirely ("<lane>.done").
func ParseSignalName(name string) (Signal, bool) {
	for suffix, kind := range signalSuffixes {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		stem := strings.TrimSuffix(name, suffix)
		lane, rest, hasPhase := strings.Cut(stem, ".")
		if lane == "" {
			return Signal{}, false
		}
		sig := Signal{Lane: lane, Kind: kind, Round: 1}
		if hasPhase && rest != "" {
			sig.Phase, sig.Round = state.SplitPhaseRound(rest)
		}
		return sig, true
	}
	return Signal{}, false
}

// ScanSignals lists every signal file currently in the run directory.
func ScanSignals(runDir string) ([]Signal, error) {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return nil, fmt.Errorf("scan run dir: %w", err)
	}
	var out []Signal
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		sig, ok := ParseSignalName(e.Name())
		if !ok {
			continue
		}
		sig.Path = filepath.Join(runDir, e.Name())
		out = append(out, sig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// SignalSet is a baseline of signal files, used to report only what is NEW.
//
// The watcher must report appearances, not contents. A live run accumulates
// dozens of signal files (44 in one observed run), so a watcher that reported
// everything it found would wake the orchestrator on last tick's signals every
// single tick — the same "nothing to decide" wakeup storm that made a
// commit-keyed watcher useless.
type SignalSet map[string]Signal

// Baseline snapshots the signals present now.
func Baseline(runDir string) (SignalSet, error) {
	sigs, err := ScanSignals(runDir)
	if err != nil {
		return nil, err
	}
	set := make(SignalSet, len(sigs))
	for _, s := range sigs {
		set[s.Path] = s
	}
	return set, nil
}

// NewSince returns signals present now but absent from the baseline.
func NewSince(runDir string, base SignalSet) ([]Signal, error) {
	sigs, err := ScanSignals(runDir)
	if err != nil {
		return nil, err
	}
	var fresh []Signal
	for _, s := range sigs {
		if _, seen := base[s.Path]; !seen {
			fresh = append(fresh, s)
		}
	}
	return fresh, nil
}

// CommitBaseline advances the baseline only when every decision applied.
//
// A tick that saved the baseline and THEN reported failures marked this tick's
// signals as seen regardless: a lane whose spawn or reap failed had its `.done`
// or `.needs-swe` dropped permanently, because the next tick no longer saw the
// claim and nothing retried it. Consuming a signal is a claim to have acted on
// it, so the two must not be separable -- hence one function that takes the
// failure count rather than two calls a caller can order wrongly.
//
// Returns (false, nil) when the baseline was deliberately left unadvanced.
func CommitBaseline(runDir, baselinePath string, failed int) (bool, error) {
	if failed > 0 {
		return false, nil
	}
	current, err := Baseline(runDir)
	if err != nil {
		return false, err
	}
	if err := SaveBaseline(baselinePath, current); err != nil {
		return false, err
	}
	return true, nil
}

// SaveBaseline persists a signal set so the next tick reports only what is new.
//
// Stored as sorted paths, one per line: a plain text file is legible to a human
// debugging a run, and the set is small.
func SaveBaseline(path string, set SignalSet) error {
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	body := strings.Join(paths, "\n")
	if body != "" {
		body += "\n"
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// LoadBaseline reads a persisted signal set.
//
// A missing file returns os.ErrNotExist so the caller can distinguish "first
// tick" from "no signals": treating them alike would replay a whole run dir's
// history as fresh claims on adoption.
func LoadBaseline(path string) (SignalSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	set := SignalSet{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sig, ok := ParseSignalName(filepath.Base(line))
		if !ok {
			continue
		}
		sig.Path = line
		set[line] = sig
	}
	return set, nil
}
