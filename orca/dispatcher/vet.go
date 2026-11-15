package dispatcher

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

// Canonical tool names exposed to the Dispatcher model.
const (
	ToolBacklogAdd      = "backlog.add"
	ToolBacklogUpdate   = "backlog.update"
	ToolBacklogReorder  = "backlog.reorder"
	ToolBacklogClose    = "backlog.close"
	ToolLaneSpawn       = "lane.spawn"
	ToolLaneSteer       = "lane.steer"
	ToolLaneRetry       = "lane.retry"
	ToolLaneStop        = "lane.stop"
	ToolLaneWait        = "lane.wait"
	ToolOwnerAsk        = "owner.ask"
	ToolOwnerAnswer     = "owner.answer"
	ToolMailAnswer      = "mail.answer"
	ToolMailDismiss     = "mail.dismiss"
	ToolDirectiveNote   = "directive.note"
	ToolDirectiveRevoke = "directive.revoke"
	ToolPlannerRequest  = "planner.request"
	ToolLaneForward     = "lane.forward"
	ToolReadLane        = "read.lane"
	ToolLaneInspect     = ToolReadLane
	ToolRunCloseout     = "run.closeout"
	ToolReadLaneReport  = "read.lane_report"
	ToolReadPR          = "read.pr"
	ToolReadCI          = "read.ci"
)

// DefaultAllowedModels lists the default worker model identifiers and policy
// tier aliases accepted on lane.spawn and lane.retry when Config.AllowedModels
// is not explicitly overridden.
var DefaultAllowedModels = []string{
	"strongest",
	"mid",
	"cheaper",
	"fastest",
	"opus",
	"sonnet",
	"haiku",
	"claude",
	"codex",
	"agy",
	"carbon",
	"agy-carbon",
	"agy carbon",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-opus-5-5",
	"claude-sonnet-5-5",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5-20250929",
	"claude-haiku-5-5",
	"claude-haiku-4-5",
	"claude-haiku-4-5-20251001",
	"gpt-6.1-sol",
	"gpt-6-sol",
	"gpt-6-astra",
	"gpt-5.6-sol",
	"gpt-5.4",
	"gpt-5.4-mini",
}

// DefaultMaxBriefBytes is the fallback brief/note size cap when a recipe does
// not specify Limits.MaxBriefBytes.
const DefaultMaxBriefBytes = 16 * 1024

// BacklogAddParams holds parameters for the backlog.add tool.
type BacklogAddParams struct {
	ID          string   `json:"id" jsonschema:"required,description=Unique backlog item ID"`
	Title       string   `json:"title" jsonschema:"required,description=Short title of the work item"`
	Description string   `json:"description,omitempty" jsonschema:"description=Detailed description of the work"`
	Acceptance  string   `json:"acceptance,omitempty" jsonschema:"description=Acceptance criteria"`
	Priority    string   `json:"priority,omitempty" jsonschema:"enum=p0,enum=p1,enum=p2,enum=p3,description=Item priority (p0..p3)"`
	RecipeName  string   `json:"recipe_name,omitempty" jsonschema:"description=Optional recipe name for this item"`
	Reason      string   `json:"reason,omitempty" jsonschema:"description=Reason for adding this item"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"description=IDs of backlog items this item depends on"`
	Tags        []string `json:"tags,omitempty" jsonschema:"description=Optional classification tags"`
	Order       int      `json:"order,omitempty" jsonschema:"description=Explicit sort order within priority"`
	NoMerge     bool     `json:"no_merge,omitempty" jsonschema:"description=Optional: true when this is a report-only or no-merge task that must stop after review and never run integrate"`
}

// BacklogUpdateParams holds parameters for the backlog.update tool.
type BacklogUpdateParams struct {
	NoMerge     *bool    `json:"no_merge,omitempty" jsonschema:"description=Optional: true when this is a report-only or no-merge task that must stop after review and never run integrate"`
	ID          string   `json:"id" jsonschema:"required,description=Backlog item ID to update"`
	Title       string   `json:"title,omitempty" jsonschema:"description=Updated title"`
	Description string   `json:"description,omitempty" jsonschema:"description=Updated description"`
	Acceptance  string   `json:"acceptance,omitempty" jsonschema:"description=Updated acceptance criteria"`
	Priority    string   `json:"priority,omitempty" jsonschema:"enum=p0,enum=p1,enum=p2,enum=p3,description=Updated priority"`
	Status      string   `json:"status,omitempty" jsonschema:"enum=open,enum=blocked,description=Updated status. Never in_progress: lane.spawn sets that itself. Use backlog.close for done/dropped"`
	RecipeName  string   `json:"recipe_name,omitempty" jsonschema:"description=Updated recipe name"`
	Reason      string   `json:"reason" jsonschema:"required,description=Reason for updating this item"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"description=Replacement dependency IDs"`
	Tags        []string `json:"tags,omitempty" jsonschema:"description=Replacement tags"`
}

// BacklogReorderParams holds parameters for the backlog.reorder tool.
type BacklogReorderParams struct {
	Reason  string   `json:"reason,omitempty" jsonschema:"description=Reason for reordering"`
	ItemIDs []string `json:"item_ids" jsonschema:"required,description=Ordered backlog item IDs"`
}

// BacklogCloseParams holds parameters for the backlog.close tool.
type BacklogCloseParams struct {
	ID     string `json:"id" jsonschema:"required,description=Backlog item ID to close"`
	Status string `json:"status" jsonschema:"required,enum=done,enum=dropped,description=Terminal status (done or dropped)"`
	Reason string `json:"reason" jsonschema:"required,description=Reason for closing this item"`
}

// LaneSpawnParams holds parameters for the lane.spawn tool.
type LaneSpawnParams struct {
	ItemID         string `json:"item_id" jsonschema:"required" jsonschema_description:"ID of an open backlog item marked ready in the snapshot. Spawning marks it in_progress itself, so do not call backlog.update first"`
	LaneID         string `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID (defaults to item_id)"`
	Brief          string `json:"brief" jsonschema:"required,description=Task brief for the lane"`
	RecipeName     string `json:"recipe_name,omitempty" jsonschema:"description=Optional recipe name"`
	RecipeOverride string `json:"recipe_override,omitempty" jsonschema:"description=Optional natural-language recipe override or recipe name for this lane"`
	Branch         string `json:"branch,omitempty" jsonschema:"description=Optional git branch name"`
	BaseBranch     string `json:"base_branch,omitempty" jsonschema:"description=Optional base branch name"`
	Reason         string `json:"reason,omitempty" jsonschema:"description=Reason for spawning this lane"`
	Model          string `json:"model,omitempty" jsonschema:"description=Optional worker model override from the allowed list"`
	NoMerge        bool   `json:"no_merge,omitempty" jsonschema:"description=Optional: true when this lane is a report-only or no-merge task that must stop after review and never run integrate"`
}

// LaneSteerParams holds parameters for the lane.steer tool.
type LaneSteerParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to steer"`
	Note   string `json:"note" jsonschema:"required,description=Steering instruction to inject into the lane"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Reason for steering this lane"`
}

// LaneForwardParams holds parameters for the lane.forward tool.
type LaneForwardParams struct {
	FromLaneID string `json:"from_lane_id,omitempty" jsonschema:"description=Source lane ID whose finding or report excerpt is being forwarded"`
	ToLaneID   string `json:"to_lane_id" jsonschema:"required,description=Target live lane ID to receive the forwarded finding as a steer"`
	Excerpt    string `json:"excerpt" jsonschema:"required,description=Finding or report excerpt to forward to the target lane"`
	Reason     string `json:"reason,omitempty" jsonschema:"description=Why this finding affects the target lane"`
}

// LaneRetryParams holds parameters for the lane.retry tool.
type LaneRetryParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to retry"`
	Phase  string `json:"phase,omitempty" jsonschema:"description=Optional phase to retry (defaults to current lane phase or recipe first phase)"`
	Note   string `json:"note,omitempty" jsonschema:"description=Optional additional brief or guidance for the retry"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Reason for retrying this lane"`
	Model  string `json:"model,omitempty" jsonschema:"description=Optional worker model override from the allowed list"`
	Round  int    `json:"round,omitempty" jsonschema:"description=Target round number (must be > max recorded round for the phase; defaults to next unused round)"`
}

// LaneStopParams holds parameters for the lane.stop tool.
type LaneStopParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to stop"`
	Reason string `json:"reason" jsonschema:"required,description=Reason for stopping the lane"`
}

// LaneWaitParams holds parameters for the lane.wait tool, recording a named
// expected wait on a lane so the controller suppresses stall judgments until
// the deadline.
type LaneWaitParams struct {
	LaneID   string `json:"lane_id" jsonschema:"required,description=Lane ID performing long external work"`
	Reason   string `json:"reason" jsonschema:"required,description=Named reason for the expected wait (e.g. cloud VM run, sweep, or merge gate)"`
	Deadline string `json:"deadline" jsonschema:"required,description=Wait deadline as RFC3339 timestamp, positive duration (e.g. 45m, 2h), or UTC clock time"`
}

// OwnerAskParams holds parameters for the owner.ask tool.
type OwnerAskParams struct {
	ID        string   `json:"id,omitempty" jsonschema:"description=Optional unique ask ID"`
	LaneID    string   `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID blocked on this question"`
	MessageID string   `json:"message_id,omitempty" jsonschema:"description=Optional inbound mail message ID this ask escalates"`
	Question  string   `json:"question" jsonschema:"required,description=One-sentence decision question for the owner"`
	Default   string   `json:"default" jsonschema:"required" jsonschema_description:"Option auto-applied at deadline. If it is not already one of options it is added as an option, so a card's own stated default can be passed as is"`
	Deadline  string   `json:"deadline" jsonschema:"required" jsonschema_description:"Deadline as RFC3339 timestamp, positive duration (30m, 2h, 7d) or UTC clock time (05:30Z = next occurrence)"`
	SteerNote string   `json:"steer_note,omitempty" jsonschema:"description=Optional steering note to send when the default is applied"`
	Reason    string   `json:"reason,omitempty" jsonschema:"description=Why owner input is required"`
	Options   []string `json:"options" jsonschema:"required" jsonschema_description:"Concrete options the owner can choose from: short labels, a few words each (put the detail in the question)"`
}

type OwnerAnswerParams struct {
	AskID  string `json:"ask_id" jsonschema:"required,description=ID of an open owner ask in the snapshot"`
	Answer string `json:"answer" jsonschema:"required,description=The owner's answer to the ask"`
}

// MailAnswerParams holds parameters for the mail.answer tool.
type MailAnswerParams struct {
	MessageID string `json:"message_id,omitempty" jsonschema:"description=Inbound mail message ID to answer or reply to"`
	LaneID    string `json:"lane_id,omitempty" jsonschema:"description=Lane ID that sent the mail"`
	Answer    string `json:"answer" jsonschema:"required,description=Answer or reply body to send on the lane mail message"`
	Reason    string `json:"reason,omitempty" jsonschema:"description=Optional reason for this answer"`
}

// MailDismissParams holds parameters for the mail.dismiss tool.
type MailDismissParams struct {
	MessageID string `json:"message_id,omitempty" jsonschema:"description=Inbound mail message ID to dismiss or close as stale/withdrawn/obsolete"`
	LaneID    string `json:"lane_id,omitempty" jsonschema:"description=Lane ID that sent the mail"`
	Reason    string `json:"reason" jsonschema:"required,description=Reason why the lane question or mail is stale, withdrawn, or obsolete"`
}

