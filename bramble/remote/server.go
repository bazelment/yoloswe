package remote

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bazelment/yoloswe/bramble/service"
	"github.com/bazelment/yoloswe/bramble/session"

	pb "github.com/bazelment/yoloswe/bramble/remote/proto"
)

// ============================================================================
// Session Server
// ============================================================================

type sessionServer struct {
	pb.UnimplementedBrambleSessionServiceServer
	mgr         session.SessionService
	broadcaster *EventBroadcaster
}

// NewSessionServer creates a gRPC session server.
func NewSessionServer(mgr session.SessionService, broadcaster *EventBroadcaster) pb.BrambleSessionServiceServer {
	return &sessionServer{mgr: mgr, broadcaster: broadcaster}
}

func (s *sessionServer) StartSession(_ context.Context, req *pb.StartSessionRequest) (*pb.StartSessionResponse, error) {
	id, err := s.mgr.StartSession(session.SessionType(req.SessionType), req.WorktreePath, req.Prompt)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start session: %v", err)
	}
	return &pb.StartSessionResponse{SessionId: string(id)}, nil
}

func (s *sessionServer) StopSession(_ context.Context, req *pb.StopSessionRequest) (*pb.StopSessionResponse, error) {
	if err := s.mgr.StopSession(session.SessionID(req.SessionId)); err != nil {
		return nil, status.Errorf(codes.Internal, "stop session: %v", err)
	}
	return &pb.StopSessionResponse{}, nil
}

func (s *sessionServer) SendFollowUp(_ context.Context, req *pb.SendFollowUpRequest) (*pb.SendFollowUpResponse, error) {
	if err := s.mgr.SendFollowUp(session.SessionID(req.SessionId), req.Message); err != nil {
		return nil, status.Errorf(codes.Internal, "send follow-up: %v", err)
	}
	return &pb.SendFollowUpResponse{}, nil
}

func (s *sessionServer) CompleteSession(_ context.Context, req *pb.CompleteSessionRequest) (*pb.CompleteSessionResponse, error) {
	if err := s.mgr.CompleteSession(session.SessionID(req.SessionId)); err != nil {
		return nil, status.Errorf(codes.Internal, "complete session: %v", err)
	}
	return &pb.CompleteSessionResponse{}, nil
}

func (s *sessionServer) DeleteSession(_ context.Context, req *pb.DeleteSessionRequest) (*pb.DeleteSessionResponse, error) {
	if err := s.mgr.DeleteSession(session.SessionID(req.SessionId)); err != nil {
		return nil, status.Errorf(codes.Internal, "delete session: %v", err)
	}
	return &pb.DeleteSessionResponse{}, nil
}

func (s *sessionServer) GetSessionInfo(_ context.Context, req *pb.GetSessionInfoRequest) (*pb.GetSessionInfoResponse, error) {
	info, ok := s.mgr.GetSessionInfo(session.SessionID(req.SessionId))
	if !ok {
		return &pb.GetSessionInfoResponse{Found: false}, nil
	}
	return &pb.GetSessionInfoResponse{Info: SessionInfoToProto(info), Found: true}, nil
}

func (s *sessionServer) GetSessionsForWorktree(_ context.Context, req *pb.GetSessionsForWorktreeRequest) (*pb.GetSessionsForWorktreeResponse, error) {
	sessions := s.mgr.GetSessionsForWorktree(req.WorktreePath)
	result := make([]*pb.SessionInfo, len(sessions))
	for i := range sessions {
		result[i] = SessionInfoToProto(sessions[i])
	}
	return &pb.GetSessionsForWorktreeResponse{Sessions: result}, nil
}

func (s *sessionServer) GetAllSessions(_ context.Context, _ *pb.GetAllSessionsRequest) (*pb.GetAllSessionsResponse, error) {
	sessions := s.mgr.GetAllSessions()
	result := make([]*pb.SessionInfo, len(sessions))
	for i := range sessions {
		result[i] = SessionInfoToProto(sessions[i])
	}
	return &pb.GetAllSessionsResponse{Sessions: result}, nil
}

