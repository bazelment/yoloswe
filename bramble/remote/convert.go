package remote

import (
	"encoding/json"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
	"github.com/bazelment/yoloswe/wt/taskrouter"

	pb "github.com/bazelment/yoloswe/bramble/remote/proto"
)

// ============================================================================
// Time helpers
// ============================================================================

func timeToUnixNs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func timeFromUnixNs(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func timePtrToUnixNs(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixNano()
}

func timePtrFromUnixNs(ns int64) *time.Time {
	if ns == 0 {
		return nil
	}
	t := time.Unix(0, ns)
	return &t
}

// ============================================================================
// SessionInfo
// ============================================================================

// SessionInfoToProto converts a Go SessionInfo to proto.
func SessionInfoToProto(s session.SessionInfo) *pb.SessionInfo {
	return &pb.SessionInfo{
		Id:                string(s.ID),
		Type:              string(s.Type),
		Status:            string(s.Status),
		WorktreePath:      s.WorktreePath,
		WorktreeName:      s.WorktreeName,
		Prompt:            s.Prompt,
		Title:             s.Title,
		Model:             s.Model,
		PlanFilePath:      s.PlanFilePath,
		TmuxWindowName:    s.TmuxWindowName,
		RunnerType:        s.RunnerType,
		CreatedAtUnixNs:   timeToUnixNs(s.CreatedAt),
		StartedAtUnixNs:   timePtrToUnixNs(s.StartedAt),
		CompletedAtUnixNs: timePtrToUnixNs(s.CompletedAt),
		ErrorMsg:          s.ErrorMsg,
		Progress:          SessionProgressSnapshotToProto(s.Progress),
	}
}

// SessionInfoFromProto converts a proto SessionInfo to Go.
func SessionInfoFromProto(p *pb.SessionInfo) session.SessionInfo {
	if p == nil {
		return session.SessionInfo{}
	}
	return session.SessionInfo{
		ID:             session.SessionID(p.Id),
		Type:           session.SessionType(p.Type),
		Status:         session.SessionStatus(p.Status),
		WorktreePath:   p.WorktreePath,
		WorktreeName:   p.WorktreeName,
		Prompt:         p.Prompt,
		Title:          p.Title,
		Model:          p.Model,
		PlanFilePath:   p.PlanFilePath,
		TmuxWindowName: p.TmuxWindowName,
		RunnerType:     p.RunnerType,
		CreatedAt:      timeFromUnixNs(p.CreatedAtUnixNs),
		StartedAt:      timePtrFromUnixNs(p.StartedAtUnixNs),
		CompletedAt:    timePtrFromUnixNs(p.CompletedAtUnixNs),
		ErrorMsg:       p.ErrorMsg,
		Progress:       SessionProgressSnapshotFromProto(p.Progress),
	}
}

// ============================================================================
// SessionProgress
// ============================================================================

// SessionProgressSnapshotToProto converts a Go progress snapshot to proto.
func SessionProgressSnapshotToProto(p session.SessionProgressSnapshot) *pb.SessionProgress {
	return &pb.SessionProgress{
		CurrentPhase:       p.CurrentPhase,
		CurrentTool:        p.CurrentTool,
		StatusLine:         p.StatusLine,
		TurnCount:          int32(p.TurnCount),
		TotalCostUsd:       p.TotalCostUSD,
		InputTokens:        int32(p.InputTokens),
		OutputTokens:       int32(p.OutputTokens),
		LastActivityUnixNs: timeToUnixNs(p.LastActivity),
	}
}

// SessionProgressSnapshotFromProto converts a proto progress to Go snapshot.
func SessionProgressSnapshotFromProto(p *pb.SessionProgress) session.SessionProgressSnapshot {
	if p == nil {
		return session.SessionProgressSnapshot{}
	}
	return session.SessionProgressSnapshot{
		CurrentPhase: p.CurrentPhase,
		CurrentTool:  p.CurrentTool,
		StatusLine:   p.StatusLine,
		TurnCount:    int(p.TurnCount),
		TotalCostUSD: p.TotalCostUsd,
		InputTokens:  int(p.InputTokens),
		OutputTokens: int(p.OutputTokens),
		LastActivity: timeFromUnixNs(p.LastActivityUnixNs),
	}
}

// ============================================================================
// OutputLine
// ============================================================================

// OutputLineToProto converts a Go OutputLine to proto.
func OutputLineToProto(o session.OutputLine) *pb.OutputLine {
	var toolInputJSON []byte
	if o.ToolInput != nil {
		toolInputJSON, _ = json.Marshal(o.ToolInput)
	}
	var toolResultJSON []byte
	if o.ToolResult != nil {
		toolResultJSON, _ = json.Marshal(o.ToolResult)
	}

	return &pb.OutputLine{
		TimestampUnixNs: timeToUnixNs(o.Timestamp),
		Type:            string(o.Type),
		Content:         o.Content,
		ToolName:        o.ToolName,
		ToolId:          o.ToolID,
		ToolState:       string(o.ToolState),
		ToolInputJson:   toolInputJSON,
		ToolResultJson:  toolResultJSON,
		StartTimeUnixNs: timeToUnixNs(o.StartTime),
		TurnNumber:      int32(o.TurnNumber),
		CostUsd:         o.CostUSD,
		DurationMs:      o.DurationMs,
		IsError:         o.IsError,
	}
}

// OutputLineFromProto converts a proto OutputLine to Go.
func OutputLineFromProto(p *pb.OutputLine) session.OutputLine {
	if p == nil {
		return session.OutputLine{}
	}

	var toolInput map[string]interface{}
	if len(p.ToolInputJson) > 0 {
		_ = json.Unmarshal(p.ToolInputJson, &toolInput)
	}
	var toolResult interface{}
	if len(p.ToolResultJson) > 0 {
		_ = json.Unmarshal(p.ToolResultJson, &toolResult)
	}

	return session.OutputLine{
		Timestamp:  timeFromUnixNs(p.TimestampUnixNs),
		Type:       session.OutputLineType(p.Type),
		Content:    p.Content,
		ToolName:   p.ToolName,
		ToolID:     p.ToolId,
		ToolState:  session.ToolState(p.ToolState),
		ToolInput:  toolInput,
		ToolResult: toolResult,
		StartTime:  timeFromUnixNs(p.StartTimeUnixNs),
		TurnNumber: int(p.TurnNumber),
		CostUSD:    p.CostUsd,
		DurationMs: p.DurationMs,
		IsError:    p.IsError,
	}
}

// ============================================================================
// Worktree
// ============================================================================

// WorktreeToProto converts a Go Worktree to proto.
func WorktreeToProto(w wt.Worktree) *pb.Worktree {
	return &pb.Worktree{
		Path:       w.Path,
		Branch:     w.Branch,
		Commit:     w.Commit,
		IsDetached: w.IsDetached,
	}
}

// WorktreeFromProto converts a proto Worktree to Go.
func WorktreeFromProto(p *pb.Worktree) wt.Worktree {
	if p == nil {
		return wt.Worktree{}
	}
	return wt.Worktree{
		Path:       p.Path,
		Branch:     p.Branch,
		Commit:     p.Commit,
		IsDetached: p.IsDetached,
	}
}

// ============================================================================
// WorktreeStatus
// ============================================================================

// WorktreeStatusToProto converts a Go WorktreeStatus to proto.
func WorktreeStatusToProto(s *wt.WorktreeStatus) *pb.WorktreeStatus {
	if s == nil {
		return nil
	}
	return &pb.WorktreeStatus{
		Worktree:             WorktreeToProto(s.Worktree),
		IsDirty:              s.IsDirty,
		Ahead:                int32(s.Ahead),
		Behind:               int32(s.Behind),
		PrNumber:             int32(s.PRNumber),
		PrUrl:                s.PRURL,
		PrState:              s.PRState,
		PrReviewStatus:       s.PRReviewStatus,
		PrIsDraft:            s.PRIsDraft,
		LastCommitMsg:        s.LastCommitMsg,
		LastCommitTimeUnixNs: timeToUnixNs(s.LastCommitTime),
	}
}

// WorktreeStatusFromProto converts a proto WorktreeStatus to Go.
func WorktreeStatusFromProto(p *pb.WorktreeStatus) *wt.WorktreeStatus {
	if p == nil {
		return nil
	}
	return &wt.WorktreeStatus{
		Worktree:       WorktreeFromProto(p.Worktree),
		IsDirty:        p.IsDirty,
		Ahead:          int(p.Ahead),
		Behind:         int(p.Behind),
		PRNumber:       int(p.PrNumber),
		PRURL:          p.PrUrl,
		PRState:        p.PrState,
		PRReviewStatus: p.PrReviewStatus,
		PRIsDraft:      p.PrIsDraft,
		LastCommitMsg:  p.LastCommitMsg,
		LastCommitTime: timeFromUnixNs(p.LastCommitTimeUnixNs),
	}
}

// ============================================================================
// PRInfo
// ============================================================================

// PRInfoToProto converts a Go PRInfo to proto.
func PRInfoToProto(p wt.PRInfo) *pb.PRInfo {
	return &pb.PRInfo{
		Url:            p.URL,
		HeadRefName:    p.HeadRefName,
		BaseRefName:    p.BaseRefName,
		State:          p.State,
		ReviewDecision: p.ReviewDecision,
		Number:         int32(p.Number),
		IsDraft:        p.IsDraft,
	}
}

// PRInfoFromProto converts a proto PRInfo to Go.
func PRInfoFromProto(p *pb.PRInfo) wt.PRInfo {
	if p == nil {
		return wt.PRInfo{}
	}
	return wt.PRInfo{
		URL:            p.Url,
		HeadRefName:    p.HeadRefName,
		BaseRefName:    p.BaseRefName,
		State:          p.State,
		ReviewDecision: p.ReviewDecision,
		Number:         int(p.Number),
		IsDraft:        p.IsDraft,
	}
}

// ============================================================================
// CommitInfo
// ============================================================================

// CommitInfoToProto converts a Go CommitInfo to proto.
func CommitInfoToProto(c wt.CommitInfo) *pb.CommitInfo {
	return &pb.CommitInfo{
		Hash:       c.Hash,
		Subject:    c.Subject,
		Author:     c.Author,
		DateUnixNs: timeToUnixNs(c.Date),
	}
}

// CommitInfoFromProto converts a proto CommitInfo to Go.
func CommitInfoFromProto(p *pb.CommitInfo) wt.CommitInfo {
	if p == nil {
		return wt.CommitInfo{}
	}
	return wt.CommitInfo{
		Hash:    p.Hash,
		Subject: p.Subject,
		Author:  p.Author,
		Date:    timeFromUnixNs(p.DateUnixNs),
	}
}

// ============================================================================
// WorktreeContext
// ============================================================================

// WorktreeContextToProto converts a Go WorktreeContext to proto.
func WorktreeContextToProto(c *wt.WorktreeContext) *pb.WorktreeContext {
	if c == nil {
		return nil
	}
	commits := make([]*pb.CommitInfo, len(c.RecentCommits))
	for i, ci := range c.RecentCommits {
		commits[i] = CommitInfoToProto(ci)
	}
	return &pb.WorktreeContext{
		Path:             c.Path,
		Branch:           c.Branch,
		Goal:             c.Goal,
		Parent:           c.Parent,
		IsDirty:          c.IsDirty,
		Ahead:            int32(c.Ahead),
		Behind:           int32(c.Behind),
		ChangedFiles:     c.ChangedFiles,
		UntrackedFiles:   c.UntrackedFiles,
		RecentCommits:    commits,
		DiffStat:         c.DiffStat,
		DiffContent:      c.DiffContent,
		PrNumber:         int32(c.PRNumber),
		PrUrl:            c.PRURL,
		PrState:          c.PRState,
		GatheredAtUnixNs: timeToUnixNs(c.GatheredAt),
	}
}

// WorktreeContextFromProto converts a proto WorktreeContext to Go.
func WorktreeContextFromProto(p *pb.WorktreeContext) *wt.WorktreeContext {
	if p == nil {
		return nil
	}
	commits := make([]wt.CommitInfo, len(p.RecentCommits))
	for i, ci := range p.RecentCommits {
		commits[i] = CommitInfoFromProto(ci)
	}
	return &wt.WorktreeContext{
		Path:           p.Path,
		Branch:         p.Branch,
		Goal:           p.Goal,
		Parent:         p.Parent,
		IsDirty:        p.IsDirty,
		Ahead:          int(p.Ahead),
		Behind:         int(p.Behind),
		ChangedFiles:   p.ChangedFiles,
		UntrackedFiles: p.UntrackedFiles,
		RecentCommits:  commits,
		DiffStat:       p.DiffStat,
		DiffContent:    p.DiffContent,
		PRNumber:       int(p.PrNumber),
		PRURL:          p.PrUrl,
		PRState:        p.PrState,
		GatheredAt:     timeFromUnixNs(p.GatheredAtUnixNs),
	}
}

// ============================================================================
// ContextOptions
// ============================================================================

// ContextOptionsToProto converts Go ContextOptions to proto.
func ContextOptionsToProto(o wt.ContextOptions) *pb.ContextOptions {
	return &pb.ContextOptions{
		IncludeDiff:     o.IncludeDiff,
		IncludeDiffStat: o.IncludeDiffStat,
		IncludeFileList: o.IncludeFileList,
		IncludePrInfo:   o.IncludePRInfo,
		IncludeCommits:  int32(o.IncludeCommits),
		MaxDiffBytes:    int32(o.MaxDiffBytes),
	}
}

// ContextOptionsFromProto converts proto ContextOptions to Go.
func ContextOptionsFromProto(p *pb.ContextOptions) wt.ContextOptions {
	if p == nil {
		return wt.ContextOptions{}
	}
	return wt.ContextOptions{
		IncludeDiff:     p.IncludeDiff,
		IncludeDiffStat: p.IncludeDiffStat,
		IncludeFileList: p.IncludeFileList,
		IncludePRInfo:   p.IncludePrInfo,
		IncludeCommits:  int(p.IncludeCommits),
		MaxDiffBytes:    int(p.MaxDiffBytes),
	}
}

// ============================================================================
// MergeOptions
// ============================================================================

// MergeOptionsToProto converts Go MergeOptions to proto.
func MergeOptionsToProto(o wt.MergeOptions) *pb.MergeOptions {
	return &pb.MergeOptions{
		MergeMethod: o.MergeMethod,
		Keep:        o.Keep,
	}
}

// MergeOptionsFromProto converts proto MergeOptions to Go.
func MergeOptionsFromProto(p *pb.MergeOptions) wt.MergeOptions {
	if p == nil {
		return wt.MergeOptions{}
	}
	return wt.MergeOptions{
		MergeMethod: p.MergeMethod,
		Keep:        p.Keep,
	}
}

// ============================================================================
// SessionMeta
// ============================================================================

// SessionMetaToProto converts a Go SessionMeta to proto.
func SessionMetaToProto(m *session.SessionMeta) *pb.SessionMeta {
	if m == nil {
		return nil
	}
	return &pb.SessionMeta{
		Id:                string(m.ID),
		Type:              string(m.Type),
		Status:            string(m.Status),
		RepoName:          m.RepoName,
		WorktreeName:      m.WorktreeName,
		Prompt:            m.Prompt,
		Title:             m.Title,
		Model:             m.Model,
		CreatedAtUnixNs:   timeToUnixNs(m.CreatedAt),
		CompletedAtUnixNs: timePtrToUnixNs(m.CompletedAt),
	}
}

// SessionMetaFromProto converts a proto SessionMeta to Go.
func SessionMetaFromProto(p *pb.SessionMeta) *session.SessionMeta {
	if p == nil {
		return nil
	}
	return &session.SessionMeta{
		ID:           session.SessionID(p.Id),
		Type:         session.SessionType(p.Type),
		Status:       session.SessionStatus(p.Status),
		RepoName:     p.RepoName,
		WorktreeName: p.WorktreeName,
		Prompt:       p.Prompt,
		Title:        p.Title,
		Model:        p.Model,
		CreatedAt:    timeFromUnixNs(p.CreatedAtUnixNs),
		CompletedAt:  timePtrFromUnixNs(p.CompletedAtUnixNs),
	}
}

// ============================================================================
// StoredSession
// ============================================================================

// StoredSessionToProto converts a Go StoredSession to proto.
func StoredSessionToProto(s *session.StoredSession) *pb.StoredSession {
	if s == nil {
		return nil
	}
	output := make([]*pb.OutputLine, len(s.Output))
	for i := range s.Output {
		output[i] = OutputLineToProto(s.Output[i])
	}

	var progress *pb.SessionProgress
	if s.Progress != nil {
		progress = &pb.SessionProgress{
			TurnCount:    int32(s.Progress.TurnCount),
			TotalCostUsd: s.Progress.TotalCostUSD,
			InputTokens:  int32(s.Progress.InputTokens),
			OutputTokens: int32(s.Progress.OutputTokens),
		}
	}

	return &pb.StoredSession{
		Id:                string(s.ID),
		Type:              string(s.Type),
		Status:            string(s.Status),
		RepoName:          s.RepoName,
		WorktreePath:      s.WorktreePath,
		WorktreeName:      s.WorktreeName,
		Prompt:            s.Prompt,
		Title:             s.Title,
		Model:             s.Model,
		CreatedAtUnixNs:   timeToUnixNs(s.CreatedAt),
		StartedAtUnixNs:   timePtrToUnixNs(s.StartedAt),
		CompletedAtUnixNs: timePtrToUnixNs(s.CompletedAt),
		ErrorMsg:          s.ErrorMsg,
		Progress:          progress,
		Output:            output,
	}
}

// StoredSessionFromProto converts a proto StoredSession to Go.
func StoredSessionFromProto(p *pb.StoredSession) *session.StoredSession {
	if p == nil {
		return nil
	}
	output := make([]session.OutputLine, len(p.Output))
	for i, o := range p.Output {
		output[i] = OutputLineFromProto(o)
	}

	var progress *session.StoredProgress
	if p.Progress != nil {
		progress = &session.StoredProgress{
			TurnCount:    int(p.Progress.TurnCount),
			TotalCostUSD: p.Progress.TotalCostUsd,
			InputTokens:  int(p.Progress.InputTokens),
			OutputTokens: int(p.Progress.OutputTokens),
		}
	}

	return &session.StoredSession{
		ID:           session.SessionID(p.Id),
		Type:         session.SessionType(p.Type),
		Status:       session.SessionStatus(p.Status),
		RepoName:     p.RepoName,
		WorktreePath: p.WorktreePath,
		WorktreeName: p.WorktreeName,
		Prompt:       p.Prompt,
		Title:        p.Title,
		Model:        p.Model,
		CreatedAt:    timeFromUnixNs(p.CreatedAtUnixNs),
		StartedAt:    timePtrFromUnixNs(p.StartedAtUnixNs),
		CompletedAt:  timePtrFromUnixNs(p.CompletedAtUnixNs),
		ErrorMsg:     p.ErrorMsg,
		Progress:     progress,
		Output:       output,
	}
}

// ============================================================================
// Events
// ============================================================================

// SessionEventToProto converts a Go session event to a proto SessionEvent.
func SessionEventToProto(event interface{}) *pb.SessionEvent {
	switch e := event.(type) {
	case session.SessionStateChangeEvent:
		return &pb.SessionEvent{
			Event: &pb.SessionEvent_StateChange{
				StateChange: &pb.StateChangeEvent{
					SessionId: string(e.SessionID),
					OldStatus: string(e.OldStatus),
					NewStatus: string(e.NewStatus),
				},
			},
		}
	case session.SessionOutputEvent:
		return &pb.SessionEvent{
			Event: &pb.SessionEvent_Output{
				Output: &pb.OutputEvent{
					SessionId: string(e.SessionID),
					Line:      OutputLineToProto(e.Line),
				},
			},
		}
	default:
		return nil
	}
}

// ============================================================================
// Task Router types
// ============================================================================

// RouteRequestToProto converts a Go RouteRequest to proto.
func RouteRequestToProto(r taskrouter.RouteRequest) *pb.RouteTaskRequest {
	wts := make([]*pb.TaskWorktreeInfo, len(r.Worktrees))
	for i, w := range r.Worktrees {
		wts[i] = &pb.TaskWorktreeInfo{
			Name:       w.Name,
			Path:       w.Path,
			Goal:       w.Goal,
			Parent:     w.Parent,
			PrState:    w.PRState,
			LastCommit: w.LastCommit,
			IsDirty:    w.IsDirty,
			IsAhead:    w.IsAhead,
			IsMerged:   w.IsMerged,
		}
	}
	return &pb.RouteTaskRequest{
		Prompt:    r.Prompt,
		CurrentWt: r.CurrentWT,
		RepoName:  r.RepoName,
		Worktrees: wts,
	}
}

// RouteRequestFromProto converts a proto RouteTaskRequest to Go.
func RouteRequestFromProto(p *pb.RouteTaskRequest) taskrouter.RouteRequest {
	if p == nil {
		return taskrouter.RouteRequest{}
	}
	wts := make([]taskrouter.WorktreeInfo, len(p.Worktrees))
	for i, w := range p.Worktrees {
		wts[i] = taskrouter.WorktreeInfo{
			Name:       w.Name,
			Path:       w.Path,
			Goal:       w.Goal,
			Parent:     w.Parent,
			PRState:    w.PrState,
			LastCommit: w.LastCommit,
			IsDirty:    w.IsDirty,
			IsAhead:    w.IsAhead,
			IsMerged:   w.IsMerged,
		}
	}
	return taskrouter.RouteRequest{
		Prompt:    p.Prompt,
		CurrentWT: p.CurrentWt,
		RepoName:  p.RepoName,
		Worktrees: wts,
	}
}

// RouteProposalToProto converts a Go RouteProposal to proto.
func RouteProposalToProto(r *taskrouter.RouteProposal) *pb.RouteProposal {
	if r == nil {
		return nil
	}
	return &pb.RouteProposal{
		Action:    string(r.Action),
		Worktree:  r.Worktree,
		Parent:    r.Parent,
		Reasoning: r.Reasoning,
	}
}

// RouteProposalFromProto converts a proto RouteProposal to Go.
func RouteProposalFromProto(p *pb.RouteProposal) *taskrouter.RouteProposal {
	if p == nil {
		return nil
	}
	return &taskrouter.RouteProposal{
		Action:    taskrouter.ProposalAction(p.Action),
		Worktree:  p.Worktree,
		Parent:    p.Parent,
		Reasoning: p.Reasoning,
	}
}