// DirectiveNoteParams holds parameters for the directive.note tool.
type DirectiveNoteParams struct {
	ID       string `json:"id,omitempty" jsonschema:"description=Optional directive ID"`
	Fact     string `json:"fact" jsonschema:"required,description=Fact or operational note to persist in the store"`
	Kind     string `json:"-" jsonschema:"-"`
	Expiry   string `json:"expiry,omitempty" jsonschema_description:"Optional expiry as RFC3339 timestamp, duration (24h, 7d) or UTC clock time (05:30Z = next occurrence)"`
	Scope    string `json:"scope,omitempty" jsonschema:"description=Optional lane ID or scope"`
	Reason   string `json:"reason,omitempty" jsonschema:"description=Reason for recording this note"`
	Contract bool   `json:"contract,omitempty" jsonschema:"description=Optional: true to record this directive in the shared run contract"`
}

// DirectiveRevokeParams holds parameters for the directive.revoke tool.
type DirectiveRevokeParams struct {
	ID     string `json:"id" jsonschema:"required,description=ID of an active directive in the snapshot to revoke or supersede"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Reason for revoking or superseding this directive"`
}

// PlannerRequestParams holds parameters for the planner.request tool.
type PlannerRequestParams struct {
	Reason  string `json:"reason" jsonschema:"required,description=Why replanning or plan reconciliation is needed"`
	PlanRef string `json:"plan_ref,omitempty" jsonschema:"description=Optional Nebula plan post reference"`
	Urgent  bool   `json:"urgent,omitempty" jsonschema:"description=Whether replanning is urgent"`
}

// ReadLaneParams holds parameters for the read.lane inspection tool.
type ReadLaneParams struct {
	LaneID   string   `json:"lane_id" jsonschema:"required,description=Lane ID to inspect (from the snapshot Lanes table)"`
	Path     string   `json:"path,omitempty" jsonschema:"description=Relative file or directory path to read or list in the lane worktree, run dir, or pushed branch (defaults to '.' when no program is set)"`
	Program  string   `json:"program,omitempty" jsonschema:"description=Optional read-only command to run in the lane session/branch (git, rg, grep, sha256sum, shasum, jq, wc, ls, cat, head, tail, stat)"`
	Command  string   `json:"command,omitempty" jsonschema:"description=Optional read-only command line (alias for program + args)"`
	Ref      string   `json:"ref,omitempty" jsonschema:"description=Optional git ref or branch to inspect when reading from the lane branch"`
	Reason   string   `json:"reason,omitempty" jsonschema:"description=Optional reason for inspecting raw lane evidence"`
	Args     []string `json:"args,omitempty" jsonschema:"description=Arguments for program when running a read-only command"`
	MaxBytes int      `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes to return"`
}

// RunCloseoutParams holds parameters for the run.closeout tool, which closes
// out an adopted run once every lane and backlog item is terminal.
type RunCloseoutParams struct {
	Files            map[string]string `json:"files,omitempty" jsonschema:"description=Additional close-out file updates inside the run dir for files named in OBJECTIVE.md"`
	Outcome          string            `json:"outcome,omitempty" jsonschema:"description=Final run outcome (defaults to done)"`
	Summary          string            `json:"summary,omitempty" jsonschema:"description=Final summary for FINAL.md"`
	FinalMarkdown    string            `json:"final_md,omitempty" jsonschema:"description=Explicit markdown body to write to FINAL.md"`
	ProgressMarkdown string            `json:"progress_md,omitempty" jsonschema:"description=Markdown summary to append to PROGRESS.md"`
	Next             string            `json:"next,omitempty" jsonschema:"description=Replacement Next: line for OBJECTIVE.md"`
	PlanFile         string            `json:"plan_file,omitempty" jsonschema:"description=Plan file named in OBJECTIVE.md to append a completion log entry to (e.g. PLAN.md)"`
	PlanLogLine      string            `json:"plan_log_line,omitempty" jsonschema:"description=Log line to append to the plan file named in OBJECTIVE.md"`
	PostID           string            `json:"post_id,omitempty" jsonschema:"description=Nebula post ID named in OBJECTIVE.md to republish/update"`
	PostContent      string            `json:"post_content,omitempty" jsonschema:"description=Updated markdown content for the Nebula post"`
	Reason           string            `json:"reason,omitempty" jsonschema:"description=Reason for closing out the run"`
}

// ReadLaneReportParams holds parameters for the read.lane_report tool.
type ReadLaneReportParams struct {
	LaneID     string `json:"lane_id" jsonschema:"required,description=Lane ID whose phase report to read"`
	TerminalID string `json:"terminal_id,omitempty" jsonschema:"description=Optional terminal ID"`
	Reason     string `json:"reason,omitempty" jsonschema:"description=Optional reason for reading the report"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes to return"`
}

// ReadPRParams holds parameters for the read.pr tool.
type ReadPRParams struct {
	LaneID   string `json:"lane_id" jsonschema:"required,description=ID of a lane listed in the snapshot Lanes table (never a placeholder such as '-')"`
	Reason   string `json:"reason,omitempty" jsonschema:"description=Optional reason for reading PR info"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes to return"`
}

// ReadCIParams holds parameters for the read.ci tool.
type ReadCIParams struct {
	LaneID   string `json:"lane_id" jsonschema:"required,description=ID of a lane listed in the snapshot Lanes table (never a placeholder such as '-')"`
	Reason   string `json:"reason,omitempty" jsonschema:"description=Optional reason for reading CI info"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes to return"`
}

