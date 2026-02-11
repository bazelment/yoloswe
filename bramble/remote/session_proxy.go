package remote

import (
	"context"
	"log"
	"time"

	"google.golang.org/grpc"

	"github.com/bazelment/yoloswe/bramble/session"

	pb "github.com/bazelment/yoloswe/bramble/remote/proto"
)

// Verify SessionProxy implements session.SessionService at compile time.
var _ session.SessionService = (*SessionProxy)(nil)

// SessionProxy implements session.SessionService by forwarding calls to a gRPC server.
type SessionProxy struct {
	client pb.BrambleSessionServiceClient
	events chan interface{}
	cancel context.CancelFunc
}

// NewSessionProxy creates a new session proxy connected to the given gRPC connection.
// It starts a background goroutine to stream events from the server.
func NewSessionProxy(ctx context.Context, conn grpc.ClientConnInterface) *SessionProxy {
	ctx, cancel := context.WithCancel(ctx)
	p := &SessionProxy{
		client: pb.NewBrambleSessionServiceClient(conn),
		events: make(chan interface{}, 10000),
		cancel: cancel,
	}
	go p.streamEvents(ctx)
	return p
}

func (p *SessionProxy) streamEvents(ctx context.Context) {
	const (
		initialBackoff = 500 * time.Millisecond
		maxBackoff     = 10 * time.Second
	)
	backoff := initialBackoff

	for {
		if ctx.Err() != nil {
			return
		}
		stream, err := p.client.StreamEvents(ctx, &pb.StreamEventsRequest{})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("StreamEvents connection error: %v (retrying in %v)", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		// Connected successfully — reset backoff
		backoff = initialBackoff
		for {
			event, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("StreamEvents recv error: %v (will reconnect)", err)
				break
			}
			p.dispatchEvent(event)
		}
	}
}

func (p *SessionProxy) dispatchEvent(event *pb.SessionEvent) {
	var goEvent interface{}
	switch e := event.Event.(type) {
	case *pb.SessionEvent_StateChange:
		goEvent = session.SessionStateChangeEvent{
			SessionID: session.SessionID(e.StateChange.SessionId),
			OldStatus: session.SessionStatus(e.StateChange.OldStatus),
			NewStatus: session.SessionStatus(e.StateChange.NewStatus),
		}
	case *pb.SessionEvent_Output:
		goEvent = session.SessionOutputEvent{
			SessionID: session.SessionID(e.Output.SessionId),
			Line:      OutputLineFromProto(e.Output.Line),
		}
	default:
		return
	}

	select {
	case p.events <- goEvent:
	default:
		log.Printf("WARNING: session proxy events channel full, dropping event")
	}
}

func (p *SessionProxy) StartSession(sessionType session.SessionType, worktreePath, prompt string) (session.SessionID, error) {
	resp, err := p.client.StartSession(context.Background(), &pb.StartSessionRequest{
		SessionType:  string(sessionType),
		WorktreePath: worktreePath,
		Prompt:       prompt,
	})
	if err != nil {
		return "", err
	}
	return session.SessionID(resp.SessionId), nil
}

func (p *SessionProxy) StopSession(id session.SessionID) error {
	_, err := p.client.StopSession(context.Background(), &pb.StopSessionRequest{SessionId: string(id)})
	return err
}

func (p *SessionProxy) SendFollowUp(id session.SessionID, message string) error {
	_, err := p.client.SendFollowUp(context.Background(), &pb.SendFollowUpRequest{
		SessionId: string(id),
		Message:   message,
	})
	return err
}

func (p *SessionProxy) CompleteSession(id session.SessionID) error {
	_, err := p.client.CompleteSession(context.Background(), &pb.CompleteSessionRequest{SessionId: string(id)})
	return err
}

func (p *SessionProxy) DeleteSession(id session.SessionID) error {
	_, err := p.client.DeleteSession(context.Background(), &pb.DeleteSessionRequest{SessionId: string(id)})
	return err
}

func (p *SessionProxy) GetSessionInfo(id session.SessionID) (session.SessionInfo, bool) {
	resp, err := p.client.GetSessionInfo(context.Background(), &pb.GetSessionInfoRequest{SessionId: string(id)})
	if err != nil {
		return session.SessionInfo{}, false
	}
	if !resp.Found {
		return session.SessionInfo{}, false
	}
	return SessionInfoFromProto(resp.Info), true
}

func (p *SessionProxy) GetSessionsForWorktree(path string) []session.SessionInfo {
	resp, err := p.client.GetSessionsForWorktree(context.Background(), &pb.GetSessionsForWorktreeRequest{WorktreePath: path})
	if err != nil {
		return nil
	}
	result := make([]session.SessionInfo, len(resp.Sessions))
	for i, s := range resp.Sessions {
		result[i] = SessionInfoFromProto(s)
	}
	return result
}

func (p *SessionProxy) GetAllSessions() []session.SessionInfo {
	resp, err := p.client.GetAllSessions(context.Background(), &pb.GetAllSessionsRequest{})
	if err != nil {
		return nil
	}
	result := make([]session.SessionInfo, len(resp.Sessions))
	for i, s := range resp.Sessions {
		result[i] = SessionInfoFromProto(s)
	}
	return result
}

func (p *SessionProxy) GetSessionOutput(id session.SessionID) []session.OutputLine {
	resp, err := p.client.GetSessionOutput(context.Background(), &pb.GetSessionOutputRequest{SessionId: string(id)})
	if err != nil {
		return nil
	}
	result := make([]session.OutputLine, len(resp.Lines))
	for i, l := range resp.Lines {
		result[i] = OutputLineFromProto(l)
	}
	return result
}

func (p *SessionProxy) CountByStatus() map[session.SessionStatus]int {
	resp, err := p.client.CountByStatus(context.Background(), &pb.CountByStatusRequest{})
	if err != nil {
		return nil
	}
	result := make(map[session.SessionStatus]int, len(resp.Counts))
	for k, v := range resp.Counts {
		result[session.SessionStatus(k)] = int(v)
	}
	return result
}

func (p *SessionProxy) Events() <-chan interface{} {
	return p.events
}

func (p *SessionProxy) LoadHistorySessions(worktreeName string) ([]*session.SessionMeta, error) {
	resp, err := p.client.LoadHistorySessions(context.Background(), &pb.LoadHistorySessionsRequest{WorktreeName: worktreeName})
	if err != nil {
		return nil, err
	}
	result := make([]*session.SessionMeta, len(resp.Sessions))
	for i, m := range resp.Sessions {
		result[i] = SessionMetaFromProto(m)
	}
	return result, nil
}

func (p *SessionProxy) LoadSessionFromHistory(worktreeName string, id session.SessionID) (*session.StoredSession, error) {
	resp, err := p.client.LoadSessionFromHistory(context.Background(), &pb.LoadSessionFromHistoryRequest{
		WorktreeName: worktreeName,
		SessionId:    string(id),
	})
	if err != nil {
		return nil, err
	}
	return StoredSessionFromProto(resp.Session), nil
}

func (p *SessionProxy) IsInTmuxMode() bool {
	resp, err := p.client.IsInTmuxMode(context.Background(), &pb.IsInTmuxModeRequest{})
	if err != nil {
		return false
	}
	return resp.IsTmuxMode
}

func (p *SessionProxy) Close() {
	p.cancel()
}
