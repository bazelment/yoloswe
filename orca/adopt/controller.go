package adopt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

// Controllers attaches host runs to the same deterministic lifecycle used for
// orca's own lanes. Its lifetime is the chat process, not an individual command.
type Controllers struct {
	ctx    context.Context
	store  store.Store
	driver lane.LaneDriver
	clock  clock.Clock
	runs   map[string]*controlledRun
	mu     sync.Mutex
}

type controlledRun struct {
	controller *controller.Controller
	cancel     context.CancelFunc
	done       chan struct{}
	stop       chan struct{}
	run        Run
}

func NewControllers(ctx context.Context, st store.Store, driver lane.LaneDriver, clk clock.Clock) *Controllers {
	return &Controllers{ctx: ctx, store: st, driver: driver, clock: clk, runs: map[string]*controlledRun{}}
}

func (d *Controllers) Start(ctx context.Context, r Run) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cr := d.runs[r.ID]; cr != nil {
		select {
		case <-cr.done:
			delete(d.runs, r.ID)
		default:
			return nil
		}
	}
	journal, err := readJournal(filepath.Join(r.Dir, leaseFile))
	if err != nil {
		return err
	}
	generation := journal.Marker
	workflow, lanes, err := controllerState(r)
	if err != nil {
		return err
	}
	if err := d.store.Runs().Create(ctx, store.Run{ID: r.ID, Objective: r.Goal, DefaultRecipe: workflow.Name}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	holder := fmt.Sprintf("%s:%p", r.Holder, d)
	if _, err := d.store.Runs().AcquireLease(ctx, r.ID, holder, controller.DefaultLeaseTTL); err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			_ = d.store.Runs().ReleaseLease(context.WithoutCancel(ctx), r.ID, holder)
		}
	}()
	storedRun, err := d.store.Runs().Get(ctx, r.ID)
	if err != nil {
		return err
	}
	resume := generation != "" && storedRun.PlanRef == generation
	if err := d.store.Runs().UpdateObjective(ctx, r.ID, r.Goal, generation); err != nil {
		return err
	}
	workflow.Version = 0
	saved, err := d.store.Recipes().Save(ctx, workflow)
	if err != nil {
		return err
	}
	workflow = saved
	for i := range lanes {
		ln := &lanes[i]
		if resume {
			stored, err := d.store.Lanes().Get(ctx, r.ID, ln.ID)
			if err == nil {
				*ln = *stored
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		ln.RecipeVersion = workflow.Version
		if err := d.store.Lanes().Upsert(ctx, *ln); err != nil {
			return err
		}
		if tracker, ok := d.driver.(interface{ TrackLane(lane.Lane) }); ok && !ln.Status.Terminal() {
			tracker.TrackLane(*ln)
		}
	}
	ctrl := controller.New(controller.Config{RunID: r.ID, HolderID: holder}, d.store, d.driver, d.clock, nil)
	// Fail synchronously if another process still holds the controller lease.
	if err := ctrl.Step(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(d.ctx)
	if err := ctrl.StartWatches(runCtx); err != nil {
		cancel()
		_ = ctrl.ReleaseLease(ctx)
		return err
	}
	cr := &controlledRun{controller: ctrl, cancel: cancel, done: make(chan struct{}), stop: make(chan struct{}), run: r}
	d.runs[r.ID] = cr
	started = true
	go func() {
		defer close(cr.done)
		// The ledger is also the host orchestrator's resume point. Checkpoint only
		// changed lane state; quiescing Stop performs the final checkpoint.
		ticker := d.clock.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		var previous []byte
		for {
			if err := ctrl.Step(runCtx); err != nil && runCtx.Err() == nil {
				slog.Error("adopted controller step", "run", r.ID, "error", err)
			}
			if runCtx.Err() != nil {
				return
			}
			current, err := d.store.Lanes().List(runCtx, store.LaneFilter{RunID: r.ID})
			if err == nil {
				snapshot, _ := json.Marshal(current)
				if !bytes.Equal(previous, snapshot) {
					err = checkpoint(r.Dir, current)
					if err == nil {
						previous = snapshot
					}
				}
			}
			if err != nil {
				slog.Error("adopted controller checkpoint", "run", r.ID, "error", err)
			}
			select {
			case <-runCtx.Done():
				return
			case <-cr.stop:
				return
			case <-ticker.C():
			}
		}
	}()
	return nil
}

func (d *Controllers) Stop(ctx context.Context, r Run) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	cr := d.runs[r.ID]
	if cr == nil {
		return nil
	}
	select {
	case <-cr.done:
	default:
		close(cr.stop)
	}
	<-cr.done
	cr.cancel()
	current, err := d.store.Lanes().List(ctx, store.LaneFilter{RunID: r.ID})
	if err != nil {
		return err
	}
	if err := checkpoint(r.Dir, current); err != nil {
		return err
	}
	if err := cr.controller.ReleaseLease(ctx); err != nil {
		return err
	}
	delete(d.runs, r.ID)
	return nil
}

// Close leaves the handover lease in place so another chat can resume it.
func (d *Controllers) Close(ctx context.Context) error {
	d.mu.Lock()
	var runs []Run
	for _, cr := range d.runs {
		runs = append(runs, cr.run)
	}
	d.mu.Unlock()
	var errs []error
	for _, r := range runs {
		errs = append(errs, d.Stop(ctx, r))
	}
	return errors.Join(errs...)
}

func controllerState(r Run) (recipe.Recipe, []lane.Lane, error) {
	b, err := os.ReadFile(filepath.Join(r.Dir, "state.json"))
	if err != nil {
		return recipe.Recipe{}, nil, err
	}
	var st struct {
		Tasks []struct {
			Sessions    map[string]string `json:"sessions"`
			ID          string            `json:"id"`
			PRHead      string            `json:"pr_head"`
			ApprovalSHA string            `json:"approval_sha"`
			MergeSHA    string            `json:"merge_sha"`
			Checks      string            `json:"checks"`
			BaseCommit  string            `json:"fork_sha"`
			Worktree    string            `json:"worktree"`
		} `json:"tasks"`
		Config struct {
			MergePolicy *recipe.MergePolicy `json:"merge_policy"`
			MergeRule   string              `json:"merge_rule"`
			Base        string              `json:"base"`
			Target      string              `json:"target"`
			Phases      []hostPhase         `json:"phases"`
			NoPR        bool                `json:"no_pr"`
		} `json:"config"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return recipe.Recipe{}, nil, err
	}
	defaultRecipe := recipe.Default()
	workflow := defaultRecipe.Clone()
	workflow.RunID = r.ID
	workflow.Name = "adopted"
	if st.Config.MergePolicy != nil {
		workflow.MergePolicy = *st.Config.MergePolicy
	}
	if st.Config.Target != "" {
		workflow.MergePolicy.TargetBranch = st.Config.Target
	}
	if !workflow.MergePolicy.Mode.Valid() {
		return workflow, nil, fmt.Errorf("invalid adopted merge mode %q", workflow.MergePolicy.Mode)
	}
	if len(st.Config.Phases) > 0 {
		workflow.Phases = nil
		for _, spec := range st.Config.Phases {
			ph, ok := defaultRecipe.Phase(spec.Name)
			if !ok {
				return workflow, nil, fmt.Errorf("unsupported host phase %q", spec.Name)
			}
			if len(spec.Chain) > 0 {
				ph.Agents = nil
				for _, a := range spec.Chain {
					ph.Agents = append(ph.Agents, a.agent())
				}
			} else if spec.Model != "" {
				ph.Agents = []recipe.AgentSpec{spec.hostAgent.agent()}
			}
			workflow.Phases = append(workflow.Phases, ph)
		}
	}
	if workflow.MergePolicy.Mode == recipe.MergeModeManual || workflow.MergePolicy.Mode == recipe.MergeModeNone {
		var phases []recipe.Phase
		for _, ph := range workflow.Phases {
			if ph.Name != recipe.PhaseIntegrate {
				phases = append(phases, ph)
			}
		}
		workflow.Phases = phases
	} else if _, ok := workflow.Phase(recipe.PhaseIntegrate); !ok {
		ph, _ := defaultRecipe.Phase(recipe.PhaseIntegrate)
		workflow.Phases = append(workflow.Phases, ph)
	}
	workflow.Transitions = nil
	for i := range workflow.Phases {
		ph := &workflow.Phases[i]
		to := recipe.TargetComplete
		if i+1 < len(workflow.Phases) {
			to = workflow.Phases[i+1].Name
		}
		workflow.Transitions = append(workflow.Transitions, recipe.Transition{From: ph.Name, To: to, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: to == recipe.PhaseIntegrate && workflow.MergePolicy.RequireCIGreen}})
		if ph.Name == recipe.PhaseReview || ph.Name == recipe.PhaseIntegrate {
			workflow.Transitions = append(workflow.Transitions, recipe.Transition{From: ph.Name, To: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, AdvanceRound: true})
		}
		if ph.Name == recipe.PhaseIntegrate {
			ph.BriefTemplate += fmt.Sprintf("\nRun merge policy: mode=%s; target=%s; script=%s; require_review_pass=%t; require_ci_green=%t. %s", workflow.MergePolicy.Mode, workflow.MergePolicy.TargetBranch, workflow.MergePolicy.Script, workflow.MergePolicy.RequireReviewPass, workflow.MergePolicy.RequireCIGreen, st.Config.MergeRule)
			if st.Config.NoPR {
				ph.BriefTemplate += "\nThis run uses branches without PRs; integrate the reviewed branch into the run target."
				ph.ExitContract.RequiresPR = false
			}
		}
	}
	var lanes []lane.Lane
	for i := range r.Lanes {
		imported := &r.Lanes[i]
		status := lane.Status(imported.Status)
		if !status.Valid() {
			return workflow, nil, fmt.Errorf("lane %s: unsupported host status %q", imported.ID, imported.Status)
		}
		ln := lane.Lane{ID: imported.ID, RunID: r.ID, Title: imported.Title, Branch: imported.Branch, Status: status, RecipeName: workflow.Name, RecipeVersion: 1, BaseBranch: st.Config.Base, Attempt: 1}
		ln.PR, _ = strconv.Atoi(imported.PR)
		for _, task := range st.Tasks {
			if task.ID == ln.ID {
				ln.PRHead, ln.ApprovalSHA, ln.MergeSHA, ln.Checks, ln.BaseCommit, ln.Worktree = task.PRHead, task.ApprovalSHA, task.MergeSHA, task.Checks, task.BaseCommit, task.Worktree
				for key, terminal := range task.Sessions {
					if id, ok := strings.CutPrefix(terminal, "nebula:"); ok {
						if ln.Terminals == nil {
							ln.Terminals = map[string]string{}
						}
						ln.Terminals[key] = id
					}
				}
			}
		}
		ln.RecordAttempt(imported.Phase, imported.Round, imported.Session, imported.Terminal)
		lanes = append(lanes, ln)
	}
	return workflow, lanes, nil
}

type hostAgent struct {
	Kind   string `json:"kind"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}
type hostPhase struct {
	hostAgent
	Name  string      `json:"name"`
	Chain []hostAgent `json:"chain"`
}

func (a hostAgent) agent() recipe.AgentSpec {
	kind := a.Kind
	if kind == "" {
		kind = "claude"
	}
	return recipe.AgentSpec{Harness: kind, Model: a.Model, Effort: a.Effort}
}

// checkpoint preserves host-only task/config fields and the historical terminal
// map. Spawn files let the resumed watcher attach to the phase orca launched.
func checkpoint(dir string, lanes []lane.Lane) error {
	lock, err := os.OpenFile(filepath.Join(dir, "state.json.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock host ledger: %w", err)
	}
	path := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	var tasks []map[string]json.RawMessage
	if err := json.Unmarshal(doc["tasks"], &tasks); err != nil {
		return err
	}
	for i := range lanes {
		ln := &lanes[i]
		for _, task := range tasks {
			var id string
			if err := json.Unmarshal(task["id"], &id); err != nil {
				return err
			}
			if id != ln.ID {
				continue
			}
			fields := map[string]any{"phase": ln.Phase, "round": ln.Round, "status": ln.Status, "nebula_session": ln.SessionID, "nebula_terminal": ln.TerminalID, "pr": ln.PR, "pr_head": ln.PRHead, "approval_sha": ln.ApprovalSHA, "merge_sha": ln.MergeSHA, "checks": ln.Checks, "fork_sha": ln.BaseCommit}
			for k, v := range fields {
				task[k], _ = json.Marshal(v)
			}
			var sessions map[string]string
			if raw := task["sessions"]; len(raw) > 0 {
				if err := json.Unmarshal(raw, &sessions); err != nil {
					return err
				}
			}
			if sessions == nil {
				sessions = map[string]string{}
			}
			for k, v := range ln.Terminals {
				sessions[k] = "nebula:" + v
			}
			task["sessions"], _ = json.Marshal(sessions)
			if ln.Phase != "" && ln.TerminalID != "" {
				spawnPath := filepath.Join(dir, ln.ID+"."+ln.Phase+".spawn.json")
				sp := map[string]json.RawMessage{}
				if raw, err := os.ReadFile(spawnPath); err == nil {
					if err := json.Unmarshal(raw, &sp); err != nil {
						return err
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				sp["session_id"], _ = json.Marshal(ln.SessionID)
				sp["terminal_id"], _ = json.Marshal(ln.TerminalID)
				spawn, _ := json.Marshal(sp)
				if err := writeAtomic(spawnPath, spawn); err != nil {
					return err
				}
			}
		}
	}
	doc["tasks"], _ = json.Marshal(tasks)
	b, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}