// ProposedAction is one typed tool call proposed by the Dispatcher model; it must
// pass Vet before being applied.
type ProposedAction struct {
	BacklogAdd      *BacklogAddParams      `json:"-"`
	BacklogUpdate   *BacklogUpdateParams   `json:"-"`
	BacklogReorder  *BacklogReorderParams  `json:"-"`
	BacklogClose    *BacklogCloseParams    `json:"-"`
	LaneSpawn       *LaneSpawnParams       `json:"-"`
	LaneSteer       *LaneSteerParams       `json:"-"`
	LaneForward     *LaneForwardParams     `json:"-"`
	LaneRetry       *LaneRetryParams       `json:"-"`
	LaneStop        *LaneStopParams        `json:"-"`
	LaneWait        *LaneWaitParams        `json:"-"`
	OwnerAsk        *OwnerAskParams        `json:"-"`
	OwnerAnswer     *OwnerAnswerParams     `json:"-"`
	MailAnswer      *MailAnswerParams      `json:"-"`
	MailDismiss     *MailDismissParams     `json:"-"`
	DirectiveNote   *DirectiveNoteParams   `json:"-"`
	DirectiveRevoke *DirectiveRevokeParams `json:"-"`
	PlannerRequest  *PlannerRequestParams  `json:"-"`
	ReadLane        *ReadLaneParams        `json:"-"`
	RunCloseout     *RunCloseoutParams     `json:"-"`
	ReadLaneReport  *ReadLaneReportParams  `json:"-"`
	ReadPR          *ReadPRParams          `json:"-"`
	ReadCI          *ReadCIParams          `json:"-"`
	ID              string                 `json:"id,omitempty"`
	Tool            string                 `json:"tool"`
	LaneID          string                 `json:"lane_id,omitempty"`
	ItemID          string                 `json:"item_id,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
	Args            json.RawMessage        `json:"args,omitempty"`
}

// Target returns a human-readable identifier (lane ID, item ID, or "run") for
// formatting rejection messages.
func (a ProposedAction) Target() string {
	return cmp.Or(a.LaneID, a.ItemID, "run")
}

// Proposal is a batch of ProposedActions returned by an Advisor turn.
type Proposal struct {
	Unresolved string           `json:"unresolved,omitempty"`
	Actions    []ProposedAction `json:"actions"`
}

// Advisor proposes orchestration actions from a rendered Store snapshot.
type Advisor interface {
	Propose(ctx context.Context, snapshot string) (Proposal, error)
}

// NormalizeToolName maps underscore tool aliases (e.g. "lane_spawn") to the
// canonical dotted tool names ("lane.spawn").
func NormalizeToolName(name string) string {
	trimmed := strings.TrimSpace(name)
	switch trimmed {
	case "directive.supersede", "directive_supersede":
		return ToolDirectiveRevoke
	case "mail.resolve", "mail_resolve", "worker.question.dismiss", "question.dismiss", "mail_dismiss":
		return ToolMailDismiss
	case "lane.inspect", "lane_inspect", "read.lane_inspect", "read_lane_inspect", "read_lane":
		return ToolReadLane
	case "lane_forward", "forward", "lane.forward_finding", "lane_forward_finding":
		return ToolLaneForward
	case "lane.expected_wait", "lane_expected_wait", "expected_wait", "lane_wait":
		return ToolLaneWait
	}
	if isKnownTool(trimmed) {
		return trimmed
	}
	if candidate := strings.Replace(trimmed, "_", ".", 1); isKnownTool(candidate) {
		return candidate
	}
	return trimmed
}

func isKnownTool(tool string) bool {
	switch tool {
	case ToolBacklogAdd, ToolBacklogUpdate, ToolBacklogReorder, ToolBacklogClose,
		ToolLaneSpawn, ToolLaneSteer, ToolLaneForward, ToolLaneRetry, ToolLaneStop, ToolLaneWait,
		ToolOwnerAsk, ToolOwnerAnswer, ToolMailAnswer, ToolMailDismiss, ToolDirectiveNote, ToolDirectiveRevoke, ToolPlannerRequest, ToolRunCloseout,
		ToolReadLane, ToolReadLaneReport, ToolReadPR, ToolReadCI:
		return true
	default:
		return false
	}
}

type jsonAliases struct {
	ID           string `json:"id"`
	MsgID        string `json:"msg_id"`
	Item         string `json:"item"`
	Lane         string `json:"lane"`
	FromLane     string `json:"from_lane"`
	SourceLane   string `json:"source_lane"`
	SourceLaneID string `json:"source_lane_id"`
	ToLane       string `json:"to_lane"`
	TargetLane   string `json:"target_lane"`
	TargetLaneID string `json:"target_lane_id"`
	Recipe       string `json:"recipe"`
	Message      string `json:"message"`
	Body         string `json:"body"`
	Note         string `json:"note"`
	Finding      string `json:"finding"`
	Brief        string `json:"brief"`
	Text         string `json:"text"`
	File         string `json:"file"`
	Cmd          string `json:"cmd"`
	WorkerModel  string `json:"worker_model"`
	Kind         string `json:"kind"`
	ExpiresAt    string `json:"expires_at"`
	DirectiveID  string `json:"directive_id"`
}

func decodeAliases(raw []byte) jsonAliases {
	var a jsonAliases
	_ = json.Unmarshal(raw, &a)
	return a
}

func decodeParams[T any](tool string, raw []byte) (*T, error) {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode %s: %w", tool, err)
	}
	return &p, nil
}

// ParseAction decodes raw tool call JSON arguments into a typed ProposedAction.
func ParseAction(callID, toolName string, raw json.RawMessage) (ProposedAction, error) {
	canonical := NormalizeToolName(toolName)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = []byte("{}")
	}
	act := ProposedAction{
		ID:   callID,
		Tool: canonical,
		Args: slices.Clone(trimmed),
	}

	switch canonical {
	case ToolBacklogAdd:
		p, err := decodeParams[BacklogAddParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.BacklogAdd = p
		act.ItemID = p.ID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("add backlog item %s", p.ID))

	case ToolBacklogUpdate:
		p, err := decodeParams[BacklogUpdateParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.BacklogUpdate = p
		act.ItemID = p.ID
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolBacklogReorder:
		p, err := decodeParams[BacklogReorderParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.BacklogReorder = p
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), "reorder backlog items")

	case ToolBacklogClose:
		p, err := decodeParams[BacklogCloseParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.BacklogClose = p
		act.ItemID = p.ID
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolLaneSpawn:
		p, err := decodeParams[LaneSpawnParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.ItemID == "" || p.LaneID == "" || p.Model == "" || (p.RecipeOverride == "" && p.RecipeName == "") {
			a := decodeAliases(trimmed)
			p.ItemID = cmp.Or(p.ItemID, a.Item)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Model = cmp.Or(p.Model, a.WorkerModel)
			if p.RecipeOverride == "" && p.RecipeName == "" {
				p.RecipeOverride = a.Recipe
			}
		}
		act.LaneSpawn = p
		act.ItemID = p.ItemID
		act.LaneID = cmp.Or(strings.TrimSpace(p.LaneID), strings.TrimSpace(p.ItemID))
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("spawn lane %s for item %s", act.LaneID, p.ItemID))

	case ToolLaneSteer:
		p, err := decodeParams[LaneSteerParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" || p.Note == "" {
			a := decodeAliases(trimmed)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Note = cmp.Or(p.Note, a.Message)
		}
		act.LaneSteer = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Note))

	case ToolLaneForward:
		p, err := decodeParams[LaneForwardParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.FromLaneID == "" || p.ToLaneID == "" || p.Excerpt == "" {
			a := decodeAliases(trimmed)
			p.FromLaneID = cmp.Or(p.FromLaneID, a.FromLane, a.SourceLaneID, a.SourceLane)
			p.ToLaneID = cmp.Or(p.ToLaneID, a.ToLane, a.TargetLaneID, a.TargetLane, a.Lane)
			p.Excerpt = cmp.Or(p.Excerpt, a.Finding, a.Note, a.Message, a.Body, a.Text)
		}
		act.LaneForward = p
		act.LaneID = strings.TrimSpace(p.ToLaneID)
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Excerpt))

	case ToolLaneRetry:
		p, err := decodeParams[LaneRetryParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" || p.Note == "" || p.Model == "" {
			a := decodeAliases(trimmed)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Note = cmp.Or(p.Note, a.Brief)
			p.Model = cmp.Or(p.Model, a.WorkerModel)
		}
		act.LaneRetry = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Note), fmt.Sprintf("retry lane %s", p.LaneID))

	case ToolLaneStop:
		p, err := decodeParams[LaneStopParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		act.LaneStop = p
		act.LaneID = p.LaneID
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolLaneWait:
		p, err := decodeParams[LaneWaitParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" || p.Reason == "" || p.Deadline == "" {
			a := decodeAliases(trimmed)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Reason = cmp.Or(p.Reason, a.Note, a.Text, a.Message)
			p.Deadline = cmp.Or(p.Deadline, a.ExpiresAt)
		}
		act.LaneWait = p
		act.LaneID = p.LaneID
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolOwnerAsk:
		p, err := decodeParams[OwnerAskParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" || p.MessageID == "" {
			a := decodeAliases(trimmed)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.MessageID = cmp.Or(p.MessageID, a.MsgID)
		}
		p.Options, p.Default = canonicalizeAskDefault(p.Options, p.Default)
		act.OwnerAsk = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Question))

	case ToolOwnerAnswer:
		p, err := decodeParams[OwnerAnswerParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.OwnerAnswer = p
		act.Reason = strings.TrimSpace(p.Answer)

	case ToolMailAnswer:
		p, err := decodeParams[MailAnswerParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.MessageID == "" || p.LaneID == "" || p.Answer == "" {
			a := decodeAliases(trimmed)
			p.MessageID = cmp.Or(p.MessageID, a.ID, a.MsgID)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Answer = cmp.Or(p.Answer, a.Body, a.Note, a.Message, a.Text)
		}
		act.MailAnswer = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Answer))

	case ToolMailDismiss:
		p, err := decodeParams[MailDismissParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.MessageID == "" || p.LaneID == "" || p.Reason == "" {
			a := decodeAliases(trimmed)
			p.MessageID = cmp.Or(p.MessageID, a.ID, a.MsgID)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Reason = cmp.Or(p.Reason, a.Body, a.Note, a.Message, a.Text)
		}
		act.MailDismiss = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("dismiss lane mail %s", cmp.Or(p.MessageID, p.LaneID)))

	case ToolDirectiveNote:
		p, err := decodeParams[DirectiveNoteParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.Fact == "" || p.Expiry == "" || p.Kind == "" {
			a := decodeAliases(trimmed)
			p.Fact = cmp.Or(p.Fact, a.Text)
			p.Expiry = cmp.Or(p.Expiry, a.ExpiresAt)
			p.Kind = cmp.Or(p.Kind, a.Kind)
		}
		if strings.EqualFold(strings.TrimSpace(p.Kind), string(store.DirectiveContract)) ||
			strings.EqualFold(strings.TrimSpace(p.Scope), string(store.DirectiveContract)) {
			p.Contract = true
		}
		act.DirectiveNote = p
		act.LaneID = p.Scope
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Fact))

	case ToolDirectiveRevoke:
		p, err := decodeParams[DirectiveRevokeParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.ID == "" {
			p.ID = decodeAliases(trimmed).DirectiveID
		}
		p.ID = store.NormalizeDirectiveID(p.ID)
		act.DirectiveRevoke = p
		act.ItemID = p.ID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("revoke directive %s", p.ID))

	case ToolPlannerRequest:
		p, err := decodeParams[PlannerRequestParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.PlannerRequest = p
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolReadLane:
		p, err := decodeParams[ReadLaneParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" || p.Path == "" || p.Command == "" {
			a := decodeAliases(trimmed)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Path = cmp.Or(p.Path, a.File)
			p.Command = cmp.Or(p.Command, a.Cmd)
		}
		act.ReadLane = p
		act.LaneID = strings.TrimSpace(p.LaneID)
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("inspect lane %s", p.LaneID))

	case ToolRunCloseout:
		p, err := decodeParams[RunCloseoutParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.RunCloseout = p
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Summary), "close out run")

	case ToolReadLaneReport:
		p, err := decodeParams[ReadLaneReportParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		act.ReadLaneReport = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("read lane report for %s", p.LaneID))

	case ToolReadPR:
		p, err := decodeParams[ReadPRParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		act.ReadPR = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("read PR info for %s", p.LaneID))

	case ToolReadCI:
		p, err := decodeParams[ReadCIParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		act.ReadCI = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("read CI status for %s", p.LaneID))
	}
	return act, nil
}

// StateView holds the verified Store state inspected by Invariant checks.
//
//nolint:govet
type StateView struct {
	Now            time.Time
	Run            *store.Run
	Lanes          map[string]lane.Lane
	Backlog        map[string]store.BacklogItem
	Recipes        map[string]recipe.Recipe
	VersionRecipes map[string]recipe.Recipe
	MaxBriefBytes  int
	Memories       []store.MemoryEntry
	AllowedModels  []string
	OwnerAsks      map[string]controller.OwnerAsk
	Directives     map[string]store.Directive
	Settings       store.RunSettings
}

// LoadStateView loads the current Run, Lanes, Backlog, and Recipes from st into
// a StateView for invariant vetting.
func LoadStateView(
	ctx context.Context,
	st store.Store,
	clk clock.Clock,
	runID string,
	maxBriefBytes int,
) (*StateView, error) {
	return loadStateViewWithCache(ctx, st, clk, runID, maxBriefBytes, nil)
}

func loadStateViewWithCache(
	ctx context.Context,
	st store.Store,
	clk clock.Clock,
	runID string,
	maxBriefBytes int,
	pinnedCache map[string]recipe.Recipe,
) (*StateView, error) {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	lanes, err := st.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	backlog, err := st.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	latestRecipes, err := st.Recipes().ListLatest(ctx, runID)
	if err != nil {
		return nil, err
	}
	now := clk.Now().UTC()
	memories, _ := st.Memories().List(ctx, store.MemoryFilter{RunID: runID})
	memories = store.FilterActiveMemories(ctx, st, runID, memories, now)
	directives, err := store.ListActiveDirectives(ctx, st, runID, now)
	if err != nil {
		return nil, err
	}
	settings, _ := st.Runs().GetSettings(ctx, runID)

	view := &StateView{
		Now:            now,
		Run:            run,
		Lanes:          make(map[string]lane.Lane, len(lanes)),
		Backlog:        make(map[string]store.BacklogItem, len(backlog)),
		Recipes:        make(map[string]recipe.Recipe, len(latestRecipes)+1),
		VersionRecipes: make(map[string]recipe.Recipe, len(latestRecipes)+1),
		MaxBriefBytes:  maxBriefBytes,
		Memories:       memories,
		OwnerAsks:      make(map[string]controller.OwnerAsk),
		Directives:     make(map[string]store.Directive, len(directives)),
		Settings:       settings,
	}
	for i := range directives {
		d := directives[i]
		if d.ExpiresAt == nil || view.Now.Before(*d.ExpiresAt) {
			view.Directives[d.ID] = d
		}
		if !strings.HasPrefix(d.Ref, ownerAskRefPrefix) || (d.ExpiresAt != nil && !view.Now.Before(*d.ExpiresAt)) {
			continue
		}
		var ask controller.OwnerAsk
		if json.Unmarshal([]byte(d.Text), &ask) == nil && ask.ID != "" {
			view.OwnerAsks[ask.ID] = ask
		}
	}
	defRecipe := recipe.Default()
	view.Recipes[recipe.DefaultName] = defRecipe
	view.VersionRecipes[recipeVersionKey(recipe.DefaultName, defRecipe.Version)] = defRecipe

	for i := range latestRecipes {
		r := latestRecipes[i]
		view.Recipes[r.Name] = r
		view.VersionRecipes[recipeVersionKey(r.Name, r.Version)] = r
	}
	for i := range lanes {
		ln := lanes[i]
		view.Lanes[ln.ID] = ln
		if ln.RecipeName != "" && ln.RecipeVersion > 0 {
			key := recipeVersionKey(ln.RecipeName, ln.RecipeVersion)
			if _, ok := view.VersionRecipes[key]; !ok {
				if cached, cOk := pinnedCache[key]; cOk {
					view.VersionRecipes[key] = cached
				} else if pinned, gErr := st.Recipes().Get(ctx, runID, ln.RecipeName, ln.RecipeVersion); gErr == nil && pinned != nil {
					view.VersionRecipes[key] = *pinned
					if pinnedCache != nil {
						pinnedCache[key] = *pinned
					}
				}
			}
		}
	}
	for i := range backlog {
		it := backlog[i]
		view.Backlog[it.ID] = it
	}
	return view, nil
}

func recipeVersionKey(name string, version int) string {
	return fmt.Sprintf("%s@%d", name, version)
}

// Lane looks up a lane by ID.
func (s *StateView) Lane(id string) (lane.Lane, bool) {
	if s == nil {
		return lane.Lane{}, false
	}
	ln, ok := s.Lanes[id]
	return ln, ok
}

// BacklogItem looks up a backlog item by ID.
func (s *StateView) BacklogItem(id string) (store.BacklogItem, bool) {
	if s == nil {
		return store.BacklogItem{}, false
	}
	it, ok := s.Backlog[id]
	return it, ok
}

// ObjectiveMentionsReadyItem reports whether s.Run.Objective is not done and
// explicitly mentions itemID as ready/next work (so proactive staffing can
// spawn work named in the objective even before a separate backlog.add call).
func (s *StateView) ObjectiveMentionsReadyItem(itemID string) (store.BacklogItem, bool) {
	itemID = strings.TrimSpace(itemID)
	if s == nil || s.Run == nil || itemID == "" {
		return store.BacklogItem{}, false
	}
	obj := strings.TrimSpace(s.Run.Objective)
	if obj == "" || store.ObjectiveIsDone(obj) {
		return store.BacklogItem{}, false
	}
	nextSec := strings.TrimSpace(store.ExtractNextSection(obj))
	searchText := nextSec
	if searchText == "" {
		if !containsReadyWorkMarker(obj) {
			return store.BacklogItem{}, false
		}
		searchText = obj
	} else if store.IsDoneOrEmptyNext(nextSec) {
		return store.BacklogItem{}, false
	}
	matchedBullet := false
	for _, line := range strings.Split(searchText, "\n") {
		if strings.EqualFold(extractReadyBulletItemID(line, nextSec != ""), itemID) {
			matchedBullet = true
			break
		}
	}
	if !matchedBullet {
		return store.BacklogItem{}, false
	}
	title := extractObjectiveItemTitle(cmp.Or(nextSec, obj), itemID)
	return store.BacklogItem{
		ID:       itemID,
		RunID:    s.Run.ID,
		Title:    cmp.Or(title, itemID),
		Priority: store.PriorityP1,
		Status:   store.BacklogOpen,
	}, true
}

// FirstObjectiveReadyItem returns the highest-priority open ready item from the
// backlog or the run objective's Next/ready section when lanes have idle capacity.
func (s *StateView) FirstObjectiveReadyItem() (store.BacklogItem, bool) {
	if s == nil || s.Run == nil || s.Settings.Paused {
		return store.BacklogItem{}, false
	}
	obj := strings.TrimSpace(s.Run.Objective)
	if obj == "" || store.ObjectiveIsDone(obj) {
		return store.BacklogItem{}, false
	}
	nextSec := strings.TrimSpace(store.ExtractNextSection(obj))
	if nextSec == "" && !containsReadyWorkMarker(obj) {
		return store.BacklogItem{}, false
	}
	if nextSec != "" && store.IsDoneOrEmptyNext(nextSec) {
		return store.BacklogItem{}, false
	}
	for id := range s.Directives {
		dir := s.Directives[id]
		if strings.HasPrefix(dir.Ref, ownerAskRefPrefix) {
			continue
		}
		if isHoldDirectiveText(dir.Text) {
			return store.BacklogItem{}, false
		}
	}

	var best *store.BacklogItem
	for id := range s.Backlog {
		it := s.Backlog[id]
		if it.Status != store.BacklogOpen {
			continue
		}
		ready := true
		for _, depID := range it.DependsOn {
			dep, ok := s.BacklogItem(depID)
			if !ok || dep.Status != store.BacklogDone {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if existing, hasLane := s.Lane(it.ID); hasLane && existing.Status != lane.StatusPlanned {
			continue
		}
		_, r, _ := s.ResolveSpawnRecipe(LaneSpawnParams{ItemID: it.ID, RecipeName: it.RecipeName})
		if maxConcurrent := s.EffectiveMaxConcurrentLanes(r); maxConcurrent > 0 && s.RunningLaneCount() >= maxConcurrent {
			continue
		}
		if best == nil || it.Priority < best.Priority || (it.Priority == best.Priority && it.Order < best.Order) || (it.Priority == best.Priority && it.Order == best.Order && it.ID < best.ID) {
			cp := it
			best = &cp
		}
	}
	if best != nil {
		return *best, true
	}

	searchText := cmp.Or(nextSec, obj)
	for _, line := range strings.Split(searchText, "\n") {
		candID := extractReadyBulletItemID(line, nextSec != "")
		if candID == "" {
			continue
		}
		if _, exists := s.BacklogItem(candID); exists {
			continue
		}
		if existing, hasLane := s.Lane(candID); hasLane && existing.Status != lane.StatusPlanned {
			continue
		}
		item, ok := s.ObjectiveMentionsReadyItem(candID)
		if !ok {
			continue
		}
		_, r, _ := s.ResolveSpawnRecipe(LaneSpawnParams{ItemID: item.ID})
		if maxConcurrent := s.EffectiveMaxConcurrentLanes(r); maxConcurrent > 0 && s.RunningLaneCount() >= maxConcurrent {
			continue
		}
		return item, true
	}
	return store.BacklogItem{}, false
}

func extractReadyBulletItemID(line string, inNextSection bool) string {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return ""
	}
	lowerRaw := strings.ToLower(raw)
	hasBulletOrMarker := strings.HasPrefix(raw, "-") ||
		strings.HasPrefix(raw, "*") ||
		(len(raw) >= 2 && raw[0] >= '0' && raw[0] <= '9' && (raw[1] == '.' || raw[1] == ')')) ||
		strings.HasPrefix(lowerRaw, "next:") ||
		strings.HasPrefix(lowerRaw, "ready:") ||
		strings.HasPrefix(lowerRaw, "ready work:") ||
		strings.HasPrefix(lowerRaw, "todo:")
	if !inNextSection && !hasBulletOrMarker {
		return ""
	}
	return extractFirstReadyItemID(raw)
}

func extractFirstReadyItemID(line string) string {
	trimmed := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*0123456789.) \t"))
	for _, cb := range []string{"[ ]", "[x]", "[X]"} {
		if strings.HasPrefix(trimmed, cb) {
			trimmed = strings.TrimSpace(trimmed[len(cb):])
			break
		}
	}
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"next:", "ready:", "ready work:", "todo:"} {
		if strings.HasPrefix(lower, prefix) {
			trimmed = strings.TrimSpace(trimmed[len(prefix):])
			break
		}
	}
	if trimmed == "" {
		return ""
	}
	firstExplicit := hasExplicitLeadingItemID(trimmed)
	for tokIdx, tok := range strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ':' || r == ',' || r == ';' || r == '(' || r == ')' || r == '`' || r == '[' || r == ']'
	}) {
		clean := strings.ToLower(strings.Trim(tok, "._-*"))
		if clean == "" {
			continue
		}
		switch clean {
		case "start", "spawn", "staff", "implement", "build", "run", "the", "next", "ready", "open", "todo",
			"piece", "item", "lane", "work", "task", "phase", "step", "for", "and", "with", "from", "in", "on", "of", "to", "a", "an",
			"note", "goal", "status", "summary", "when", "after", "once", "if", "then", "none", "done", "complete", "completed":
			continue
		}
		if len(clean) < 2 || clean[0] < 'a' || clean[0] > 'z' {
			continue
		}
		valid := true
		hasSlugOrDigit := false
		for i := 0; i < len(clean); i++ {
			if !isIdentChar(clean[i]) {
				valid = false
				break
			}
			if clean[i] == '-' || clean[i] == '_' || (clean[i] >= '0' && clean[i] <= '9') {
				hasSlugOrDigit = true
			}
		}
		if valid && (hasSlugOrDigit || (tokIdx == 0 && firstExplicit)) {
			return clean
		}
	}
	return ""
}

