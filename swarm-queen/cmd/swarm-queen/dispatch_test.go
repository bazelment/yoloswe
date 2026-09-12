package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/state"
)

func dispatchSeedRun(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	store := state.NewStore(dir)
	if err := store.Create(state.Config{
		Goal: "g", Base: "main", Target: "swarm/t",
		Phases: []state.Phase{{Name: "swe"}, {Name: "clean"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) error {
		st.Lanes = append(st.Lanes, &state.Lane{
			ID: "lane-a", Title: "A", Branch: "b-a",
			Status: state.StatusPlanned, Priority: state.P1,
			Sessions: map[string]string{},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dir, store
}

func TestDeclaredPhaseAcceptsOnlyPhasesTheRunConfigured(t *testing.T) {
	t.Parallel()
	st := &state.State{Config: state.Config{Phases: []state.Phase{
		{Name: "swe"}, {Name: "clean"},
	}}}
	if !declaredPhase(st, "swe") {
		t.Error("swe is declared and must be accepted")
	}
	if declaredPhase(st, "no-such-phase") {
		t.Error("a phase the run never declared must be rejected")
	}
}

// readMission prefers the round-specific mission file, so a retry can carry
// different instructions than the attempt it replaces.
func TestReadMissionPrefersRoundSpecificFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lane-a.swe.mission.txt", "phase-level mission")
	write("lane-a.swe2.mission.txt", "round-2 mission")
	write("lane-a.mission.txt", "lane-level fallback")

	text, path, err := readMission(dir, "lane-a", "swe", 2)
	if err != nil {
		t.Fatal(err)
	}
	if text != "round-2 mission" {
		t.Errorf("text = %q, want the round-specific file to win", text)
	}
	if path != "lane-a.swe2.mission.txt" {
		t.Errorf("path = %q", path)
	}
}

// Without a round-specific file, readMission falls back to the phase-level
// file, then the lane-level one.
func TestReadMissionFallsBackInOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lane-a.mission.txt"), []byte("lane fallback"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, path, err := readMission(dir, "lane-a", "swe", 1)
	if err != nil {
		t.Fatal(err)
	}
	if text != "lane fallback" || path != "lane-a.mission.txt" {
		t.Errorf("text=%q path=%q, want the lane-level fallback", text, path)
	}
}

// A missing mission is an ERROR, not a silent fallback to the lane title:
// dispatch is explicit, and silently briefing a lane with its own name is how
// a retry repeats the failure it was meant to fix.
func TestReadMissionErrorsWhenNoFileExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, _, err := readMission(dir, "lane-a", "swe", 1)
	if err == nil {
		t.Fatal("a missing mission must error, not silently fall back to the lane title")
	}
	if !strings.Contains(err.Error(), "no mission file") {
		t.Errorf("err = %v", err)
	}
}

// dispatch refuses a phase the run never declared, before touching anything.
func TestRunDispatchRefusesAnUndeclaredPhase(t *testing.T) {
	dir, _ := dispatchSeedRun(t)
	dispatchLane = "lane-a"
	dispatchPhase = "no-such-phase"
	dispatchRound = 0
	dispatchApply = false
	t.Cleanup(func() { dispatchPhase = "" })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runDispatch(cmd, []string{dir})
	if err == nil {
		t.Fatal("an undeclared phase must be refused")
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Errorf("err = %v", err)
	}
}

// dispatch refuses to overwrite a round that already recorded a session --
// the sessions map is keyed by phase, so reusing a round silently destroys the
// earlier session id.
func TestRunDispatchRefusesToOverwriteARecordedRound(t *testing.T) {
	dir, store := dispatchSeedRun(t)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.RecordSession("swe", 1, "sess-existing")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dispatchLane = "lane-a"
	dispatchPhase = "swe"
	dispatchRound = 1
	dispatchApply = false
	t.Cleanup(func() { dispatchPhase = ""; dispatchRound = 0 })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runDispatch(cmd, []string{dir})
	if err == nil {
		t.Fatal("dispatching an already-recorded round must be refused")
	}
	if !strings.Contains(err.Error(), "already recorded session sess-existing") {
		t.Errorf("err = %v", err)
	}
}

// dispatch refuses a lane that is not in the ledger at all.
func TestRunDispatchRefusesAnUnknownLane(t *testing.T) {
	dir, _ := dispatchSeedRun(t)
	dispatchLane = "no-such-lane"
	dispatchPhase = ""
	dispatchRound = 0
	dispatchApply = false
	t.Cleanup(func() { dispatchLane = "" })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runDispatch(cmd, []string{dir})
	if err == nil {
		t.Fatal("an unknown lane must be refused")
	}
	if !strings.Contains(err.Error(), "not in the ledger") {
		t.Errorf("err = %v", err)
	}
}
