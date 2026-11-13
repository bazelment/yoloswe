package bramble_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/lane/bramble"
	br "github.com/bazelment/yoloswe/swarm-queen/bramble"
)

type fakeClient struct{ sessions []br.Session }

func (f *fakeClient) NewSession(context.Context, br.SpawnRequest) (br.SpawnResult, error) {
	return br.SpawnResult{SessionID: "s1"}, nil
}
func (f *fakeClient) SendInput(context.Context, string, string) error    { return nil }
func (f *fakeClient) SendKey(context.Context, string, string) error      { return nil }
func (f *fakeClient) ListSessions(context.Context) ([]br.Session, error) { return f.sessions, nil }

func TestPhaseBriefContract(t *testing.T) {
	d, err := bramble.New(bramble.Config{Client: &fakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.PhaseSkill("review"); got != "/pr-polish" {
		t.Fatalf("review skill=%q", got)
	}
	if got := d.PhaseSkill("clean"); got != "/simplify" {
		t.Fatalf("clean skill=%q", got)
	}
	if got := d.ReportInstructions("run", "lane", "review", 1); !strings.Contains(got, "Reason:") {
		t.Fatalf("handback=%q", got)
	}
}

func TestSpawnCapturesBaseBeforeClientStartsWorker(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "test"}, {"commit", "--allow-empty", "-m", "base", "-q"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_ = strings.TrimSpace(string(mustGit(t, dir, "rev-parse", "HEAD")))
	client := &commitOnStartClient{dir: dir}
	d, err := bramble.New(bramble.Config{Client: client, Repo: dir})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.SpawnSession(t.Context(), lane.SpawnRequest{RunID: "r", LaneID: "l", Phase: "swe", Brief: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseCommit != "" {
		t.Fatalf("base=%q, want no locally resolved base", got.BaseCommit)
	}
}

type commitOnStartClient struct{ dir string }

func (c *commitOnStartClient) NewSession(ctx context.Context, _ br.SpawnRequest) (br.SpawnResult, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", c.dir, "commit", "--allow-empty", "-m", "worker-start", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		return br.SpawnResult{}, fmt.Errorf("commit: %v: %s", err, out)
	}
	return br.SpawnResult{SessionID: "s1"}, nil
}
func (c *commitOnStartClient) SendInput(context.Context, string, string) error    { return nil }
func (c *commitOnStartClient) SendKey(context.Context, string, string) error      { return nil }
func (c *commitOnStartClient) ListSessions(context.Context) ([]br.Session, error) { return nil, nil }
func mustGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWatchEmitsLifecycleKinds(t *testing.T) {
	dir := t.TempDir()
	raw := "Outcome: done\nHEAD: 0123456789abcdef0123456789abcdef01234567\n\ncompleted"
	if err := os.WriteFile(filepath.Join(dir, "lane.swe.report.md"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeClient{sessions: []br.Session{{ID: "s1", Status: "waiting"}}}
	d, err := bramble.New(bramble.Config{Client: f, PollInterval: time.Millisecond, InactivityAfter: time.Hour, RunDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.StartPhaseTerminal(t.Context(), lane.StartPhaseRequest{RunID: "r", LaneID: "lane", SessionID: "s1", Phase: "swe", Round: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := d.Watch(ctx, "r")
	select {
	case ev := <-ch:
		if ev.Kind != lane.EventPhaseReported {
			t.Fatalf("kind=%q, want %q", ev.Kind, lane.EventPhaseReported)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestReportInstructionsPathIsReadPath(t *testing.T) {
	t.Parallel()
	for _, round := range []int{1, 2} {
		t.Run(lane.PhaseRoundKey("swe", round), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			f := &fakeClient{sessions: []br.Session{{ID: "s1", Status: "waiting"}}}
			d, err := bramble.New(bramble.Config{Client: f, RunDir: dir, PollInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "lane."+lane.PhaseRoundKey("swe", round)+".report.md")
			if got := d.ReportInstructions("run", "lane", "swe", round); !strings.Contains(got, path) {
				t.Fatalf("instructions=%q, want path %q", got, path)
			}
			raw := "Outcome: done\nHEAD: abc\n"
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := d.StartPhaseTerminal(t.Context(), lane.StartPhaseRequest{RunID: "run", LaneID: "lane", SessionID: "s1", Phase: "swe", Round: round, RunDir: filepath.Join(dir, "wrong")}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ReadReport(t.Context(), "run", "lane", "s1"); err != nil {
				t.Fatalf("ReadReport did not read advertised path: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			select {
			case ev := <-d.Watch(ctx, "run"):
				if ev.Kind != lane.EventPhaseReported || ev.Phase != "swe" {
					t.Fatalf("event = %+v, want report for round %d", ev, round)
				}
			case <-time.After(time.Second):
				t.Fatal("Watch did not read advertised path")
			}
		})
	}
}

func TestWatchEmitsEachEventKind(t *testing.T) {
	cases := []struct {
		name, status string
		want         lane.EventKind
	}{
		{"turn ended", "turn.ended", lane.EventTurnEndedNoReport},
		{"waiting", "waiting", lane.EventWaitingForInput},
		{"exited", "exited", lane.EventExited},
		{"quota", "quota", lane.EventQuotaFailure},
		{"auth", "auth", lane.EventAuthFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeClient{sessions: []br.Session{{ID: "s1", Status: tc.status}}}
			d, err := bramble.New(bramble.Config{Client: f, PollInterval: time.Millisecond, InactivityAfter: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.StartPhaseTerminal(t.Context(), lane.StartPhaseRequest{RunID: "r", LaneID: "lane", SessionID: "s1", Phase: "swe", Round: 1}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			select {
			case ev := <-d.Watch(ctx, "r"):
				if ev.Kind != tc.want {
					t.Fatalf("kind=%q, want %q", ev.Kind, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("timeout")
			}
		})
	}
	t.Run("no activity", func(t *testing.T) {
		f := &fakeClient{}
		d, err := bramble.New(bramble.Config{Client: f, PollInterval: time.Millisecond, InactivityAfter: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		select {
		case ev := <-d.Watch(ctx, "r"):
			if ev.Kind != lane.EventNoActivity {
				t.Fatalf("kind=%q", ev.Kind)
			}
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}
	})
}

func TestWatchReportsEachPhaseRoundAndTransition(t *testing.T) {
	dir := t.TempDir()
	f := &fakeClient{sessions: []br.Session{{ID: "s1", Status: "waiting"}}}
	d, err := bramble.New(bramble.Config{Client: f, PollInterval: time.Millisecond, InactivityAfter: time.Hour, RunDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	write := func(round int, outcome string) {
		t.Helper()
		raw := "Outcome: " + outcome + "\nHEAD: 0123456789abcdef0123456789abcdef01234567\n\nround"
		if err := os.WriteFile(filepath.Join(dir, "lane."+lane.PhaseRoundKey("swe", round)+".report.md"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := func(round int) {
		t.Helper()
		if _, err := d.StartPhaseTerminal(t.Context(), lane.StartPhaseRequest{RunID: "r", LaneID: "lane", SessionID: "s1", Phase: "swe", Round: round}); err != nil {
			t.Fatal(err)
		}
	}
	write(1, "done")
	start(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := d.Watch(ctx, "r")
	select {
	case ev := <-watch:
		if ev.Kind != lane.EventPhaseReported {
			t.Fatalf("round 1 kind=%q", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("round 1 timeout")
	}
	write(2, "blocked")
	start(2)
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-watch:
			if ev.Kind == lane.EventPhaseReported {
				if ev.Report == nil || ev.Report.Outcome != lane.OutcomeBlocked {
					t.Fatalf("round 2 event=%+v", ev)
				}
				return
			}
		case <-deadline:
			t.Fatal("round 2 timeout")
		}
	}
}