func hasExplicitLeadingItemID(trimmed string) bool {
	if strings.HasPrefix(trimmed, "`") || strings.HasPrefix(trimmed, "[") {
		return true
	}
	firstField := strings.Fields(trimmed)[0]
	firstField = strings.Trim(firstField, "*")
	return strings.HasSuffix(firstField, ":")
}

func containsReadyWorkMarker(s string) bool {
	lower := strings.ToLower(s)
	for _, kw := range []string{"next:", "## next", "# next", "ready:", "ready work", "todo:", "- [ ]"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func containsIdentifierToken(text, id string) bool {
	lowerText := strings.ToLower(text)
	lowerID := strings.ToLower(strings.TrimSpace(id))
	if lowerID == "" {
		return false
	}
	offset := 0
	for offset <= len(lowerText)-len(lowerID) {
		idx := strings.Index(lowerText[offset:], lowerID)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(lowerID)
		leftOK := start == 0 || !isIdentChar(lowerText[start-1])
		rightOK := end == len(lowerText) || !isIdentChar(lowerText[end])
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func isIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '_'
}

func extractObjectiveItemTitle(text, itemID string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*0123456789.)[] \t"))
		if trimmed == "" || !containsIdentifierToken(trimmed, itemID) {
			continue
		}
		idx := strings.Index(strings.ToLower(trimmed), strings.ToLower(itemID))
		if idx >= 0 {
			rest := strings.TrimSpace(trimmed[idx+len(itemID):])
			rest = strings.TrimLeft(rest, ":—–-()[] \t")
			if rest != "" {
				return rest
			}
		}
		return trimmed
	}
	return itemID
}

func isModelAllowed(modelName string, allowed []string) bool {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return true
	}
	if len(allowed) == 0 {
		allowed = DefaultAllowedModels
	}
	return slices.ContainsFunc(allowed, func(m string) bool {
		return strings.EqualFold(strings.TrimSpace(m), trimmed)
	})
}

func isFastestWorkerModel(modelName string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	return lower == "fastest" || lower == "haiku" || strings.Contains(lower, "haiku") || lower == "gpt-5.4-mini" || strings.HasSuffix(lower, "-mini")
}

// ResolveWorkerModel maps policy tier aliases (strongest, mid, cheaper, fastest)
// to concrete worker model identifiers for harness, leaving explicit model names
// unchanged.
func ResolveWorkerModel(modelName, harness string) string {
	trimmed := strings.TrimSpace(modelName)
	lower := strings.ToLower(trimmed)
	isCodex := strings.EqualFold(strings.TrimSpace(harness), "codex") || strings.EqualFold(strings.TrimSpace(harness), "openai")
	switch lower {
	case "strongest":
		if isCodex {
			return "gpt-6.1-sol"
		}
		return "opus"
	case "mid", "cheaper":
		if isCodex {
			return "gpt-5.4"
		}
		return "sonnet"
	case "fastest":
		if isCodex {
			return "gpt-5.4-mini"
		}
		return "haiku"
	default:
		return trimmed
	}
}

func (s *StateView) resolveRecipeName(name string) string {
	if name != "" {
		return name
	}
	if s != nil && s.Run != nil && s.Run.DefaultRecipe != "" {
		return s.Run.DefaultRecipe
	}
	return recipe.DefaultName
}

