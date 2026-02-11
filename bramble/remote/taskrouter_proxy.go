package remote

import (
	"context"

	"google.golang.org/grpc"

	"github.com/bazelment/yoloswe/bramble/service"
	"github.com/bazelment/yoloswe/wt/taskrouter"

	pb "github.com/bazelment/yoloswe/bramble/remote/proto"
)

// Verify TaskRouterProxy implements service.TaskRouterService at compile time.
var _ service.TaskRouterService = (*TaskRouterProxy)(nil)

// TaskRouterProxy implements service.TaskRouterService by forwarding calls to a gRPC server.
type TaskRouterProxy struct {
	client pb.BrambleTaskRouterServiceClient
}

// NewTaskRouterProxy creates a new task router proxy connected to the given gRPC connection.
func NewTaskRouterProxy(conn grpc.ClientConnInterface) *TaskRouterProxy {
	return &TaskRouterProxy{
		client: pb.NewBrambleTaskRouterServiceClient(conn),
	}
}

func (p *TaskRouterProxy) Route(ctx context.Context, req taskrouter.RouteRequest) (*taskrouter.RouteProposal, error) {
	resp, err := p.client.Route(ctx, RouteRequestToProto(req))
	if err != nil {
		return nil, err
	}
	return RouteProposalFromProto(resp.Proposal), nil
}
