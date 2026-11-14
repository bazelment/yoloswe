package adopt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

// JudgeFactory creates a dispatcher for an adopted run.
type JudgeFactory func(runID string, answer func(ctx context.Context, runID, askID, text string) error) controller.Judge

// escalationAskTTL bounds an unanswered escalation question.
const escalationAskTTL = 24 * time.Hour

// Controllers attaches host runs to orca's deterministic lane lifecycle.
type Controllers struct {
	ctx      context.Context
	store    store.Store
	driver   lane.LaneDriver
	clock    clock.Clock
	judges   JudgeFactory
	runs     map[string]*controlledRun
	starting map[string]*startup
	mu       sync.Mutex
}

type controlledRun struct {
	controller *controller.Controller
	cancel     context.CancelFunc
	done       chan struct{}
	run        Run
}

type startup struct {
	done chan struct{}
	err  error
}

func NewControllers(ctx context.Context, st store.Store, driver lane.LaneDriver, clk clock.Clock, judges JudgeFactory) *Controllers {
	return &Controllers{ctx: ctx, store: st, driver: driver, clock: clk, judges: judges, runs: map[string]*controlledRun{}, starting: map[string]*startup{}}
}

// Wake asks an adopted controller to process pending events immediately.
func (d *Controllers) Wake(runID string) {
	d.mu.Lock()
	cr := d.runs[runID]
	d.mu.Unlock()
	if cr != nil {
		cr.controller.Wake()
	}
}

// Active reports whether this process currently runs a controller for runID.
func (d *Controllers) Active(runID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	cr := d.runs[runID]
	if cr == nil {
		return false
	}
	select {
	case <-cr.done:
		return false
	default:
		return true
	}
}

func (d *Controllers) Start(ctx context.Context, r Run) (retErr error) {
	d.mu.Lock()
	if cr := d.runs[r.ID]; cr != nil {
		select {
		case <-cr.done:
			delete(d.runs, r.ID)
		default:
			d.mu.Unlock()
			return nil
		}
	}
	if s, ok := d.starting[r.ID]; ok {
		d.mu.Unlock()
		<-s.done
		return s.err
	}
	s := &startup{done: make(chan struct{})}
	d.starting[r.ID] = s
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.starting, r.ID)
		s.err = retErr
		close(s.done)
		d.mu.Unlock()
	}()
	journal, err := readJournal(filepath.Join(r.Dir, leaseFile))
	if err != nil {
		return err
	}
	generation := journal.Marker
	workflow, lanes, items, err := controllerState(r)
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
	workflow.Version = 0
	saved, err := d.store.Recipes().Save(ctx, workflow)
	if err != nil {
		return err
	}
	workflow = saved
	for i := range items {
		item := items[i]
		if err := d.store.Backlog().Add(ctx, item); err != nil {
			if !errors.Is(err, store.ErrAlreadyExists) {
				return err
			}
			if !resume {
				if err := d.store.Backlog().Update(ctx, item, controller.ActorController, "adopt host task"); err != nil {
					return err
				}
			}
		}
	}
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
		if ln.Status.Terminal() && ln.SessionID != "" {
			if releaser, ok := d.driver.(lane.SessionReleaser); ok {
				if err := releaser.ReleaseSession(ctx, ln.SessionID, true); err != nil {
					slog.Warn("release completed lane session during adoption", "session", ln.SessionID, "error", err)
				}
			}
		}
		if tracker, ok := d.driver.(interface{ TrackLane(lane.Lane) }); ok && !ln.Status.Terminal() {
			tracker.TrackLane(*ln)
		}
	}
	if err := d.store.Runs().UpdateObjective(ctx, r.ID, r.Goal, generation); err != nil {
		return err
	}
	var ctrl *controller.Controller
	var dispatch controller.Judge
	if d.judges != nil {
		dispatch = d.judges(r.ID, func(ctx context.Context, runID, askID, text string) error {
			if runID != r.ID {
				return fmt.Errorf("owner answer run %q does not match adopted run %q", runID, r.ID)
			}
			return ctrl.AnswerOwnerAskTextFromJudge(ctx, askID, text)
		})
	}
	ctrl = controller.New(controller.Config{RunID: r.ID, HolderID: holder}, d.store, d.driver, d.clock, d.judge(holder, dispatch))
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
	cr := &controlledRun{controller: ctrl, cancel: cancel, done: make(chan struct{}), run: r}
	d.mu.Lock()
	d.runs[r.ID] = cr
	d.mu.Unlock()
	started = true
	go func() {
		defer close(cr.done)
		defer cancel()
		if err := ctrl.Run(runCtx); err != nil && !errors.Is(err, controller.ErrControllerStopped) && runCtx.Err() == nil {
			slog.Error("adopted controller stopped", "run", r.ID, "error", err)
		}
	}()
	return nil
}