// RecipeByName resolves the latest recipe for name (falling back to recipe.Default).
func (s *StateView) RecipeByName(name string) (recipe.Recipe, bool) {
	if s == nil {
		def := recipe.Default()
		return def, name == "" || name == recipe.DefaultName
	}
	name = s.resolveRecipeName(name)
	if r, ok := s.Recipes[name]; ok {
		return r, true
	}
	if name == recipe.DefaultName || (s.Run != nil && name == s.Run.DefaultRecipe) {
		return recipe.Default(), true
	}
	return recipe.Recipe{}, false
}

// RecipeForLane resolves the pinned or latest recipe for ln.
func (s *StateView) RecipeForLane(ln lane.Lane) (recipe.Recipe, bool) {
	name := s.resolveRecipeName(ln.RecipeName)
	var r recipe.Recipe
	var ok bool
	if s != nil && ln.RecipeVersion > 0 {
		r, ok = s.VersionRecipes[recipeVersionKey(name, ln.RecipeVersion)]
	}
	if !ok {
		r, ok = s.RecipeByName(name)
	}
	if ok && s != nil {
		itemID := cmp.Or(ln.ItemID, ln.ID)
		if item, hasItem := s.BacklogItem(itemID); (hasItem && (item.NoMerge || store.IsNoMergeBacklogItem(item))) || store.IsNoMergeText(ln.Title) {
			r = recipe.WithoutIntegrate(r)
		}
	}
	return r, ok
}

// ResolveSpawnRecipe resolves the base recipe and effective recipe (including
// named-recipe or deterministic overrides) for a lane.spawn action so vet and
// execution evaluate the same recipe.
func (s *StateView) ResolveSpawnRecipe(p LaneSpawnParams) (base recipe.Recipe, effective recipe.Recipe, hasBase bool) {
	item, _ := s.BacklogItem(strings.TrimSpace(p.ItemID))
	baseName := cmp.Or(strings.TrimSpace(p.RecipeName), item.RecipeName)
	base, hasBase = s.RecipeByName(baseName)
	if !hasBase {
		base = recipe.Default()
	}
	effective = base
	override := strings.TrimSpace(p.RecipeOverride)
	if override != "" {
		if existingOverride, exists := s.RecipeByName(override); exists && override != recipe.DefaultName {
			effective = existingOverride
		} else {
			effective = CompileRecipeOverride(base, override)
		}
	}
	if p.NoMerge || item.NoMerge || store.IsNoMergeBacklogItem(item) || store.IsNoMergeText(p.Brief) {
		effective = recipe.WithoutIntegrate(effective)
	}
	return base, effective, hasBase
}

func (s *StateView) currentTime() time.Time {
	if s != nil && !s.Now.IsZero() {
		return s.Now
	}
	return time.Now().UTC()
}

func (s *StateView) maxBriefBytesForLane(laneID string) int {
	ln, _ := s.Lane(laneID)
	r, _ := s.RecipeForLane(ln)
	return s.EffectiveMaxBriefBytes(r)
}

// RunningLaneCount returns the number of lanes currently in StatusRunning.
func (s *StateView) RunningLaneCount() int {
	if s == nil {
		return 0
	}
	count := 0
	for id := range s.Lanes {
		if s.Lanes[id].Status == lane.StatusRunning {
			count++
		}
	}
	return count
}

// EffectiveMaxBriefBytes returns the brief size limit for r, falling back to
// s.MaxBriefBytes or DefaultMaxBriefBytes.
func (s *StateView) EffectiveMaxBriefBytes(r recipe.Recipe) int {
	if r.Limits.MaxBriefBytes > 0 {
		if s != nil && s.MaxBriefBytes > 0 {
			return min(r.Limits.MaxBriefBytes, s.MaxBriefBytes)
		}
		return r.Limits.MaxBriefBytes
	}
	if s != nil && s.MaxBriefBytes > 0 {
		return s.MaxBriefBytes
	}
	return DefaultMaxBriefBytes
}

// EffectiveMaxConcurrentLanes returns the active concurrency limit for r,
// taking the tighter of r.Limits.MaxConcurrentLanes and s.Settings.ConcurrencyCap.
func (s *StateView) EffectiveMaxConcurrentLanes(r recipe.Recipe) int {
	maxConcurrent := r.Limits.MaxConcurrentLanes
	if s != nil && s.Settings.ConcurrencyCap > 0 && (maxConcurrent <= 0 || s.Settings.ConcurrencyCap < maxConcurrent) {
		maxConcurrent = s.Settings.ConcurrencyCap
	}
	return maxConcurrent
}

// Invariant is a deterministic rule a proposed Dispatcher action must not violate.
type Invariant struct {
	Check func(st *StateView, action ProposedAction) error
	Name  string
}

// DefaultInvariants returns the standard invariant suite for Dispatcher tool calls.
func DefaultInvariants() []Invariant {
	return []Invariant{
		{
			Name:  "no merge outside recipe",
			Check: checkNoMergeOutsideRecipe,
		},
		{
			Name:  "tool must be in dispatcher toolkit",
			Check: checkKnownTool,
		},
		{
			Name:  "owner.ask requires options, default, and deadline",
			Check: checkOwnerAsk,
		},
		{
			Name:  "owner.answer must name an open owner ask",
			Check: checkOwnerAnswer,
		},
		{
			Name:  "brief size within cap",
			Check: checkBriefSizeCap,
		},
		{
			Name:  "lane must exist",
			Check: checkLaneExists,
		},
		{
			Name:  "phase must be declared",
			Check: checkPhaseDeclared,
		},
		{
			Name:  "no spawn onto an occupied lane or ineligible item",
			Check: checkSpawnInvariants,
		},
		{
			Name:  "a spawn or retry must not reuse a recorded round",
			Check: checkNoRoundReuse,
		},
		{
			Name:  "lane state must allow steer, retry, or stop",
			Check: checkLaneActionState,
		},
		{
			Name:  "backlog mutation invariants",
			Check: checkBacklogInvariants,
		},
		{
			Name:  "directive.note and planner.request invariants",
			Check: checkDirectiveAndPlannerInvariants,
		},
	}
}

// Vet filters a Proposal to the actions that satisfy every invariant in invs,
// returning the accepted actions and formatted rejection reasons.
func Vet(st *StateView, p Proposal, invs []Invariant) (accepted []ProposedAction, rejected []string) {
	for i := range p.Actions {
		act := p.Actions[i]
		var bad string
		for _, inv := range invs {
			if err := inv.Check(st, act); err != nil {
				bad = fmt.Sprintf("%s [%s]: %s — %v", act.Tool, act.Target(), inv.Name, err)
				break
			}
		}
		if bad != "" {
			rejected = append(rejected, bad)
			continue
		}
		accepted = append(accepted, act)
	}
	return accepted, rejected
}

// VetAction checks a single ProposedAction against invs and returns an error
// describing the first violated invariant, or nil if all pass.
func VetAction(st *StateView, action ProposedAction, invs []Invariant) error {
	_, rejected := Vet(st, Proposal{Actions: []ProposedAction{action}}, invs)
	if len(rejected) > 0 {
		return errors.New(rejected[0])
	}
	return nil
}

func checkKnownTool(_ *StateView, act ProposedAction) error {
	if !isKnownTool(act.Tool) {
		return fmt.Errorf("tool %q is not permitted for the dispatcher", act.Tool)
	}
	return nil
}

func checkNoMergeOutsideRecipe(st *StateView, act ProposedAction) error {
	lowerTool := strings.ToLower(act.Tool)
	if strings.Contains(lowerTool, "merge") {
		return fmt.Errorf("direct merge tool %q is forbidden; merges are governed by the recipe's integrate phase and merge policy", act.Tool)
	}

	switch act.Tool {
	case ToolLaneSpawn:
		if act.LaneSpawn == nil {
			return nil
		}
		_, r, _ := st.ResolveSpawnRecipe(*act.LaneSpawn)
		if r.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(act.LaneSpawn.Brief) {
			return fmt.Errorf("brief instructs lane to merge directly, but recipe %q has merge mode %q (not %q)",
				r.Name, r.MergePolicy.Mode, recipe.MergeModeSelf)
		}

	case ToolLaneSteer:
		if act.LaneSteer == nil {
			return nil
		}
		if ln, ok := st.Lane(act.LaneSteer.LaneID); ok {
			r, _ := st.RecipeForLane(ln)
			if r.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(act.LaneSteer.Note) {
				return fmt.Errorf("steer note instructs lane to merge directly, but recipe %q has merge mode %q",
					r.Name, r.MergePolicy.Mode)
			}
		}

	case ToolLaneForward:
		if act.LaneForward == nil {
			return nil
		}
		if ln, ok := st.Lane(act.LaneForward.ToLaneID); ok {
			r, _ := st.RecipeForLane(ln)
			if r.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(act.LaneForward.Excerpt) {
				return fmt.Errorf("forwarded excerpt instructs lane to merge directly, but recipe %q has merge mode %q",
					r.Name, r.MergePolicy.Mode)
			}
		}

	case ToolLaneRetry:
		if act.LaneRetry == nil {
			return nil
		}
		ln, ok := st.Lane(act.LaneRetry.LaneID)
		if !ok {
			return nil
		}
		r, _ := st.RecipeForLane(ln)
		targetPhase := cmp.Or(strings.TrimSpace(act.LaneRetry.Phase), ln.Phase, r.FirstPhase())
		if targetPhase == recipe.TargetComplete {
			return fmt.Errorf("only the controller may mark a lane complete after recipe gates pass")
		}
		if r.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(act.LaneRetry.Note) {
			return fmt.Errorf("retry note instructs lane to merge directly, but recipe %q has merge mode %q",
				r.Name, r.MergePolicy.Mode)
		}
		if ph, hasPhase := r.Phase(targetPhase); hasPhase {
			if (targetPhase == recipe.PhaseIntegrate || hasGate(ph, "review_approved")) && r.MergePolicy.RequireReviewPass && ln.ApprovalStale() {
				return fmt.Errorf("cannot enter phase %q with stale or missing review approval (head=%q approval=%q)",
					targetPhase, ln.PRHead, ln.ApprovalSHA)
			}
			if targetPhase == recipe.PhaseIntegrate && r.MergePolicy.RequireCIGreen && !isCIGreen(ln.Checks) {
				return fmt.Errorf("cannot enter integrate phase %q while CI checks are %q",
					targetPhase, cmp.Or(ln.Checks, "pending"))
			}
		}

	case ToolBacklogClose:
		if act.BacklogClose == nil {
			return nil
		}
		if store.BacklogStatus(act.BacklogClose.Status) == store.BacklogDone {
			if it, ok := st.BacklogItem(act.BacklogClose.ID); ok && it.LaneID != "" {
				if ln, hasLane := st.Lane(it.LaneID); hasLane && ln.Status != lane.StatusDone {
					return fmt.Errorf("cannot close backlog item %q as done while assigned lane %q is %s (recipe not completed)",
						it.ID, ln.ID, ln.Status)
				}
			}
		}
	}
	return nil
}

var directMergePatterns = []string{
	"gh pr merge",
	"hub merge",
	"merge the pr yourself",
	"merge your own pr",
	"git push origin main",
	"git push origin master",
}

func containsDirectMergeInstruction(text string) bool {
	lower := strings.ToLower(text)
	for _, pat := range directMergePatterns {
		search := lower
		for {
			idx := strings.Index(search, pat)
			if idx < 0 {
				break
			}
			prefix := search[:idx]
			if cut := strings.LastIndexAny(prefix, ".\n;!?:"); cut >= 0 {
				prefix = prefix[cut+1:]
			}
			if !hasNegationPrefix(prefix) {
				return true
			}
			search = search[idx+len(pat):]
		}
	}
	return false
}

