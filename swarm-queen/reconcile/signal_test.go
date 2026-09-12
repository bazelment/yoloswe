package reconcile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSignalName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		wantLane  string
		wantPhase string
		wantKind  SignalKind
		wantRound int
		wantOK    bool
	}{
		{"base-image-hash-sync.swe.done", "base-image-hash-sync", "swe", SignalDone, 1, true},
		{"deploy-progress-schema.github-review.done", "deploy-progress-schema", "github-review", SignalDone, 1, true},
		{"migration-pool-tuning.local-review-r2.needs-swe", "migration-pool-tuning", "local-review", SignalNeedsSWE, 2, true},
		{"monitor-provenance.local-review-r1.needs-swe", "monitor-provenance", "local-review", SignalNeedsSWE, 1, true},
		{"replay-harness-modernize.local-review.needs-swe", "replay-harness-modernize", "local-review", SignalNeedsSWE, 1, true},
		{"error-taxonomy.swe6.done", "error-taxonomy", "swe", SignalDone, 6, true},
		// A first-phase signal may omit the phase segment.
		{"gaps.done", "gaps", "", SignalDone, 1, true},
		// Non-signals must be ignored, not misparsed.
		{"OBJECTIVE.md", "", "", "", 0, false},
		{"state.json", "", "", "", 0, false},
		{"base-image-hash-sync.swe.brief.txt", "", "", "", 0, false},
		{"base-image-hash-sync.swe.spawn.json", "", "", "", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseSignalName(tc.name)
		if ok != tc.wantOK {
			t.Errorf("ParseSignalName(%q) ok = %v, want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Lane != tc.wantLane || got.Phase != tc.wantPhase ||
			got.Round != tc.wantRound || got.Kind != tc.wantKind {
			t.Errorf("ParseSignalName(%q) = %+v, want lane=%q phase=%q round=%d kind=%q",
				tc.name, got, tc.wantLane, tc.wantPhase, tc.wantRound, tc.wantKind)
		}
	}
}

// The watcher reports APPEARANCES, not contents. A run dir accumulates dozens of
// signal files, so reporting everything found would wake the orchestrator on
// last tick's signals every tick -- nothing to decide, every time.
func TestNewSinceReportsOnlyFreshSignals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	touch := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Pre-existing signals from earlier phases.
	touch("lane-a.swe.done")
	touch("lane-a.clean.done")
	touch("lane-b.swe.done")

	base, err := Baseline(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 3 {
		t.Fatalf("baseline has %d signals, want 3", len(base))
	}

	// Nothing new yet.
	fresh, err := NewSince(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Fatalf("expected no fresh signals, got %+v", fresh)
	}

	// A review rejection arrives.
	touch("lane-b.local-review-r1.needs-swe")
	fresh, err = NewSince(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 {
		t.Fatalf("got %d fresh signals, want 1: %+v", len(fresh), fresh)
	}
	if fresh[0].Kind != SignalNeedsSWE || fresh[0].Lane != "lane-b" || fresh[0].Round != 1 {
		t.Errorf("unexpected fresh signal: %+v", fresh[0])
	}
}

// Non-signal files vastly outnumber signals in a real run dir (briefs, reports,
// spawn.json, the ledger). None may be misread as a claim.
func TestScanIgnoresNonSignalFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{
		"OBJECTIVE.md", "state.json", "ledger.md",
		"lane-a.swe.brief.txt", "lane-a.swe.spawn.json", "lane-a.swe.md",
		"lane-a.swe.done",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sigs, err := ScanSignals(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 {
		t.Fatalf("got %d signals, want 1: %+v", len(sigs), sigs)
	}
	if sigs[0].Lane != "lane-a" || sigs[0].Kind != SignalDone {
		t.Errorf("unexpected signal: %+v", sigs[0])
	}
}

// The baseline must persist across processes: each tick is a fresh process, so
// an in-memory baseline would replay every signal in the run dir every time.
func TestBaselineRoundTripsThroughDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{"a.swe.done", "b.local-review-r2.needs-swe"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base, err := Baseline(dir)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, ".baseline")
	if err := SaveBaseline(path, base); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(base) {
		t.Fatalf("loaded %d signals, saved %d", len(loaded), len(base))
	}

	// Nothing new against the reloaded baseline.
	fresh, err := NewSince(dir, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Errorf("reloaded baseline reported stale signals as fresh: %+v", fresh)
	}

	// A genuinely new signal is still detected.
	if err := os.WriteFile(filepath.Join(dir, "c.swe.done"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err = NewSince(dir, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0].Lane != "c" {
		t.Errorf("expected exactly the new signal, got %+v", fresh)
	}
}

// A missing baseline must be distinguishable from an empty one: adopting an
// in-flight run must treat existing signals as history, not as a wave of fresh
// claims.
func TestLoadBaselineDistinguishesMissingFromEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := LoadBaseline(filepath.Join(dir, "absent")); !os.IsNotExist(err) {
		t.Errorf("missing baseline should report os.ErrNotExist, got %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := SaveBaseline(empty, SignalSet{}); err != nil {
		t.Fatal(err)
	}
	set, err := LoadBaseline(empty)
	if err != nil {
		t.Fatalf("an empty baseline must load cleanly: %v", err)
	}
	if len(set) != 0 {
		t.Errorf("expected an empty set, got %+v", set)
	}
}

// A tick that failed to apply a decision must NOT consume the signals it saw.
// Saving the baseline first and reporting the failure afterwards marked them seen
// anyway, so the next tick never saw the claim again and nothing retried it.
func TestCommitBaselineHoldsSignalsWhenDecisionsFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lane-a.swe.done"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(dir, ".baseline")

	advanced, err := CommitBaseline(dir, basePath, 1)
	if err != nil {
		t.Fatal(err)
	}
	if advanced {
		t.Error("the baseline must not advance while a decision failed to apply")
	}
	if _, err := os.Stat(basePath); !os.IsNotExist(err) {
		t.Errorf("no baseline should have been written, stat err = %v", err)
	}

	// The signal is therefore still new to the next tick, which is the whole
	// point: a dropped .done is a lane that reported completion to nobody.
	fresh, err := NewSince(dir, SignalSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 {
		t.Fatalf("the unconsumed signal must still be visible, got %d", len(fresh))
	}
}

// With every decision applied, the baseline advances and the signal is consumed.
func TestCommitBaselineAdvancesWhenNothingFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lane-a.swe.done"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(dir, ".baseline")

	advanced, err := CommitBaseline(dir, basePath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !advanced {
		t.Fatal("the baseline must advance once every decision applied")
	}
	saved, err := LoadBaseline(basePath)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := NewSince(dir, saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Errorf("a consumed signal must not be new to the next tick, got %v", fresh)
	}
}