func (d *Controllers) Stop(ctx context.Context, r Run) error {
	d.mu.Lock()
	s := d.starting[r.ID]
	d.mu.Unlock()
	if s != nil {
		<-s.done
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cr := d.runs[r.ID]
	if cr == nil {
		return nil
	}
	cr.controller.Kill()
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

// judge runs the dispatcher and exposes unresolved escalations to the owner.
func (d *Controllers) judge(holder string, dispatch controller.Judge) controller.Judge {
	return controller.JudgeFunc(func(ctx context.Context, runID string, events []store.Event) error {
		if dispatch != nil {
			stop := d.keepLease(ctx, runID, holder)
			err := dispatch.Judge(ctx, runID, events)
			stop()
			if err != nil {
				slog.Error("adopted run dispatcher failed; asking the owner about its escalations", "run", runID, "error", err)
				return d.askOwnerOnEscalation(ctx, runID, events)
			}
		}
		return d.askOwnerOnEscalation(ctx, runID, events)
	})
}

func (d *Controllers) keepLease(ctx context.Context, runID, holder string) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	ticker := d.clock.NewTicker(controller.DefaultHeartbeatInterval)
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C():
				_, _ = d.store.Runs().HeartbeatLease(ctx, runID, holder, controller.DefaultLeaseTTL)
			}
		}
	}()
	return func() { close(stop); <-done }
}

// askOwnerOnEscalation creates one owner question per unresolved escalation.
func (d *Controllers) askOwnerOnEscalation(ctx context.Context, runID string, events []store.Event) error {
	var laneIDs []string
	for i := range events {
		if id := events[i].LaneID; id != "" && !slices.Contains(laneIDs, id) {
			laneIDs = append(laneIDs, id)
		}
	}
	if len(laneIDs) == 0 {
		return nil
	}
	dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		return err
	}
	now := d.clock.Now().UTC()
	asked := map[string]bool{}
	for i := range dirs {
		dir := &dirs[i]
		if strings.HasPrefix(dir.Ref, ownerAskRef) && (dir.ExpiresAt == nil || now.Before(*dir.ExpiresAt)) {
			asked[dir.Scope] = true
		}
	}
	var errs []error
	for _, id := range laneIDs {
		ln, err := d.store.Lanes().Get(ctx, runID, id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, err)
			}
			continue
		}
		if ln.Status != lane.StatusBlocked || asked[id] {
			continue
		}
		decs, err := d.store.Decisions().List(ctx, store.DecisionFilter{RunID: runID, LaneID: id})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var esc *store.Decision
		for j := len(decs) - 1; j >= 0 && esc == nil; j-- {
			if decs[j].Tool == controller.ToolLaneEscalate {
				esc = &decs[j]
			}
		}
		if esc == nil {
			continue
		}
		errs = append(errs, d.addEscalationAsk(ctx, runID, ln, esc, now))
	}
	return errors.Join(errs...)
}

const ownerAskRef = "owner_ask:"