func (s *sessionServer) GetSessionOutput(_ context.Context, req *pb.GetSessionOutputRequest) (*pb.GetSessionOutputResponse, error) {
	lines := s.mgr.GetSessionOutput(session.SessionID(req.SessionId))
	result := make([]*pb.OutputLine, len(lines))
	for i := range lines {
		result[i] = OutputLineToProto(lines[i])
	}
	return &pb.GetSessionOutputResponse{Lines: result}, nil
}

func (s *sessionServer) CountByStatus(_ context.Context, _ *pb.CountByStatusRequest) (*pb.CountByStatusResponse, error) {
	counts := s.mgr.CountByStatus()
	result := make(map[string]int32, len(counts))
	for k, v := range counts {
		result[string(k)] = int32(v)
	}
	return &pb.CountByStatusResponse{Counts: result}, nil
}

func (s *sessionServer) LoadHistorySessions(_ context.Context, req *pb.LoadHistorySessionsRequest) (*pb.LoadHistorySessionsResponse, error) {
	metas, err := s.mgr.LoadHistorySessions(req.WorktreeName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load history: %v", err)
	}
	result := make([]*pb.SessionMeta, len(metas))
	for i, m := range metas {
		result[i] = SessionMetaToProto(m)
	}
	return &pb.LoadHistorySessionsResponse{Sessions: result}, nil
}

func (s *sessionServer) LoadSessionFromHistory(_ context.Context, req *pb.LoadSessionFromHistoryRequest) (*pb.LoadSessionFromHistoryResponse, error) {
	stored, err := s.mgr.LoadSessionFromHistory(req.WorktreeName, session.SessionID(req.SessionId))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load session: %v", err)
	}
	return &pb.LoadSessionFromHistoryResponse{Session: StoredSessionToProto(stored)}, nil
}

func (s *sessionServer) IsInTmuxMode(_ context.Context, _ *pb.IsInTmuxModeRequest) (*pb.IsInTmuxModeResponse, error) {
	return &pb.IsInTmuxModeResponse{IsTmuxMode: s.mgr.IsInTmuxMode()}, nil
}

func (s *sessionServer) StreamEvents(_ *pb.StreamEventsRequest, stream pb.BrambleSessionService_StreamEventsServer) error {
	id, ch := s.broadcaster.Subscribe(10000)
	defer s.broadcaster.Unsubscribe(id)

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case event, ok := <-ch:
			if !ok {
				return nil
			}
			pbEvent := SessionEventToProto(event)
			if pbEvent == nil {
				continue
			}
			if err := stream.Send(pbEvent); err != nil {
				return err
			}
		}
	}
}

// ============================================================================
// Worktree Server
// ============================================================================

type worktreeServer struct {
	pb.UnimplementedBrambleWorktreeServiceServer
	svc service.WorktreeService
}

// NewWorktreeServer creates a gRPC worktree server.
func NewWorktreeServer(svc service.WorktreeService) pb.BrambleWorktreeServiceServer {
	return &worktreeServer{svc: svc}
}

func (s *worktreeServer) List(ctx context.Context, _ *pb.ListWorktreesRequest) (*pb.ListWorktreesResponse, error) {
	wts, err := s.svc.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list worktrees: %v", err)
	}
	result := make([]*pb.Worktree, len(wts))
	for i, w := range wts {
		result[i] = WorktreeToProto(w)
	}
	return &pb.ListWorktreesResponse{Worktrees: result}, nil
}

func (s *worktreeServer) GetGitStatus(ctx context.Context, req *pb.GetGitStatusRequest) (*pb.GetGitStatusResponse, error) {
	w := WorktreeFromProto(req.Worktree)
	st, err := s.svc.GetGitStatus(ctx, w)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get git status: %v", err)
	}
	return &pb.GetGitStatusResponse{
		Status:   WorktreeStatusToProto(st),
		Messages: s.svc.Messages(),
	}, nil
}

