package remote

import (
	"context"

	"google.golang.org/grpc"

	"github.com/bazelment/yoloswe/bramble/service"
	"github.com/bazelment/yoloswe/wt"

	pb "github.com/bazelment/yoloswe/bramble/remote/proto"
)

// Verify WorktreeProxy implements service.WorktreeService at compile time.
var _ service.WorktreeService = (*WorktreeProxy)(nil)

// WorktreeProxy implements service.WorktreeService by forwarding calls to a gRPC server.
type WorktreeProxy struct {
	client   pb.BrambleWorktreeServiceClient
	messages []string
}

// NewWorktreeProxy creates a new worktree proxy connected to the given gRPC connection.
func NewWorktreeProxy(conn grpc.ClientConnInterface) *WorktreeProxy {
	return &WorktreeProxy{
		client: pb.NewBrambleWorktreeServiceClient(conn),
	}
}

func (p *WorktreeProxy) List(ctx context.Context) ([]wt.Worktree, error) {
	p.messages = nil
	resp, err := p.client.List(ctx, &pb.ListWorktreesRequest{})
	if err != nil {
		return nil, err
	}
	result := make([]wt.Worktree, len(resp.Worktrees))
	for i, w := range resp.Worktrees {
		result[i] = WorktreeFromProto(w)
	}
	return result, nil
}

func (p *WorktreeProxy) GetGitStatus(ctx context.Context, w wt.Worktree) (*wt.WorktreeStatus, error) {
	p.messages = nil
	resp, err := p.client.GetGitStatus(ctx, &pb.GetGitStatusRequest{Worktree: WorktreeToProto(w)})
	if err != nil {
		return nil, err
	}
	p.messages = resp.Messages
	return WorktreeStatusFromProto(resp.Status), nil
}

func (p *WorktreeProxy) FetchAllPRInfo(ctx context.Context) ([]wt.PRInfo, error) {
	p.messages = nil
	resp, err := p.client.FetchAllPRInfo(ctx, &pb.FetchAllPRInfoRequest{})
	if err != nil {
		return nil, err
	}
	result := make([]wt.PRInfo, len(resp.Prs))
	for i, pr := range resp.Prs {
		result[i] = PRInfoFromProto(pr)
	}
	return result, nil
}

func (p *WorktreeProxy) NewAtomic(ctx context.Context, branch, baseBranch, goal string) (string, error) {
	p.messages = nil
	resp, err := p.client.NewAtomic(ctx, &pb.NewAtomicRequest{
		Branch:     branch,
		BaseBranch: baseBranch,
		Goal:       goal,
	})
	if err != nil {
		return "", err
	}
	p.messages = resp.Messages
	return resp.Path, nil
}

func (p *WorktreeProxy) Remove(ctx context.Context, nameOrBranch string, deleteBranch bool) error {
	p.messages = nil
	resp, err := p.client.Remove(ctx, &pb.RemoveWorktreeRequest{
		NameOrBranch: nameOrBranch,
		DeleteBranch: deleteBranch,
	})
	if err != nil {
		return err
	}
	p.messages = resp.Messages
	return nil
}

func (p *WorktreeProxy) Sync(ctx context.Context, branch string) error {
	p.messages = nil
	resp, err := p.client.Sync(ctx, &pb.SyncWorktreeRequest{Branch: branch})
	if err != nil {
		return err
	}
	p.messages = resp.Messages
	return nil
}

func (p *WorktreeProxy) MergePRForBranch(ctx context.Context, branch string, opts wt.MergeOptions) (int, error) {
	p.messages = nil
	resp, err := p.client.MergePRForBranch(ctx, &pb.MergePRRequest{
		Branch:  branch,
		Options: MergeOptionsToProto(opts),
	})
	if err != nil {
		return 0, err
	}
	p.messages = resp.Messages
	return int(resp.PrNumber), nil
}

func (p *WorktreeProxy) GatherContext(ctx context.Context, w wt.Worktree, opts wt.ContextOptions) (*wt.WorktreeContext, error) {
	p.messages = nil
	resp, err := p.client.GatherContext(ctx, &pb.GatherContextRequest{
		Worktree: WorktreeToProto(w),
		Options:  ContextOptionsToProto(opts),
	})
	if err != nil {
		return nil, err
	}
	return WorktreeContextFromProto(resp.Context), nil
}

func (p *WorktreeProxy) ResetToDefault(ctx context.Context, branch string) error {
	p.messages = nil
	resp, err := p.client.ResetToDefault(ctx, &pb.ResetToDefaultRequest{Branch: branch})
	if err != nil {
		return err
	}
	p.messages = resp.Messages
	return nil
}

func (p *WorktreeProxy) Messages() []string {
	return p.messages
}
