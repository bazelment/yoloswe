//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/store"
)

const defaultExistingReportTerminalID = "a8cf0d8f-fb9f-4012-aa7c-fde3652f7e4c"

func requireLiveDriver(t *testing.T, st *store.MemoryStore) *nebula.Driver {
	t.Helper()
	if strings.TrimSpace(os.Getenv(nebula.EnvAPIURL)) == "" || strings.TrimSpace(os.Getenv(nebula.EnvToken)) == "" {
		t.Skip("skipping live Nebula test: NEBULA_API_URL or NEBULA_TOKEN not set")
	}
	var opts []nebula.Option
	if st != nil {
		opts = append(opts, nebula.WithEventStore(st.Events()))
	}
	drv, err := nebula.NewFromEnv(opts...)
	if err != nil {
		t.Fatalf("nebula.NewFromEnv: %v", err)
	}
	return drv
}

func TestReadExistingTerminalReport(t *testing.T) {
	drv := requireLiveDriver(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	termID := strings.TrimSpace(os.Getenv("NEBULA_TEST_REPORT_TERMINAL_ID"))
	if termID == "" {
		termID = defaultExistingReportTerminalID
	}

	rep, err := drv.ReadReport(ctx, "20261112-014646-orch-harness", "core", termID)
	if err != nil {
		t.Fatalf("ReadReport(%s): %v", termID, err)
	}
	if !rep.Outcome.Valid() {
		t.Fatalf("invalid Outcome %q in report: %+v", rep.Outcome, rep)
	}
	if rep.TerminalID != termID {
		t.Fatalf("TerminalID = %q, want %q", rep.TerminalID, termID)
	}
	if rep.HeadSHA == "" || rep.Summary == "" {
		t.Fatalf("expected non-empty HeadSHA and Summary, got HeadSHA=%q Summary=%q", rep.HeadSHA, rep.Summary)
	}
	t.Logf("read existing phase report: terminal=%s outcome=%s head=%s pr=%d reportedAt=%s",
		rep.TerminalID, rep.Outcome, rep.HeadSHA, rep.PR, rep.ReportedAt.Format(time.RFC3339))
}

func TestSpawnStartReadReportAndRelease(t *testing.T) {
	st := store.NewMemoryStore(nil)
	drv := requireLiveDriver(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	runID := fmt.Sprintf("live-test-%d", time.Now().Unix())
	laneID := "nebuladriver-live"
	currSessID := strings.TrimSpace(os.Getenv(nebula.EnvSessionID))
	if currSessID != "" {
		if detail, err := drv.GetSession(ctx, currSessID); err == nil {
			if r := detail.Session.Metadata["swarmRun"]; r != "" {
				runID = r
			}
			if l := detail.Session.Metadata["swarmLane"]; l != "" {
				laneID = l
			}
		}
	}
	branch := fmt.Sprintf("swarm-lane/%s/%s-live-%d", runID, laneID, time.Now().Unix())

	spawned, err := drv.SpawnSession(ctx, lane.SpawnRequest{
		RunID:      runID,
		LaneID:     laneID,
		Branch:     branch,
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}
	if spawned.SessionID == "" {
		t.Fatal("SpawnSession returned empty SessionID")
	}
	if currSessID != "" && spawned.SessionID == currSessID {
		t.Fatalf("SpawnSession reused current host session %s instead of creating branch session", currSessID)
	}
	t.Logf("spawned live session: %s on branch %s", spawned.SessionID, branch)

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		if err := drv.ReleaseSession(cleanCtx, spawned.SessionID, true); err != nil {
			t.Logf("cleanup ReleaseSession(%s): %v", spawned.SessionID, err)
		} else {
			t.Logf("released live session: %s", spawned.SessionID)
		}
	})

	sentinelSHA := "8942b50a2c4576b7ae4201dcad1754db65d6643b"
	brief := fmt.Sprintf(
		"Run this exact bash command and then end your turn immediately without reading any files:\nprintf 'Outcome: done\\nHEAD: %s\\nRiskiest step: live terminal phase report\\nObserved: live integration test -> ok\\n\\nLive test report.\\n' | nebula phase report --outcome done --head %s --stdin",
		sentinelSHA, sentinelSHA,
	)
	started, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunID:     runID,
		LaneID:    laneID,
		SessionID: spawned.SessionID,
		Phase:     "swe",
		Round:     1,
		Harness:   "claude",
		Model:     "haiku",
		Effort:    "low",
		Brief:     brief,
	})
	if err != nil {
		t.Fatalf("StartPhaseTerminal: %v", err)
	}
	if started.TerminalID == "" {
		t.Fatal("StartPhaseTerminal returned empty TerminalID")
	}
	t.Logf("started live terminal: %s", started.TerminalID)

	streamCtx, streamCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer streamCancel()
	if err := drv.StreamEvents(streamCtx, nebula.StreamRequest{
		Events:       st.Events(),
		RunID:        runID,
		LaneID:       laneID,
		SessionID:    spawned.SessionID,
		TerminalID:   started.TerminalID,
		StopOnReport: true,
	}); err != nil {
		t.Fatalf("StreamEvents waiting for phase.reported: %v", err)
	}

	evs, err := st.Events().List(ctx, store.EventFilter{
		RunID:  runID,
		LaneID: laneID,
		Kinds:  []store.EventKind{store.EventLaneReport},
	})
	if err != nil {
		t.Fatalf("Events().List: %v", err)
	}
	if len(evs) == 0 {
		t.Fatalf("expected streamed store.EventLaneReport, got 0 events")
	}

	rep, err := drv.ReadReport(ctx, runID, laneID, started.TerminalID)
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}
	if rep.Outcome != lane.OutcomeDone || rep.HeadSHA != sentinelSHA {
		t.Fatalf("unexpected report: %+v", rep)
	}
	t.Logf("verified live report: outcome=%s head=%s summary=%q", rep.Outcome, rep.HeadSHA, rep.Summary)

	if err := drv.Stop(ctx, lane.StopRequest{
		RunID:      runID,
		LaneID:     laneID,
		SessionID:  spawned.SessionID,
		TerminalID: started.TerminalID,
	}); err != nil {
		t.Fatalf("Stop terminal: %v", err)
	}
}