func (d *Controllers) addEscalationAsk(ctx context.Context, runID string, ln *lane.Lane, esc *store.Decision, now time.Time) error {
	hold := "hold: keep " + ln.ID + " blocked"
	ask := controller.OwnerAsk{
		ID:       fmt.Sprintf("escalation-%s-%d", ln.ID, esc.Seq),
		RunID:    runID,
		LaneID:   ln.ID,
		Question: fmt.Sprintf("Lane %s escalated (%s). How should it continue?", ln.ID, esc.Reason),
		Options:  []string{fmt.Sprintf("rework: start swe round %d", ln.Round+1), "stop: drop " + ln.ID, hold},
		Default:  hold,
		Deadline: now.Add(escalationAskTTL),
	}
	// Keep the ask scoped to the blocked lane so its default cannot change lane state.
	if err := ask.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(ask)
	if err != nil {
		return err
	}
	err = d.store.Directives().Add(ctx, store.Directive{
		ID:        "ask-" + ask.ID,
		RunID:     runID,
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     ln.ID,
		Ref:       ownerAskRef + ask.ID,
		Text:      string(raw),
		ExpiresAt: &ask.Deadline,
		Status:    store.DirectiveActive,
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		return nil
	}
	return err
}

// Close stops controllers while leaving the handover lease for another chat.
func (d *Controllers) Close(ctx context.Context) error {
	d.mu.Lock()
	var starts []*startup
	for _, s := range d.starting {
		starts = append(starts, s)
	}
	d.mu.Unlock()
	for _, s := range starts {
		<-s.done
	}
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

func controllerState(r Run) (recipe.Recipe, []lane.Lane, []store.BacklogItem, error) {
	b, err := os.ReadFile(filepath.Join(r.Dir, "state.json"))
	if err != nil {
		return recipe.Recipe{}, nil, nil, err
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
			Brief       string            `json:"brief"`
			Priority    store.Priority    `json:"priority"`
			DependsOn   []string          `json:"depends_on"`
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
		return recipe.Recipe{}, nil, nil, err
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
		return workflow, nil, nil, fmt.Errorf("invalid adopted merge mode %q", workflow.MergePolicy.Mode)
	}
	if len(st.Config.Phases) > 0 {
		workflow.Phases = nil
		for _, spec := range st.Config.Phases {
			ph, ok := defaultRecipe.Phase(spec.Name)
			if !ok {
				return workflow, nil, nil, fmt.Errorf("unsupported host phase %q", spec.Name)
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
			rework := recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}
			if ph.Name == recipe.PhaseReview {
				// Keep adopted runs on the same review -> SWE round budget as
				// lanes spawned by orca. The ledger's imported round is the
				// current round and must be used by this condition.
				rework.MaxRound = 2
			}
			workflow.Transitions = append(workflow.Transitions, recipe.Transition{From: ph.Name, To: recipe.PhaseSWE, On: rework, AdvanceRound: true})
			if ph.Name == recipe.PhaseReview {
				workflow.Transitions = append(workflow.Transitions, recipe.Transition{From: ph.Name, To: recipe.TargetEscalate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 3}})
			}
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
	var items []store.BacklogItem
	for i := range r.Lanes {
		imported := &r.Lanes[i]
		if err := validatePathComponent("lane id", imported.ID); err != nil {
			return workflow, nil, nil, err
		}
		if imported.Phase != "" {
			if err := validatePathComponent("lane phase", imported.Phase); err != nil {
				return workflow, nil, nil, err
			}
		}
		status := lane.Status(imported.Status)
		if !status.Valid() {
			return workflow, nil, nil, fmt.Errorf("lane %s: unsupported host status %q", imported.ID, imported.Status)
		}
		ln := lane.Lane{ItemID: imported.ID, ID: imported.ID, RunID: r.ID, Title: imported.Title, Branch: imported.Branch, Status: status, RecipeName: workflow.Name, RecipeVersion: 1, BaseBranch: st.Config.Base, Attempt: 1}
		ln.PR, _ = strconv.Atoi(imported.PR)
		for j := range st.Tasks {
			task := &st.Tasks[j]

			if task.ID == ln.ID {
				mission, err := hostMission(r.Dir, task.ID, task.Brief)
				if err != nil {
					return workflow, nil, nil, err
				}
				priority := task.Priority
				if !priority.Valid() {
					priority = store.PriorityP2
				}
				status := store.BacklogInProgress
				switch ln.Status {
				case lane.StatusDone:
					status = store.BacklogDone
				case lane.StatusFailed:
					status = store.BacklogDropped
				case lane.StatusPlanned:
					status = store.BacklogOpen
				case lane.StatusBlocked:
					status = store.BacklogBlocked
				}
				items = append(items, store.BacklogItem{ID: ln.ID, RunID: r.ID, LaneID: ln.ID, Title: ln.Title, Description: mission, Priority: priority, Status: status, DependsOn: task.DependsOn})
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
	return workflow, lanes, items, nil
}

func hostMission(dir, id, brief string) (string, error) {
	if err := validatePathComponent("task id", id); err != nil {
		return "", err
	}
	paths := []string{filepath.Join(dir, "missions", id+".md"), filepath.Join(dir, "briefs", id+".mission.md")}
	if brief != "" {
		resolved, err := resolveBriefPath(dir, brief)
		if err != nil {
			return "", err
		}
		paths = append(paths, resolved)
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err == nil {
			return string(b), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

// validatePathComponent keeps ledger-controlled names from escaping the run
// directory when they are used to construct mission and spawn-file paths.
func validatePathComponent(label, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("%s %q must be a single path component inside the run directory", label, value)
	}
	return nil
}

// resolveBriefPath permits imported task briefs only when their lexical and
// resolved paths remain inside the host run directory. Ledger data is
// untrusted: an absolute path or traversal must never become model context.
func resolveBriefPath(dir, brief string) (string, error) {
	if filepath.IsAbs(brief) {
		return "", fmt.Errorf("brief path %q must be inside the run directory", brief)
	}
	clean := filepath.Clean(brief)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("brief path %q escapes the run directory", brief)
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve run directory for brief path %q: %w", brief, err)
	}
	candidate := filepath.Join(base, clean)
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		resolvedBase, baseErr := filepath.EvalSymlinks(base)
		if baseErr != nil {
			return "", fmt.Errorf("resolve run directory for brief path %q: %w", brief, baseErr)
		}
		rel, relErr := filepath.Rel(resolvedBase, resolved)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("brief path %q escapes the run directory", brief)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve brief path %q: %w", brief, err)
	}
	return candidate, nil
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

// checkpoint writes controller state back to the host ledger and spawn files.
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
		if err := validatePathComponent("lane id", ln.ID); err != nil {
			return err
		}
		if ln.Phase != "" {
			if err := validatePathComponent("lane phase", ln.Phase); err != nil {
				return err
			}
		}
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