func stripNegatedMergeListTail(clause string) string {
	for {
		trimmed := strings.TrimRight(clause, " \t`\"'([—–-")
		before := trimmed
		hasListSep := false
		for _, conj := range []string{" or", " nor", " and"} {
			if strings.HasSuffix(before, conj) {
				before = strings.TrimRight(before[:len(before)-len(conj)], " \t,`\"')]")
				hasListSep = true
				break
			}
		}
		if !hasListSep && strings.HasSuffix(before, ",") {
			before = strings.TrimRight(before, " \t,`\"')]")
			hasListSep = true
		}
		if !hasListSep {
			return clause
		}
		changed := false
		for _, pat := range directMergePatterns {
			if idx := strings.LastIndex(before, pat); idx >= 0 {
				tail := strings.TrimSpace(before[idx+len(pat):])
				if tail == "" || strings.HasPrefix(tail, "--") || strings.HasPrefix(tail, "-") {
					clause = strings.TrimRight(before[:idx], " \t`\"'([") + " "
					changed = true
					break
				}
			}
		}
		if !changed {
			return clause
		}
	}
}

func isDirectMergeNegationWord(w string) bool {
	switch w {
	case "not", "never", "no", "neither", "nor", "dont", "cannot", "cant", "mustnt", "shouldnt", "without", "forbid", "forbidden", "prohibited":
		return true
	default:
		return false
	}
}

func isDirectMergeExecVerb(w string) bool {
	switch w {
	case "run", "running", "call", "calling", "execute", "executing",
		"use", "using", "invoke", "invoking", "perform", "performing",
		"do", "doing", "issue", "issuing":
		return true
	default:
		return false
	}
}

func hasNegationPrefix(clause string) bool {
	clause = stripNegatedMergeListTail(clause)
	if firstComma := strings.IndexByte(clause, ','); firstComma >= 0 {
		lastComma := strings.LastIndexByte(clause, ',')
		beforeFirst := normalizeContractionWords(strings.ToLower(strings.TrimSpace(clause[:firstComma])))
		beforeWords := strings.FieldsFunc(beforeFirst, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9')
		})
		if firstComma < lastComma && len(beforeWords) > 0 && isDirectMergeNegationWord(beforeWords[len(beforeWords)-1]) {
			clause = clause[:firstComma] + " " + clause[lastComma+1:]
		} else {
			clause = clause[lastComma+1:]
		}
	}
	cleaned := strings.TrimRight(clause, " \t`\"'([—–-")
	norm := normalizeContractionWords(strings.ToLower(cleaned))
	words := strings.FieldsFunc(norm, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	if len(words) == 0 {
		return false
	}
	if isDirectMergeExecVerb(words[len(words)-1]) {
		words = words[:len(words)-1]
	}
	if len(words) == 0 {
		return false
	}
	last := words[len(words)-1]
	if isDirectMergeNegationWord(last) {
		return true
	}
	if (last == "to" || last == "from") && len(words) >= 2 && (words[len(words)-2] == "forbidden" || words[len(words)-2] == "prohibited") {
		return true
	}
	return false
}

// canonicalizeAskDefault makes a default that is not (exactly or up to case and
// surrounding space) one of the options an option of its own. Decision cards
// routinely state "default if unanswered" separately from their choices, and
// the owner has to be able to see what will be applied if they do not answer.
// An empty default or an empty option list is left for checkOwnerAsk to reject.
func canonicalizeAskDefault(options []string, def string) ([]string, string) {
	def = strings.TrimSpace(def)
	if def == "" || len(options) == 0 {
		return options, def
	}
	// Options are trimmed so the default, which applyOwnerAsk also trims, still
	// compares equal to its option under controller.OwnerAsk.Validate.
	trimmed := make([]string, len(options))
	for i, opt := range options {
		trimmed[i] = strings.TrimSpace(opt)
	}
	for _, opt := range trimmed {
		if strings.EqualFold(opt, def) {
			return trimmed, opt
		}
	}
	return append(trimmed, def), def
}

func checkOwnerAsk(st *StateView, act ProposedAction) error {
	if act.Tool != ToolOwnerAsk {
		return nil
	}
	p := act.OwnerAsk
	if p == nil {
		return errors.New("missing owner.ask parameters")
	}
	if strings.TrimSpace(p.Question) == "" {
		return errors.New("question is required")
	}
	if len(p.Options) == 0 {
		return errors.New("options are required (decide, don't ask)")
	}
	for i, opt := range p.Options {
		if strings.TrimSpace(opt) == "" {
			return fmt.Errorf("option %d must not be empty", i)
		}
	}
	if strings.TrimSpace(p.Default) == "" {
		return errors.New("default option is required")
	}
	if !slices.Contains(p.Options, p.Default) {
		return fmt.Errorf("default %q must be one of options %v", p.Default, p.Options)
	}
	if strings.TrimSpace(p.Deadline) == "" {
		return errors.New("deadline is required")
	}
	now := st.currentTime()
	dl, err := parseFutureTime(now, p.Deadline)
	if err != nil {
		return fmt.Errorf("invalid deadline %q: %w", p.Deadline, err)
	}
	if !dl.After(now) {
		return fmt.Errorf("deadline %s must be in the future (now=%s)", dl.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	return nil
}

func checkOwnerAnswer(st *StateView, act ProposedAction) error {
	if act.Tool != ToolOwnerAnswer {
		return nil
	}
	if act.OwnerAnswer == nil || strings.TrimSpace(act.OwnerAnswer.AskID) == "" {
		return errors.New("ask_id is required")
	}
	if strings.TrimSpace(act.OwnerAnswer.Answer) == "" {
		return errors.New("answer is required")
	}
	if _, ok := st.OwnerAsks[strings.TrimSpace(act.OwnerAnswer.AskID)]; !ok {
		return fmt.Errorf("owner ask %q is not open in the snapshot", act.OwnerAnswer.AskID)
	}
	return nil
}

func checkBriefSizeCap(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolLaneSpawn:
		if act.LaneSpawn == nil {
			return errors.New("missing lane.spawn parameters")
		}
		if strings.TrimSpace(act.LaneSpawn.Brief) == "" {
			return errors.New("brief is required")
		}
		_, r, _ := st.ResolveSpawnRecipe(*act.LaneSpawn)
		maxBytes := st.EffectiveMaxBriefBytes(r)
		if len(act.LaneSpawn.Brief) > maxBytes {
			return fmt.Errorf("brief size %d bytes exceeds recipe cap of %d bytes", len(act.LaneSpawn.Brief), maxBytes)
		}

	case ToolLaneSteer:
		if act.LaneSteer == nil {
			return errors.New("missing lane.steer parameters")
		}
		if strings.TrimSpace(act.LaneSteer.Note) == "" {
			return errors.New("steer note is required")
		}
		maxBytes := st.maxBriefBytesForLane(act.LaneSteer.LaneID)
		if len(act.LaneSteer.Note) > maxBytes {
			return fmt.Errorf("steer note size %d bytes exceeds cap of %d bytes", len(act.LaneSteer.Note), maxBytes)
		}

	case ToolLaneForward:
		if act.LaneForward == nil {
			return errors.New("missing lane.forward parameters")
		}
		if strings.TrimSpace(act.LaneForward.Excerpt) == "" {
			return errors.New("excerpt is required for lane.forward")
		}
		maxBytes := st.maxBriefBytesForLane(act.LaneForward.ToLaneID)
		if len(act.LaneForward.Excerpt) > maxBytes {
			return fmt.Errorf("forwarded excerpt size %d bytes exceeds cap of %d bytes", len(act.LaneForward.Excerpt), maxBytes)
		}

	case ToolLaneRetry:
		if act.LaneRetry == nil || act.LaneRetry.Note == "" {
			return nil
		}
		maxBytes := st.maxBriefBytesForLane(act.LaneRetry.LaneID)
		if len(act.LaneRetry.Note) > maxBytes {
			return fmt.Errorf("retry note size %d bytes exceeds cap of %d bytes", len(act.LaneRetry.Note), maxBytes)
		}
	}
	return nil
}

func checkLaneExists(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolLaneSteer, ToolLaneRetry, ToolLaneStop, ToolLaneWait,
		ToolReadLane, ToolReadLaneReport, ToolReadPR, ToolReadCI:
		if strings.TrimSpace(act.LaneID) == "" {
			return errors.New("lane_id is required")
		}
		if _, ok := st.Lane(act.LaneID); !ok {
			return fmt.Errorf("lane %q is not in the store", act.LaneID)
		}
	case ToolLaneForward:
		if act.LaneForward == nil || strings.TrimSpace(act.LaneForward.ToLaneID) == "" {
			return errors.New("to_lane_id is required")
		}
		if _, ok := st.Lane(strings.TrimSpace(act.LaneForward.ToLaneID)); !ok {
			return fmt.Errorf("target lane %q is not in the store", act.LaneForward.ToLaneID)
		}
		if from := strings.TrimSpace(act.LaneForward.FromLaneID); from != "" {
			if _, ok := st.Lane(from); !ok {
				return fmt.Errorf("source lane %q is not in the store", from)
			}
		}
	case ToolOwnerAsk:
		if act.OwnerAsk != nil && strings.TrimSpace(act.OwnerAsk.LaneID) != "" {
			if _, ok := st.Lane(act.OwnerAsk.LaneID); !ok {
				return fmt.Errorf("lane %q is not in the store", act.OwnerAsk.LaneID)
			}
		}
	case ToolMailAnswer:
		if act.MailAnswer != nil && strings.TrimSpace(act.MailAnswer.LaneID) != "" {
			if _, ok := st.Lane(act.MailAnswer.LaneID); !ok {
				return fmt.Errorf("lane %q is not in the store", act.MailAnswer.LaneID)
			}
		}
	case ToolMailDismiss:
		if act.MailDismiss != nil && strings.TrimSpace(act.MailDismiss.LaneID) != "" {
			if _, ok := st.Lane(act.MailDismiss.LaneID); !ok {
				return fmt.Errorf("lane %q is not in the store", act.MailDismiss.LaneID)
			}
		}
	}
	return nil
}

func checkPhaseDeclared(st *StateView, act ProposedAction) error {
	if act.Tool != ToolLaneRetry || act.LaneRetry == nil {
		return nil
	}
	phase := strings.TrimSpace(act.LaneRetry.Phase)
	if phase == "" || phase == recipe.TargetComplete {
		return nil
	}
	ln, ok := st.Lane(act.LaneRetry.LaneID)
	if !ok {
		return nil
	}
	r, ok := st.RecipeForLane(ln)
	if !ok {
		return fmt.Errorf("recipe %q not found for lane %q", ln.RecipeName, ln.ID)
	}
	if _, declared := r.Phase(phase); !declared {
		return fmt.Errorf("phase %q is not declared by recipe %q (%v)", phase, r.Name, r.PhaseNames())
	}
	return nil
}

