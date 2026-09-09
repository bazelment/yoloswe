package state

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// roundSuffix matches the ad-hoc round encodings real runs put in session keys
// and signal filenames: `swe2`, `local-review7`, `local-review-r3`.
var roundSuffix = regexp.MustCompile(`^(.*?)-?r?(\d+)$`)

// Attempt is one execution of a phase for a lane. Rework creates a new attempt
// rather than overwriting the previous one.
type Attempt struct {
	Phase   string `json:"phase"`
	Session string `json:"session"`
	Round   int    `json:"round"`
}

// SplitPhaseRound decodes a session key or signal stem into its phase and round.
// Round 1 is the bare phase name.
//
// This exists because `sessions` is a map keyed by phase, so a lane that reworks
// overwrites the previous round's session id unless the orchestrator invents a
// suffixed key. Real runs did both, inconsistently: one lane recorded
// swe/clean/local-review then jumped straight to swe7 and local-review7, so the
// session ids for rounds 2 through 6 were silently destroyed. A lane that
// reached round 11 retained 14 of its ~22 sessions.
func SplitPhaseRound(key string) (phase string, round int) {
	m := roundSuffix.FindStringSubmatch(key)
	if m == nil {
		return key, 1
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return key, 1
	}
	// A phase legitimately ending in a digit would be misread; require a
	// non-empty stem so "swe2" splits but "2" does not.
	if m[1] == "" {
		return key, 1
	}
	return m[1], n
}

// PhaseRoundKey is the inverse of SplitPhaseRound. Round 1 keeps the bare phase
// name so keys stay compatible with what ledger.py and the shell scripts write.
func PhaseRoundKey(phase string, round int) string {
	if round <= 1 {
		return phase
	}
	return fmt.Sprintf("%s%d", phase, round)
}

// Attempts decodes the sessions map into ordered attempts, so rework history is
// legible even though the underlying storage is a flat phase->session map.
func (l *Lane) Attempts() []Attempt {
	out := make([]Attempt, 0, len(l.Sessions))
	for key, sess := range l.Sessions {
		phase, round := SplitPhaseRound(key)
		out = append(out, Attempt{Phase: phase, Round: round, Session: sess})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Phase != out[j].Phase {
			return out[i].Phase < out[j].Phase
		}
		return out[i].Round < out[j].Round
	})
	return out
}

// RecordSession stores the session for a phase attempt without clobbering an
// earlier round's id.
func (l *Lane) RecordSession(phase string, round int, session string) {
	if l.Sessions == nil {
		l.Sessions = map[string]string{}
	}
	l.Sessions[PhaseRoundKey(phase, round)] = session
}

// SessionFor returns the recorded session id for a phase attempt.
func (l *Lane) SessionFor(phase string, round int) (string, bool) {
	s, ok := l.Sessions[PhaseRoundKey(phase, round)]
	return s, ok
}

// MaxRound reports the highest recorded round for a phase, 0 if none.
func (l *Lane) MaxRound(phase string) int {
	max := 0
	for _, a := range l.Attempts() {
		if a.Phase == phase && a.Round > max {
			max = a.Round
		}
	}
	return max
}

// LostRounds reports rounds that are missing from the middle of a phase's
// history — evidence that an earlier attempt's session id was overwritten.
// Reconcile surfaces these rather than silently presenting a partial history as
// complete.
func (l *Lane) LostRounds(phase string) []int {
	seen := map[int]bool{}
	for _, a := range l.Attempts() {
		if a.Phase == phase {
			seen[a.Round] = true
		}
	}
	var missing []int
	for r := 1; r <= l.MaxRound(phase); r++ {
		if !seen[r] {
			missing = append(missing, r)
		}
	}
	return missing
}

// UnknownPhases returns phases referenced by this lane that the run config does
// not declare. Real runs invented `merged` and `rebase` at runtime, which makes
// phase-ordered logic silently wrong.
func (l *Lane) UnknownPhases(cfg *Config) []string {
	declared := map[string]bool{}
	for _, n := range cfg.PhaseNames() {
		declared[n] = true
	}
	var unknown []string
	seen := map[string]bool{}
	check := func(p string) {
		if p == "" || declared[p] || seen[p] {
			return
		}
		seen[p] = true
		unknown = append(unknown, p)
	}
	check(l.Phase)
	for _, a := range l.Attempts() {
		check(a.Phase)
	}
	sort.Strings(unknown)
	return unknown
}