func (s *worktreeServer) FetchAllPRInfo(ctx context.Context, _ *pb.FetchAllPRInfoRequest) (*pb.FetchAllPRInfoResponse, error) {
	prs, err := s.svc.FetchAllPRInfo(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch PR info: %v", err)
	}
	result := make([]*pb.PRInfo, len(prs))
	for i, p := range prs {
		result[i] = PRInfoToProto(p)
	}
	return &pb.FetchAllPRInfoResponse{Prs: result}, nil
}

func (s *worktreeServer) NewAtomic(ctx context.Context, req *pb.NewAtomicRequest) (*pb.NewAtomicResponse, error) {
	path, err := s.svc.NewAtomic(ctx, req.Branch, req.BaseBranch, req.Goal)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create worktree: %v", err)
	}
	return &pb.NewAtomicResponse{
		Path:     path,
		Messages: s.svc.Messages(),
	}, nil
}

func (s *worktreeServer) Remove(ctx context.Context, req *pb.RemoveWorktreeRequest) (*pb.RemoveWorktreeResponse, error) {
	if err := s.svc.Remove(ctx, req.NameOrBranch, req.DeleteBranch); err != nil {
		return nil, status.Errorf(codes.Internal, "remove worktree: %v", err)
	}
	return &pb.RemoveWorktreeResponse{Messages: s.svc.Messages()}, nil
}

func (s *worktreeServer) Sync(ctx context.Context, req *pb.SyncWorktreeRequest) (*pb.SyncWorktreeResponse, error) {
	if err := s.svc.Sync(ctx, req.Branch); err != nil {
		return nil, status.Errorf(codes.Internal, "sync worktree: %v", err)
	}
	return &pb.SyncWorktreeResponse{Messages: s.svc.Messages()}, nil
}

func (s *worktreeServer) MergePRForBranch(ctx context.Context, req *pb.MergePRRequest) (*pb.MergePRResponse, error) {
	prNum, err := s.svc.MergePRForBranch(ctx, req.Branch, MergeOptionsFromProto(req.Options))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "merge PR: %v", err)
	}
	return &pb.MergePRResponse{
		PrNumber: int32(prNum),
		Messages: s.svc.Messages(),
	}, nil
}

func (s *worktreeServer) GatherContext(ctx context.Context, req *pb.GatherContextRequest) (*pb.GatherContextResponse, error) {
	w := WorktreeFromProto(req.Worktree)
	opts := ContextOptionsFromProto(req.Options)
	wtCtx, err := s.svc.GatherContext(ctx, w, opts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "gather context: %v", err)
	}
	return &pb.GatherContextResponse{Context: WorktreeContextToProto(wtCtx)}, nil
}

func (s *worktreeServer) ResetToDefault(ctx context.Context, req *pb.ResetToDefaultRequest) (*pb.ResetToDefaultResponse, error) {
	if err := s.svc.ResetToDefault(ctx, req.Branch); err != nil {
		return nil, status.Errorf(codes.Internal, "reset to default: %v", err)
	}
	return &pb.ResetToDefaultResponse{Messages: s.svc.Messages()}, nil
}

// ============================================================================
// Task Router Server
// ============================================================================

type taskRouterServer struct {
	pb.UnimplementedBrambleTaskRouterServiceServer
	svc service.TaskRouterService
}

// NewTaskRouterServer creates a gRPC task router server.
func NewTaskRouterServer(svc service.TaskRouterService) pb.BrambleTaskRouterServiceServer {
	return &taskRouterServer{svc: svc}
}

func (s *taskRouterServer) Route(ctx context.Context, req *pb.RouteTaskRequest) (*pb.RouteTaskResponse, error) {
	goReq := RouteRequestFromProto(req)
	proposal, err := s.svc.Route(ctx, goReq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "route task: %v", err)
	}
	return &pb.RouteTaskResponse{Proposal: RouteProposalToProto(proposal)}, nil
}