func checkNoRoundReuse(st *StateView, act ProposedAction) error {
	if act.Tool != ToolLaneRetry || act.LaneRetry == nil {
		return nil
	}
	ln, ok := st.Lane(act.LaneRetry.LaneID)
	if !ok {
		return nil
	}
	r, _ := st.RecipeForLane(ln)
	phase := cmp.Or(strings.TrimSpace(act.LaneRetry.Phase), ln.Phase, r.FirstPhase())
	if phase == "" {
		return nil
	}
	recorded := ln.MaxRound(phase)
	if act.LaneRetry.Round > 0 && recorded > 0 && act.LaneRetry.Round <= recorded {
		return fmt.Errorf("round %d for phase %q would overwrite an existing attempt (max recorded is %d)",
			act.LaneRetry.Round, phase, recorded)
	}
	targetRound := act.LaneRetry.Round
	if targetRound <= 0 {
		targetRound = recorded + 1
	}
	if r.Limits.MaxRounds > 0 && targetRound > r.Limits.MaxRounds {
		return fmt.Errorf("round %d exceeds recipe %q max_rounds %d", targetRound, r.Name, r.Limits.MaxRounds)
	}
	return nil
}

func checkSpawnInvariants(st *StateView, act ProposedAction) error {
	if act.Tool != ToolLaneSpawn {
		return nil
	}
	p := act.LaneSpawn
	if p == nil {
		return errors.New("missing lane.spawn parameters")
	}
	if st != nil && st.Settings.Paused {
		return errors.New("run is paused (cost guard or owner pause)")
	}
	itemID := strings.TrimSpace(p.ItemID)
	if itemID == "" {
		return errors.New("item_id is required")
	}
	item, ok := st.BacklogItem(itemID)
	if !ok {
		item, ok = st.ObjectiveMentionsReadyItem(itemID)
		if !ok {
			return fmt.Errorf("backlog item %q does not exist", itemID)
		}
	}
	if item.Status.Terminal() {
		return fmt.Errorf("backlog item %q is already %s", itemID, item.Status)
	}
	if item.Status == store.BacklogInProgress {
		return fmt.Errorf("backlog item %q is already in progress on lane %q", itemID, cmp.Or(item.LaneID, itemID))
	}

	for _, depID := range item.DependsOn {
		dep, hasDep := st.BacklogItem(depID)
		if !hasDep {
			return fmt.Errorf("backlog item %q depends on unknown item %q", itemID, depID)
		}
		if dep.Status != store.BacklogDone {
			return fmt.Errorf("backlog item %q depends on %q which is %s (must be done)", itemID, depID, dep.Status)
		}
	}

	laneID := cmp.Or(strings.TrimSpace(p.LaneID), itemID)
	if strings.ContainsRune(laneID, '.') {
		return fmt.Errorf("lane_id %q must not contain '.'", laneID)
	}
	if existing, hasLane := st.Lane(laneID); hasLane {
		if existing.Status == lane.StatusDone {
			return fmt.Errorf("lane %q is already done", laneID)
		}
		if existing.Status != lane.StatusPlanned || existing.SessionID != "" || existing.TerminalID != "" ||
			len(existing.Sessions) > 0 || len(existing.Terminals) > 0 {
			return fmt.Errorf("lane %q is already %s; spawning onto an occupied lane would collide", laneID, existing.Status)
		}
	}

	_, r, hasRecipe := st.ResolveSpawnRecipe(*p)
	if !hasRecipe && strings.TrimSpace(p.RecipeName) != "" {
		return fmt.Errorf("unknown recipe %q", p.RecipeName)
	}
	if strings.TrimSpace(p.Model) != "" {
		var allowedModels []string
		if st != nil {
			allowedModels = st.AllowedModels
		}
		if !isModelAllowed(p.Model, allowedModels) {
			if len(allowedModels) == 0 {
				allowedModels = DefaultAllowedModels
			}
			return fmt.Errorf("model %q is not in the allowed models list %v", p.Model, allowedModels)
		}
		if firstPhase := r.FirstPhase(); firstPhase == recipe.PhaseReview && (isFastestWorkerModel(p.Model) || isFastestWorkerModel(ResolveWorkerModel(p.Model, ""))) {
			return fmt.Errorf("model %q cannot be used for verification/review phase %q (never use fastest model for verification)", p.Model, firstPhase)
		}
	}
	maxConcurrent := st.EffectiveMaxConcurrentLanes(r)
	if maxConcurrent > 0 && st.RunningLaneCount() >= maxConcurrent {
		return fmt.Errorf("concurrency limit reached (%d/%d running lanes)",
			st.RunningLaneCount(), maxConcurrent)
	}
	return nil
}

func checkLaneActionState(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolLaneSteer:
		ln, ok := st.Lane(act.LaneID)
		if !ok {
			return nil
		}
		if ln.Status.Terminal() {
			return fmt.Errorf("cannot steer lane %q in terminal status %s", ln.ID, ln.Status)
		}
		if ln.SessionID == "" && ln.TerminalID == "" {
			return fmt.Errorf("cannot steer lane %q before a session or terminal has started", ln.ID)
		}

	case ToolLaneForward:
		if act.LaneForward == nil {
			return errors.New("missing lane.forward parameters")
		}
		toID := strings.TrimSpace(act.LaneForward.ToLaneID)
		fromID := strings.TrimSpace(act.LaneForward.FromLaneID)
		if fromID != "" {
			if strings.EqualFold(fromID, toID) {
				return fmt.Errorf("cannot forward finding from lane %q to itself", toID)
			}
			if srcLn, ok := st.Lane(fromID); ok {
				if srcLn.Status == lane.StatusPlanned && srcLn.SessionID == "" && srcLn.TerminalID == "" &&
					len(srcLn.Sessions) == 0 && len(srcLn.Terminals) == 0 {
					return fmt.Errorf("cannot forward finding from lane %q before a session or terminal has started", srcLn.ID)
				}
			}
		}
		ln, ok := st.Lane(toID)
		if !ok {
			return nil
		}
		if ln.Status.Terminal() {
			return fmt.Errorf("cannot forward finding to lane %q in terminal status %s", ln.ID, ln.Status)
		}
		if ln.SessionID == "" && ln.TerminalID == "" {
			return fmt.Errorf("cannot forward finding to lane %q before a session or terminal has started", ln.ID)
		}

	case ToolLaneRetry:
		ln, ok := st.Lane(act.LaneID)
		if !ok {
			return nil
		}
		if ln.Status == lane.StatusRunning {
			return fmt.Errorf("lane %q is currently running; stop or wait for it before retrying", ln.ID)
		}
		if ln.Status == lane.StatusDone {
			return fmt.Errorf("lane %q is already done", ln.ID)
		}
		if act.LaneRetry != nil && strings.TrimSpace(act.LaneRetry.Model) != "" {
			var allowedModels []string
			if st != nil {
				allowedModels = st.AllowedModels
			}
			if !isModelAllowed(act.LaneRetry.Model, allowedModels) {
				if len(allowedModels) == 0 {
					allowedModels = DefaultAllowedModels
				}
				return fmt.Errorf("model %q is not in the allowed models list %v", act.LaneRetry.Model, allowedModels)
			}
			r, _ := st.RecipeForLane(ln)
			targetPhase := cmp.Or(strings.TrimSpace(act.LaneRetry.Phase), ln.Phase, r.FirstPhase())
			if targetPhase == recipe.PhaseReview && (isFastestWorkerModel(act.LaneRetry.Model) || isFastestWorkerModel(ResolveWorkerModel(act.LaneRetry.Model, ""))) {
				return fmt.Errorf("model %q cannot be used for verification/review phase %q (never use fastest model for verification)", act.LaneRetry.Model, targetPhase)
			}
		}

	case ToolLaneStop:
		if act.LaneStop == nil || strings.TrimSpace(act.LaneStop.Reason) == "" {
			return errors.New("reason is required to stop a lane")
		}
		ln, ok := st.Lane(act.LaneID)
		if !ok {
			return nil
		}
		if ln.Status.Terminal() {
			return fmt.Errorf("lane %q is already in terminal status %s", ln.ID, ln.Status)
		}

	case ToolLaneWait:
		if act.LaneWait == nil || strings.TrimSpace(act.LaneWait.Reason) == "" {
			return errors.New("reason is required for lane.wait")
		}
		if strings.TrimSpace(act.LaneWait.Deadline) == "" {
			return errors.New("deadline is required for lane.wait")
		}
		now := st.currentTime()
		dl, err := parseFutureTime(now, act.LaneWait.Deadline)
		if err != nil {
			return fmt.Errorf("invalid deadline %q: %w", act.LaneWait.Deadline, err)
		}
		if !dl.After(now) {
			return fmt.Errorf("deadline %s must be in the future (now=%s)", dl.Format(time.RFC3339), now.Format(time.RFC3339))
		}
		ln, ok := st.Lane(act.LaneID)
		if !ok {
			return nil
		}
		if ln.Status.Terminal() {
			return fmt.Errorf("lane %q is already in terminal status %s", ln.ID, ln.Status)
		}
	}
	return nil
}

func checkBacklogInvariants(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolBacklogAdd:
		p := act.BacklogAdd
		if p == nil {
			return errors.New("missing backlog.add parameters")
		}
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("backlog item id is required")
		}
		if strings.TrimSpace(p.Title) == "" {
			return errors.New("backlog item title is required")
		}
		if _, exists := st.BacklogItem(p.ID); exists {
			return fmt.Errorf("backlog item %q already exists", p.ID)
		}
		if p.Priority != "" && !store.Priority(p.Priority).Valid() {
			return fmt.Errorf("invalid priority %q", p.Priority)
		}
		for _, depID := range p.DependsOn {
			if depID == p.ID {
				return fmt.Errorf("backlog item %q cannot depend on itself", p.ID)
			}
			if _, ok := st.BacklogItem(depID); !ok {
				return fmt.Errorf("dependency %q does not exist in backlog", depID)
			}
		}

	case ToolBacklogUpdate:
		p := act.BacklogUpdate
		if p == nil {
			return errors.New("missing backlog.update parameters")
		}
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("backlog item id is required")
		}
		if strings.TrimSpace(p.Reason) == "" {
			return errors.New("reason is required for backlog.update")
		}
		if _, exists := st.BacklogItem(p.ID); !exists {
			return fmt.Errorf("backlog item %q does not exist", p.ID)
		}
		if p.Priority != "" && !store.Priority(p.Priority).Valid() {
			return fmt.Errorf("invalid priority %q", p.Priority)
		}
		if p.Status != "" {
			status := store.BacklogStatus(p.Status)
			if !status.Valid() {
				return fmt.Errorf("invalid backlog status %q", p.Status)
			}
			if status.Terminal() {
				return fmt.Errorf("use %s to set terminal status %q", ToolBacklogClose, p.Status)
			}
			if status == store.BacklogInProgress {
				return fmt.Errorf("status %q is owned by %s, which marks the item in_progress when it spawns the lane; do not set it by hand", p.Status, ToolLaneSpawn)
			}
		}

	case ToolBacklogReorder:
		p := act.BacklogReorder
		if p == nil || len(p.ItemIDs) == 0 {
			return errors.New("item_ids is required for backlog.reorder")
		}
		seen := make(map[string]bool, len(p.ItemIDs))
		for _, id := range p.ItemIDs {
			if seen[id] {
				return fmt.Errorf("duplicate item_id %q in backlog.reorder", id)
			}
			seen[id] = true
			if _, exists := st.BacklogItem(id); !exists {
				return fmt.Errorf("backlog item %q does not exist", id)
			}
		}

	case ToolBacklogClose:
		p := act.BacklogClose
		if p == nil {
			return errors.New("missing backlog.close parameters")
		}
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("backlog item id is required")
		}
		if strings.TrimSpace(p.Reason) == "" {
			return errors.New("reason is required for backlog.close")
		}
		if _, exists := st.BacklogItem(p.ID); !exists {
			return fmt.Errorf("backlog item %q does not exist", p.ID)
		}
		status := store.BacklogStatus(p.Status)
		if !status.Terminal() {
			return fmt.Errorf("backlog.close requires status 'done' or 'dropped', got %q", p.Status)
		}
	}
	return nil
}

func checkDirectiveAndPlannerInvariants(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolDirectiveNote:
		p := act.DirectiveNote
		if p == nil || strings.TrimSpace(p.Fact) == "" {
			return errors.New("fact is required for directive.note")
		}
		if strings.TrimSpace(p.Expiry) != "" {
			now := st.currentTime()
			exp, err := parseFutureTime(now, p.Expiry)
			if err != nil {
				return fmt.Errorf("invalid expiry %q: %w", p.Expiry, err)
			}
			if !exp.After(now) {
				return fmt.Errorf("expiry %s must be in the future (now=%s)", exp.Format(time.RFC3339), now.Format(time.RFC3339))
			}
		}

	case ToolDirectiveRevoke:
		p := act.DirectiveRevoke
		if p == nil || store.NormalizeDirectiveID(p.ID) == "" {
			return errors.New("id is required for directive.revoke")
		}
		if st != nil && st.Directives != nil {
			id := store.NormalizeDirectiveID(p.ID)
			dir, inDirs := st.Directives[id]
			if !inDirs {
				return fmt.Errorf("active directive %q does not exist in the snapshot", id)
			}
			if strings.HasPrefix(dir.Ref, ownerAskRefPrefix) {
				return fmt.Errorf("directive %q is an open owner ask; use %s to resolve it", id, ToolOwnerAnswer)
			}
		}

	case ToolPlannerRequest:
		p := act.PlannerRequest
		if p == nil || strings.TrimSpace(p.Reason) == "" {
			return errors.New("reason is required for planner.request")
		}

	case ToolMailAnswer:
		p := act.MailAnswer
		if p == nil || strings.TrimSpace(p.Answer) == "" {
			return errors.New("answer is required for mail.answer")
		}
		if strings.TrimSpace(p.MessageID) == "" && strings.TrimSpace(p.LaneID) == "" {
			return errors.New("message_id or lane_id is required for mail.answer")
		}

	case ToolMailDismiss:
		p := act.MailDismiss
		if p == nil || strings.TrimSpace(p.Reason) == "" {
			return errors.New("reason is required for mail.dismiss")
		}
		if strings.TrimSpace(p.MessageID) == "" && strings.TrimSpace(p.LaneID) == "" {
			return errors.New("message_id or lane_id is required for mail.dismiss")
		}

	case ToolRunCloseout:
		return checkRunCloseout(st, act.RunCloseout)
	}
	return nil
}

// ValidateCloseoutFileRef checks that targetPath is a relative path confined to
// the run directory and is either a standard run close-out file (FINAL.md,
// PROGRESS.md, OBJECTIVE.md) or a file explicitly named in objective.
func ValidateCloseoutFileRef(objective, targetPath string) (string, error) {
	raw := strings.TrimSpace(targetPath)
	if raw == "" {
		return "", errors.New("close-out file path is empty")
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\`) {
		return "", fmt.Errorf("close-out write %q is outside the run directory", raw)
	}
	cleaned := filepath.ToSlash(filepath.Clean(raw))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("close-out write %q is outside the run directory", raw)
	}
	switch cleaned {
	case "FINAL.md", "PROGRESS.md", "OBJECTIVE.md":
		return cleaned, nil
	}
	if !strings.HasSuffix(strings.ToLower(cleaned), ".md") || strings.EqualFold(filepath.Base(cleaned), "HANDOVER.md") {
		return "", fmt.Errorf("close-out write %q must target a markdown file, not a control or handover file", raw)
	}
	if !closeoutObjectiveMentionsFile(objective, cleaned) {
		return "", fmt.Errorf("close-out write %q is not named in OBJECTIVE.md", raw)
	}
	return cleaned, nil
}

func closeoutObjectiveMentionsFile(objective, relPath string) bool {
	if strings.TrimSpace(objective) == "" || relPath == "" {
		return false
	}
	idx := 0
	for idx <= len(objective)-len(relPath) {
		pos := strings.Index(objective[idx:], relPath)
		if pos < 0 {
			return false
		}
		start := idx + pos
		end := start + len(relPath)
		leftOK := start == 0 || isCloseoutPathBoundary(objective[start-1])
		rightOK := end == len(objective) || isCloseoutPathBoundary(objective[end])
		if leftOK && rightOK {
			return true
		}
		idx = start + 1
	}
	return false
}

func isCloseoutPathBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', ',', ';', ':', '(', ')', '[', ']', '"', '\'', '`':
		return true
	default:
		return false
	}
}

// CheckRunTerminal verifies that every lane and backlog item in the run is in a
// terminal status (a backlog item whose ID or LaneID matches a terminal lane is
// treated as terminal). When requireLanes is true, at least one lane must exist.
func CheckRunTerminal(lanes []lane.Lane, items []store.BacklogItem, requireLanes bool) error {
	if requireLanes && len(lanes) == 0 {
		return errors.New("cannot close out run with no lanes")
	}
	terminalLanes := make(map[string]bool, len(lanes))
	for i := range lanes {
		if !lanes[i].Status.Terminal() {
			return fmt.Errorf("cannot close out run while lane %q is %s (all lanes must be terminal)", lanes[i].ID, lanes[i].Status)
		}
		terminalLanes[lanes[i].ID] = true
	}
	for i := range items {
		it := &items[i]
		if !it.Status.Terminal() && !terminalLanes[it.ID] && !terminalLanes[it.LaneID] {
			return fmt.Errorf("cannot close out run while backlog item %q is %s (all items must be terminal)", it.ID, it.Status)
		}
	}
	return nil
}

// ValidateCloseoutPostID checks that postID (when non-empty) is a well-formed
// post identifier explicitly named as a whole token in at least one of sources.
func ValidateCloseoutPostID(postID string, sources ...string) error {
	id := strings.TrimSpace(postID)
	if id == "" {
		return nil
	}
	if !IsLikelyPostID(id) {
		return fmt.Errorf("post_id %q is malformed", id)
	}
	for _, src := range sources {
		if closeoutTextMentionsToken(src, id) {
			return nil
		}
	}
	return fmt.Errorf("post_id %q is not named in OBJECTIVE.md or handover sources", id)
}

func closeoutTextMentionsToken(text, tok string) bool {
	if strings.TrimSpace(text) == "" || tok == "" || len(text) < len(tok) {
		return false
	}
	isTokenByte := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '-'
	}
	for idx := 0; idx <= len(text)-len(tok); {
		pos := strings.Index(text[idx:], tok)
		if pos < 0 {
			return false
		}
		start := idx + pos
		end := start + len(tok)
		leftOK := start == 0 || !isTokenByte(text[start-1])
		rightOK := end == len(text) || !isTokenByte(text[end])
		if leftOK && rightOK {
			return true
		}
		idx = start + 1
	}
	return false
}

// FindPostIDInText extracts a Nebula post ID referenced in text after a
// "post:" or "post" marker.
func FindPostIDInText(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	if idx := indexFoldASCII(text, "post:"); idx >= 0 {
		rest := strings.TrimSpace(text[idx+len("post:"):])
		if tok := leadingHexToken(rest); IsLikelyPostID(tok) {
			return tok
		}
	}
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		postIdx := indexFoldASCII(line, "post")
		if postIdx < 0 {
			continue
		}
		if tok := firstLikelyPostToken(line[postIdx+len("post"):]); tok != "" {
			return tok
		}
	}
	return ""
}

func indexFoldASCII(s, substr string) int {
	if substr == "" {
		return 0
	}
	if len(s) < len(substr) {
		return -1
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if strings.EqualFold(s[i:i+len(substr)], substr) {
			return i
		}
	}
	return -1
}

func firstLikelyPostToken(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' || r == '-')
	})
	for _, f := range fields {
		tok := strings.Trim(f, "-")
		if IsLikelyPostID(tok) {
			return tok
		}
	}
	return ""
}

func leadingHexToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '-' {
			b.WriteByte(c)
		} else {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// IsLikelyPostID reports whether tok looks like a hex or UUID Nebula post ID.
func IsLikelyPostID(tok string) bool {
	if len(tok) < 7 || len(tok) > 36 {
		return false
	}
	hasDigit := false
	hasAlpha := false
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
			hasAlpha = true
		case c == '-':
		default:
			return false
		}
	}
	return hasDigit && hasAlpha
}

func checkRunCloseout(st *StateView, p *RunCloseoutParams) error {
	if p == nil {
		return errors.New("missing run.closeout parameters")
	}
	if st != nil {
		lanes := make([]lane.Lane, 0, len(st.Lanes))
		for id := range st.Lanes {
			lanes = append(lanes, st.Lanes[id])
		}
		slices.SortFunc(lanes, func(a, b lane.Lane) int {
			return strings.Compare(a.ID, b.ID)
		})
		items := make([]store.BacklogItem, 0, len(st.Backlog))
		for id := range st.Backlog {
			items = append(items, st.Backlog[id])
		}
		slices.SortFunc(items, func(a, b store.BacklogItem) int {
			return strings.Compare(a.ID, b.ID)
		})
		if err := CheckRunTerminal(lanes, items, true); err != nil {
			return err
		}
	}
	var objText, planRef string
	if st != nil && st.Run != nil {
		objText = st.Run.Objective
		planRef = st.Run.PlanRef
	}
	if strings.TrimSpace(p.PlanFile) != "" {
		if _, err := ValidateCloseoutFileRef(objText, p.PlanFile); err != nil {
			return err
		}
	}
	for target := range p.Files {
		if _, err := ValidateCloseoutFileRef(objText, target); err != nil {
			return err
		}
	}
	if err := ValidateCloseoutPostID(p.PostID, objText, planRef); err != nil {
		return err
	}
	return nil
}

func isObjectiveDoneText(objective string) bool {
	return store.ObjectiveIsDone(objective)
}

func parseFutureTime(now time.Time, raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, errors.New("empty time string")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(d).UTC(), nil
	}
	// Owner cards state deadlines as "7d" or as a UTC clock time ("05:30Z").
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return now.AddDate(0, 0, n).UTC(), nil
		}
	}
	if clock, err := time.Parse("15:04Z", s); err == nil {
		now = now.UTC()
		t := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, time.UTC)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 timestamp, duration (e.g. 30m, 2h, 7d) or UTC clock time (e.g. 05:30Z, next occurrence)")
}

func isCIGreen(checks string) bool {
	switch strings.ToLower(strings.TrimSpace(checks)) {
	case "green", "pass", "passed", "success", "ok", "true":
		return true
	default:
		return false
	}
}

func hasGate(p recipe.Phase, gateName string) bool {
	return slices.ContainsFunc(p.Gates, func(g recipe.Gate) bool {
		return g.Name == gateName
	})
}
