package remote

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
	"github.com/bazelment/yoloswe/wt/taskrouter"

	pb "github.com/bazelment/yoloswe/bramble/remote/proto"
)

// ============================================================================
// Mock implementations
// ============================================================================

// mockSessionService implements session.SessionService for testing.
type mockSessionService struct {
	startSessionFn  func(sessionType session.SessionType, worktreePath, prompt string) (session.SessionID, error)
	stopSessionFn   func(id session.SessionID) error
	sendFollowUpFn  func(id session.SessionID, message string) error
	deleteSessionFn func(id session.SessionID) error
	sessions        map[session.SessionID]session.SessionInfo
	output          map[session.SessionID][]session.OutputLine
	events          chan interface{}
	historySession  *session.StoredSession
	loadHistoryErr  error
	loadSessionErr  error
	completeErr     error
	historyMetas    []*session.SessionMeta
	mu              sync.RWMutex
	tmuxMode        bool
}

func newMockSessionService() *mockSessionService {
	return &mockSessionService{
		sessions: make(map[session.SessionID]session.SessionInfo),
		output:   make(map[session.SessionID][]session.OutputLine),
		events:   make(chan interface{}, 1000),
	}
}

func (m *mockSessionService) StartSession(sessionType session.SessionType, worktreePath, prompt string) (session.SessionID, error) {
	if m.startSessionFn != nil {
		return m.startSessionFn(sessionType, worktreePath, prompt)
	}
	id := session.SessionID(fmt.Sprintf("sess-%d", time.Now().UnixNano()))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = session.SessionInfo{
		ID:           id,
		Type:         sessionType,
		Status:       session.StatusRunning,
		WorktreePath: worktreePath,
		Prompt:       prompt,
		CreatedAt:    time.Now(),
	}
	return id, nil
}

func (m *mockSessionService) StopSession(id session.SessionID) error {
	if m.stopSessionFn != nil {
		return m.stopSessionFn(id)
	}
	return nil
}

func (m *mockSessionService) SendFollowUp(id session.SessionID, message string) error {
	if m.sendFollowUpFn != nil {
		return m.sendFollowUpFn(id, message)
	}
	return nil
}

func (m *mockSessionService) CompleteSession(id session.SessionID) error {
	if m.completeErr != nil {
		return m.completeErr
	}
	return nil
}

func (m *mockSessionService) DeleteSession(id session.SessionID) error {
	if m.deleteSessionFn != nil {
		return m.deleteSessionFn(id)
	}
	return nil
}

func (m *mockSessionService) GetSessionInfo(id session.SessionID) (session.SessionInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info, ok := m.sessions[id]
	return info, ok
}

func (m *mockSessionService) GetSessionsForWorktree(path string) []session.SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []session.SessionInfo
	for _, s := range m.sessions {
		if s.WorktreePath == path {
			result = append(result, s)
		}
	}
	return result
}

func (m *mockSessionService) GetAllSessions() []session.SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]session.SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s)
	}
	return result
}

func (m *mockSessionService) GetSessionOutput(id session.SessionID) []session.OutputLine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.output[id]
}

func (m *mockSessionService) CountByStatus() map[session.SessionStatus]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	counts := make(map[session.SessionStatus]int)
	for _, s := range m.sessions {
		counts[s.Status]++
	}
	return counts
}

func (m *mockSessionService) Events() <-chan interface{} {
	return m.events
}

func (m *mockSessionService) LoadHistorySessions(worktreeName string) ([]*session.SessionMeta, error) {
	if m.loadHistoryErr != nil {
		return nil, m.loadHistoryErr
	}
	return m.historyMetas, nil
}

func (m *mockSessionService) LoadSessionFromHistory(worktreeName string, id session.SessionID) (*session.StoredSession, error) {
	if m.loadSessionErr != nil {
		return nil, m.loadSessionErr
	}
	return m.historySession, nil
}

func (m *mockSessionService) IsInTmuxMode() bool {
	return m.tmuxMode
}

func (m *mockSessionService) Close() {}

// mockWorktreeService implements service.WorktreeService for testing.
type mockWorktreeService struct {
	statusMap  map[string]*wt.WorktreeStatus
	contextMap map[string]*wt.WorktreeContext

	newAtomicErr  error
	removeErr     error
	syncErr       error
	mergeErr      error
	listErr       error
	statusErr     error
	prErr         error
	contextErr    error
	resetErr      error
	newAtomicPath string

	worktrees []wt.Worktree
	prInfos   []wt.PRInfo
	msgs      []string

	mergeResult int
}

func newMockWorktreeService() *mockWorktreeService {
	return &mockWorktreeService{
		statusMap:  make(map[string]*wt.WorktreeStatus),
		contextMap: make(map[string]*wt.WorktreeContext),
	}
}

func (m *mockWorktreeService) List(_ context.Context) ([]wt.Worktree, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.worktrees, nil
}

func (m *mockWorktreeService) GetGitStatus(_ context.Context, w wt.Worktree) (*wt.WorktreeStatus, error) {
	if m.statusErr != nil {
		return nil, m.statusErr
	}
	if st, ok := m.statusMap[w.Path]; ok {
		return st, nil
	}
	return &wt.WorktreeStatus{Worktree: w}, nil
}

func (m *mockWorktreeService) FetchAllPRInfo(_ context.Context) ([]wt.PRInfo, error) {
	if m.prErr != nil {
		return nil, m.prErr
	}
	return m.prInfos, nil
}

func (m *mockWorktreeService) NewAtomic(_ context.Context, branch, baseBranch, goal string) (string, error) {
	if m.newAtomicErr != nil {
		return "", m.newAtomicErr
	}
	m.msgs = []string{fmt.Sprintf("Created worktree %s from %s", branch, baseBranch)}
	return m.newAtomicPath, nil
}

func (m *mockWorktreeService) Remove(_ context.Context, nameOrBranch string, deleteBranch bool) error {
	if m.removeErr != nil {
		return m.removeErr
	}
	m.msgs = []string{fmt.Sprintf("Removed worktree %s", nameOrBranch)}
	return nil
}

func (m *mockWorktreeService) Sync(_ context.Context, branch string) error {
	if m.syncErr != nil {
		return m.syncErr
	}
	m.msgs = []string{fmt.Sprintf("Synced %s", branch)}
	return nil
}

func (m *mockWorktreeService) MergePRForBranch(_ context.Context, branch string, opts wt.MergeOptions) (int, error) {
	if m.mergeErr != nil {
		return 0, m.mergeErr
	}
	m.msgs = []string{fmt.Sprintf("Merged PR for %s", branch)}
	return m.mergeResult, nil
}

func (m *mockWorktreeService) GatherContext(_ context.Context, w wt.Worktree, opts wt.ContextOptions) (*wt.WorktreeContext, error) {
	if m.contextErr != nil {
		return nil, m.contextErr
	}
	if ctx, ok := m.contextMap[w.Path]; ok {
		return ctx, nil
	}
	return &wt.WorktreeContext{Path: w.Path, Branch: w.Branch}, nil
}

func (m *mockWorktreeService) ResetToDefault(_ context.Context, branch string) error {
	if m.resetErr != nil {
		return m.resetErr
	}
	m.msgs = []string{fmt.Sprintf("Reset %s to default", branch)}
	return nil
}

func (m *mockWorktreeService) Messages() []string {
	return m.msgs
}

// mockTaskRouterService implements service.TaskRouterService for testing.
type mockTaskRouterService struct {
	proposal *taskrouter.RouteProposal
	err      error
}

func (m *mockTaskRouterService) Route(_ context.Context, req taskrouter.RouteRequest) (*taskrouter.RouteProposal, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.proposal, nil
}

// ============================================================================
// Test helpers
// ============================================================================

// testEnv holds the test gRPC server and client connection.
type testEnv struct {
	server      *grpc.Server
	conn        *grpc.ClientConn
	sessionSvc  *mockSessionService
	wtSvc       *mockWorktreeService
	trSvc       *mockTaskRouterService
	broadcaster *EventBroadcaster
	cancel      context.CancelFunc
}

// setupTestEnv starts a gRPC server with auth on a random port and returns
// a connected client that uses the same token.
func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	sessionSvc := newMockSessionService()
	wtSvc := newMockWorktreeService()
	trSvc := &mockTaskRouterService{}

	broadcaster := NewEventBroadcaster()

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	// Generate a per-test token so tests are isolated.
	token, err := GenerateToken()
	require.NoError(t, err)

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(TokenAuthInterceptor(token)),
		grpc.StreamInterceptor(TokenStreamInterceptor(token)),
	)
	pb.RegisterBrambleSessionServiceServer(srv, NewSessionServer(sessionSvc, broadcaster))
	pb.RegisterBrambleWorktreeServiceServer(srv, NewWorktreeServer(wtSvc))
	pb.RegisterBrambleTaskRouterServiceServer(srv, NewTaskRouterServer(trSvc))

	go func() {
		_ = srv.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(TokenCallCredentials(token)),
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go broadcaster.Run(ctx, sessionSvc.events)

	t.Cleanup(func() {
		cancel()
		conn.Close()
		srv.GracefulStop()
	})

	return &testEnv{
		server:      srv,
		conn:        conn,
		sessionSvc:  sessionSvc,
		wtSvc:       wtSvc,
		trSvc:       trSvc,
		broadcaster: broadcaster,
		cancel:      cancel,
	}
}

// ============================================================================
// Session server + proxy end-to-end tests
// ============================================================================

func TestE2E_StartSession(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	id, err := proxy.StartSession(session.SessionTypeBuilder, "/path/to/wt", "Fix the bug")
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Verify the session was created on the server side
	info, ok := env.sessionSvc.GetSessionInfo(id)
	assert.True(t, ok)
	assert.Equal(t, session.SessionTypeBuilder, info.Type)
	assert.Equal(t, "/path/to/wt", info.WorktreePath)
	assert.Equal(t, "Fix the bug", info.Prompt)
}

func TestE2E_StartSession_Error(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.startSessionFn = func(sessionType session.SessionType, worktreePath, prompt string) (session.SessionID, error) {
		return "", fmt.Errorf("worktree not found")
	}

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	_, err := proxy.StartSession(session.SessionTypePlanner, "/bad/path", "plan")
	assert.Error(t, err)
}

func TestE2E_StopSession(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	stopped := false
	env.sessionSvc.stopSessionFn = func(id session.SessionID) error {
		if id == "sess-stop" {
			stopped = true
			return nil
		}
		return fmt.Errorf("unknown session")
	}

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	err := proxy.StopSession("sess-stop")
	require.NoError(t, err)
	assert.True(t, stopped)
}

func TestE2E_SendFollowUp(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	var receivedMsg string
	env.sessionSvc.sendFollowUpFn = func(id session.SessionID, message string) error {
		receivedMsg = message
		return nil
	}

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	err := proxy.SendFollowUp("sess-1", "please continue")
	require.NoError(t, err)
	assert.Equal(t, "please continue", receivedMsg)
}

func TestE2E_CompleteSession(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	err := proxy.CompleteSession("sess-1")
	require.NoError(t, err)
}

func TestE2E_CompleteSession_Error(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.completeErr = fmt.Errorf("session not idle")

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	err := proxy.CompleteSession("sess-1")
	assert.Error(t, err)
}

func TestE2E_DeleteSession(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	var deletedID session.SessionID
	env.sessionSvc.deleteSessionFn = func(id session.SessionID) error {
		deletedID = id
		return nil
	}

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	err := proxy.DeleteSession("sess-del")
	require.NoError(t, err)
	assert.Equal(t, session.SessionID("sess-del"), deletedID)
}

func TestE2E_GetSessionInfo_Found(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.mu.Lock()
	env.sessionSvc.sessions["sess-info"] = session.SessionInfo{
		ID:           "sess-info",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/wt/path",
		Prompt:       "test prompt",
		Model:        "claude-opus-4",
		CreatedAt:    time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.mu.Unlock()

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	info, ok := proxy.GetSessionInfo("sess-info")
	assert.True(t, ok)
	assert.Equal(t, session.SessionID("sess-info"), info.ID)
	assert.Equal(t, session.SessionTypePlanner, info.Type)
	assert.Equal(t, session.StatusRunning, info.Status)
	assert.Equal(t, "/wt/path", info.WorktreePath)
	assert.Equal(t, "test prompt", info.Prompt)
	assert.Equal(t, "claude-opus-4", info.Model)
}

func TestE2E_GetSessionInfo_NotFound(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	_, ok := proxy.GetSessionInfo("nonexistent")
	assert.False(t, ok)
}

func TestE2E_GetSessionsForWorktree(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.mu.Lock()
	env.sessionSvc.sessions["s1"] = session.SessionInfo{
		ID:           "s1",
		WorktreePath: "/wt/feature-a",
		CreatedAt:    time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.sessions["s2"] = session.SessionInfo{
		ID:           "s2",
		WorktreePath: "/wt/feature-b",
		CreatedAt:    time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.sessions["s3"] = session.SessionInfo{
		ID:           "s3",
		WorktreePath: "/wt/feature-a",
		CreatedAt:    time.Date(2025, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.mu.Unlock()

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	sessions := proxy.GetSessionsForWorktree("/wt/feature-a")
	assert.Len(t, sessions, 2)
	for _, s := range sessions {
		assert.Equal(t, "/wt/feature-a", s.WorktreePath)
	}
}

func TestE2E_GetAllSessions(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.mu.Lock()
	env.sessionSvc.sessions["s1"] = session.SessionInfo{
		ID:        "s1",
		CreatedAt: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.sessions["s2"] = session.SessionInfo{
		ID:        "s2",
		CreatedAt: time.Date(2025, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.mu.Unlock()

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	sessions := proxy.GetAllSessions()
	assert.Len(t, sessions, 2)
}

func TestE2E_GetSessionOutput(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.mu.Lock()
	env.sessionSvc.output["sess-out"] = []session.OutputLine{
		{
			Timestamp: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
			Type:      session.OutputTypeText,
			Content:   "Hello from session",
		},
		{
			Timestamp: time.Date(2025, 6, 1, 10, 0, 1, 0, time.UTC),
			Type:      session.OutputTypeToolStart,
			ToolName:  "edit_file",
			ToolID:    "tool-1",
		},
	}
	env.sessionSvc.mu.Unlock()

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	lines := proxy.GetSessionOutput("sess-out")
	require.Len(t, lines, 2)
	assert.Equal(t, "Hello from session", lines[0].Content)
	assert.Equal(t, session.OutputTypeText, lines[0].Type)
	assert.Equal(t, "edit_file", lines[1].ToolName)
	assert.Equal(t, "tool-1", lines[1].ToolID)
}

func TestE2E_CountByStatus(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.mu.Lock()
	env.sessionSvc.sessions["s1"] = session.SessionInfo{
		ID:        "s1",
		Status:    session.StatusRunning,
		CreatedAt: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.sessions["s2"] = session.SessionInfo{
		ID:        "s2",
		Status:    session.StatusRunning,
		CreatedAt: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.sessions["s3"] = session.SessionInfo{
		ID:        "s3",
		Status:    session.StatusCompleted,
		CreatedAt: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	env.sessionSvc.mu.Unlock()

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	counts := proxy.CountByStatus()
	assert.Equal(t, 2, counts[session.StatusRunning])
	assert.Equal(t, 1, counts[session.StatusCompleted])
}

func TestE2E_IsInTmuxMode(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.tmuxMode = true

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	assert.True(t, proxy.IsInTmuxMode())
}

func TestE2E_IsInTmuxMode_False(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.tmuxMode = false

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	assert.False(t, proxy.IsInTmuxMode())
}

func TestE2E_LoadHistorySessions(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	completed := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	env.sessionSvc.historyMetas = []*session.SessionMeta{
		{
			ID:           "hist-1",
			Type:         session.SessionTypeBuilder,
			Status:       session.StatusCompleted,
			WorktreeName: "feature-x",
			Prompt:       "Do something",
			CreatedAt:    time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
			CompletedAt:  &completed,
		},
	}

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	metas, err := proxy.LoadHistorySessions("feature-x")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, session.SessionID("hist-1"), metas[0].ID)
	assert.Equal(t, "Do something", metas[0].Prompt)
}

func TestE2E_LoadHistorySessions_Error(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	env.sessionSvc.loadHistoryErr = fmt.Errorf("storage error")

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	_, err := proxy.LoadHistorySessions("feature-x")
	assert.Error(t, err)
}

func TestE2E_LoadSessionFromHistory(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.sessionSvc.historySession = &session.StoredSession{
		ID:           "stored-1",
		Type:         session.SessionTypeBuilder,
		Status:       session.StatusCompleted,
		RepoName:     "repo",
		WorktreeName: "feature-x",
		Prompt:       "Build feature",
		CreatedAt:    time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
		Progress: &session.StoredProgress{
			TurnCount:    5,
			TotalCostUSD: 1.0,
		},
		Output: []session.OutputLine{
			{
				Timestamp: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
				Type:      session.OutputTypeText,
				Content:   "Working...",
			},
		},
	}

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	stored, err := proxy.LoadSessionFromHistory("feature-x", "stored-1")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, session.SessionID("stored-1"), stored.ID)
	assert.Equal(t, "Build feature", stored.Prompt)
	require.NotNil(t, stored.Progress)
	assert.Equal(t, 5, stored.Progress.TurnCount)
	require.Len(t, stored.Output, 1)
	assert.Equal(t, "Working...", stored.Output[0].Content)
}

// ============================================================================
// StreamEvents end-to-end test
// ============================================================================

func TestE2E_StreamEvents(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	// Give the proxy's StreamEvents goroutine time to connect
	time.Sleep(100 * time.Millisecond)

	// Send events through the mock service events channel
	env.sessionSvc.events <- session.SessionStateChangeEvent{
		SessionID: "sess-event-1",
		OldStatus: session.StatusPending,
		NewStatus: session.StatusRunning,
	}

	env.sessionSvc.events <- session.SessionOutputEvent{
		SessionID: "sess-event-1",
		Line: session.OutputLine{
			Timestamp: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
			Type:      session.OutputTypeText,
			Content:   "Hello from event",
		},
	}

	// The proxy should receive these events via its Events() channel
	eventsCh := proxy.Events()

	// Wait for the state change event
	require.Eventually(t, func() bool {
		return len(eventsCh) >= 2
	}, 5*time.Second, 50*time.Millisecond, "expected 2 events to arrive")

	event1 := <-eventsCh
	stateChange, ok := event1.(session.SessionStateChangeEvent)
	require.True(t, ok, "expected SessionStateChangeEvent, got %T", event1)
	assert.Equal(t, session.SessionID("sess-event-1"), stateChange.SessionID)
	assert.Equal(t, session.StatusPending, stateChange.OldStatus)
	assert.Equal(t, session.StatusRunning, stateChange.NewStatus)

	event2 := <-eventsCh
	outputEvent, ok := event2.(session.SessionOutputEvent)
	require.True(t, ok, "expected SessionOutputEvent, got %T", event2)
	assert.Equal(t, session.SessionID("sess-event-1"), outputEvent.SessionID)
	assert.Equal(t, "Hello from event", outputEvent.Line.Content)
}

// ============================================================================
// Worktree server + proxy end-to-end tests
// ============================================================================

func TestE2E_Worktree_List(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.wtSvc.worktrees = []wt.Worktree{
		{Path: "/wt/main", Branch: "main", Commit: "abc123"},
		{Path: "/wt/feature-x", Branch: "feature-x", Commit: "def456", IsDetached: false},
	}

	proxy := NewWorktreeProxy(env.conn)

	worktrees, err := proxy.List(context.Background())
	require.NoError(t, err)
	require.Len(t, worktrees, 2)
	assert.Equal(t, "main", worktrees[0].Branch)
	assert.Equal(t, "abc123", worktrees[0].Commit)
	assert.Equal(t, "feature-x", worktrees[1].Branch)
}

func TestE2E_Worktree_List_Error(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	env.wtSvc.listErr = fmt.Errorf("permission denied")

	proxy := NewWorktreeProxy(env.conn)
	_, err := proxy.List(context.Background())
	assert.Error(t, err)
}

func TestE2E_Worktree_GetGitStatus(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	w := wt.Worktree{Path: "/wt/feature-x", Branch: "feature-x", Commit: "abc123"}
	env.wtSvc.statusMap["/wt/feature-x"] = &wt.WorktreeStatus{
		Worktree:       w,
		IsDirty:        true,
		Ahead:          2,
		Behind:         1,
		PRNumber:       42,
		PRURL:          "https://github.com/org/repo/pull/42",
		PRState:        "OPEN",
		PRReviewStatus: "APPROVED",
		LastCommitMsg:  "fix: typo",
		LastCommitTime: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}

	proxy := NewWorktreeProxy(env.conn)
	status, err := proxy.GetGitStatus(context.Background(), w)
	require.NoError(t, err)
	require.NotNil(t, status)

	assert.True(t, status.IsDirty)
	assert.Equal(t, 2, status.Ahead)
	assert.Equal(t, 1, status.Behind)
	assert.Equal(t, 42, status.PRNumber)
	assert.Equal(t, "OPEN", status.PRState)
	assert.Equal(t, "APPROVED", status.PRReviewStatus)
	assert.Equal(t, "fix: typo", status.LastCommitMsg)
}

func TestE2E_Worktree_GetGitStatus_WithMessages(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	w := wt.Worktree{Path: "/wt/main", Branch: "main"}
	env.wtSvc.msgs = []string{"status checked"}

	proxy := NewWorktreeProxy(env.conn)
	_, err := proxy.GetGitStatus(context.Background(), w)
	require.NoError(t, err)

	msgs := proxy.Messages()
	assert.Contains(t, msgs, "status checked")
}

func TestE2E_Worktree_FetchAllPRInfo(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.wtSvc.prInfos = []wt.PRInfo{
		{
			URL:            "https://github.com/org/repo/pull/1",
			HeadRefName:    "feature-a",
			BaseRefName:    "main",
			State:          "OPEN",
			Number:         1,
			ReviewDecision: "APPROVED",
			IsDraft:        false,
		},
		{
			URL:         "https://github.com/org/repo/pull/2",
			HeadRefName: "feature-b",
			BaseRefName: "main",
			State:       "OPEN",
			Number:      2,
			IsDraft:     true,
		},
	}

	proxy := NewWorktreeProxy(env.conn)
	prs, err := proxy.FetchAllPRInfo(context.Background())
	require.NoError(t, err)
	require.Len(t, prs, 2)

	assert.Equal(t, 1, prs[0].Number)
	assert.Equal(t, "feature-a", prs[0].HeadRefName)
	assert.Equal(t, "APPROVED", prs[0].ReviewDecision)
	assert.False(t, prs[0].IsDraft)

	assert.Equal(t, 2, prs[1].Number)
	assert.True(t, prs[1].IsDraft)
}

func TestE2E_Worktree_NewAtomic(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.wtSvc.newAtomicPath = "/wt/feature-new"

	proxy := NewWorktreeProxy(env.conn)
	path, err := proxy.NewAtomic(context.Background(), "feature-new", "main", "Implement login")
	require.NoError(t, err)
	assert.Equal(t, "/wt/feature-new", path)

	msgs := proxy.Messages()
	require.NotEmpty(t, msgs)
}

func TestE2E_Worktree_NewAtomic_Error(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	env.wtSvc.newAtomicErr = fmt.Errorf("branch already exists")

	proxy := NewWorktreeProxy(env.conn)
	_, err := proxy.NewAtomic(context.Background(), "feature-x", "main", "goal")
	assert.Error(t, err)
}

func TestE2E_Worktree_Remove(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	proxy := NewWorktreeProxy(env.conn)
	err := proxy.Remove(context.Background(), "feature-old", true)
	require.NoError(t, err)

	msgs := proxy.Messages()
	require.NotEmpty(t, msgs)
	assert.Contains(t, msgs[0], "Removed worktree feature-old")
}

func TestE2E_Worktree_Sync(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	proxy := NewWorktreeProxy(env.conn)
	err := proxy.Sync(context.Background(), "feature-x")
	require.NoError(t, err)

	msgs := proxy.Messages()
	require.NotEmpty(t, msgs)
	assert.Contains(t, msgs[0], "Synced feature-x")
}

func TestE2E_Worktree_MergePRForBranch(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	env.wtSvc.mergeResult = 42

	proxy := NewWorktreeProxy(env.conn)
	prNum, err := proxy.MergePRForBranch(context.Background(), "feature-x", wt.MergeOptions{
		MergeMethod: "squash",
		Keep:        true,
	})
	require.NoError(t, err)
	assert.Equal(t, 42, prNum)
}

func TestE2E_Worktree_GatherContext(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	w := wt.Worktree{Path: "/wt/feature-x", Branch: "feature-x"}
	env.wtSvc.contextMap["/wt/feature-x"] = &wt.WorktreeContext{
		Path:         "/wt/feature-x",
		Branch:       "feature-x",
		Goal:         "Implement login",
		Parent:       "main",
		IsDirty:      true,
		Ahead:        3,
		ChangedFiles: []string{"login.go"},
		RecentCommits: []wt.CommitInfo{
			{Hash: "aaa", Subject: "init", Author: "Alice", Date: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)},
		},
		GatheredAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
	}

	proxy := NewWorktreeProxy(env.conn)
	ctx, err := proxy.GatherContext(context.Background(), w, wt.ContextOptions{
		IncludeDiff:    true,
		IncludeCommits: 10,
	})
	require.NoError(t, err)
	require.NotNil(t, ctx)

	assert.Equal(t, "feature-x", ctx.Branch)
	assert.Equal(t, "Implement login", ctx.Goal)
	assert.Equal(t, "main", ctx.Parent)
	assert.True(t, ctx.IsDirty)
	assert.Equal(t, 3, ctx.Ahead)
	assert.Equal(t, []string{"login.go"}, ctx.ChangedFiles)
	require.Len(t, ctx.RecentCommits, 1)
	assert.Equal(t, "aaa", ctx.RecentCommits[0].Hash)
}

func TestE2E_Worktree_ResetToDefault(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	proxy := NewWorktreeProxy(env.conn)
	err := proxy.ResetToDefault(context.Background(), "feature-old")
	require.NoError(t, err)

	msgs := proxy.Messages()
	require.NotEmpty(t, msgs)
}

func TestE2E_Worktree_ResetToDefault_Error(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	env.wtSvc.resetErr = fmt.Errorf("branch not found")

	proxy := NewWorktreeProxy(env.conn)
	err := proxy.ResetToDefault(context.Background(), "nonexistent")
	assert.Error(t, err)
}

// ============================================================================
// Task router server + proxy end-to-end tests
// ============================================================================

func TestE2E_TaskRouter_Route_CreateNew(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.trSvc.proposal = &taskrouter.RouteProposal{
		Action:    taskrouter.ActionCreateNew,
		Worktree:  "feature-login",
		Parent:    "main",
		Reasoning: "No existing worktree for login feature",
	}

	proxy := NewTaskRouterProxy(env.conn)

	proposal, err := proxy.Route(context.Background(), taskrouter.RouteRequest{
		Prompt:   "Build a login page",
		RepoName: "my-repo",
		Worktrees: []taskrouter.WorktreeInfo{
			{Name: "feature-ui", Goal: "Dashboard"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, proposal)

	assert.Equal(t, taskrouter.ActionCreateNew, proposal.Action)
	assert.Equal(t, "feature-login", proposal.Worktree)
	assert.Equal(t, "main", proposal.Parent)
	assert.Equal(t, "No existing worktree for login feature", proposal.Reasoning)
}

func TestE2E_TaskRouter_Route_UseExisting(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.trSvc.proposal = &taskrouter.RouteProposal{
		Action:    taskrouter.ActionUseExisting,
		Worktree:  "feature-auth",
		Reasoning: "Auth worktree is the best match",
	}

	proxy := NewTaskRouterProxy(env.conn)

	proposal, err := proxy.Route(context.Background(), taskrouter.RouteRequest{
		Prompt:    "Fix auth bug",
		CurrentWT: "feature-auth",
		RepoName:  "my-repo",
	})
	require.NoError(t, err)
	require.NotNil(t, proposal)

	assert.Equal(t, taskrouter.ActionUseExisting, proposal.Action)
	assert.Equal(t, "feature-auth", proposal.Worktree)
}

func TestE2E_TaskRouter_Route_Error(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	env.trSvc.err = fmt.Errorf("AI router unavailable")

	proxy := NewTaskRouterProxy(env.conn)

	_, err := proxy.Route(context.Background(), taskrouter.RouteRequest{
		Prompt: "Do something",
	})
	assert.Error(t, err)
}

func TestE2E_TaskRouter_Route_NilProposal(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	env.trSvc.proposal = nil

	proxy := NewTaskRouterProxy(env.conn)

	proposal, err := proxy.Route(context.Background(), taskrouter.RouteRequest{
		Prompt: "Do something",
	})
	require.NoError(t, err)
	assert.Nil(t, proposal)
}

// ============================================================================
// Proxy Messages() reset test
// ============================================================================

func TestE2E_WorktreeProxy_MessagesReset(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.wtSvc.worktrees = []wt.Worktree{
		{Path: "/wt/main", Branch: "main"},
	}

	proxy := NewWorktreeProxy(env.conn)

	// First call sets messages
	_, err := proxy.List(context.Background())
	require.NoError(t, err)

	// Messages should be reset on each new call
	// (WorktreeProxy resets messages at the start of each call)
	msgs := proxy.Messages()
	// List doesn't populate messages since it doesn't go through managerWithCapture
	// but it does reset them to nil
	assert.Nil(t, msgs)
}

// ============================================================================
// Full roundtrip: proxy -> gRPC -> server -> mock service
// ============================================================================

func TestE2E_FullSessionLifecycle(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	proxy := NewSessionProxy(context.Background(), env.conn)
	defer proxy.Close()

	// Start a session
	id, err := proxy.StartSession(session.SessionTypeBuilder, "/wt/feature-x", "Fix the bug")
	require.NoError(t, err)
	require.NotEmpty(t, id)

	// Verify it shows up in GetAllSessions
	allSessions := proxy.GetAllSessions()
	require.Len(t, allSessions, 1)
	assert.Equal(t, id, allSessions[0].ID)

	// Get session info
	info, ok := proxy.GetSessionInfo(id)
	assert.True(t, ok)
	assert.Equal(t, session.StatusRunning, info.Status)

	// Send follow-up
	err = proxy.SendFollowUp(id, "continue with tests")
	require.NoError(t, err)

	// Check CountByStatus
	counts := proxy.CountByStatus()
	assert.Equal(t, 1, counts[session.StatusRunning])

	// Stop the session
	err = proxy.StopSession(id)
	require.NoError(t, err)
}

func TestE2E_FullWorktreeWorkflow(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.wtSvc.worktrees = []wt.Worktree{
		{Path: "/wt/main", Branch: "main", Commit: "abc123"},
	}
	env.wtSvc.newAtomicPath = "/wt/feature-new"
	env.wtSvc.mergeResult = 99

	proxy := NewWorktreeProxy(env.conn)

	// List worktrees
	worktrees, err := proxy.List(context.Background())
	require.NoError(t, err)
	require.Len(t, worktrees, 1)

	// Create new worktree
	path, err := proxy.NewAtomic(context.Background(), "feature-new", "main", "Do stuff")
	require.NoError(t, err)
	assert.Equal(t, "/wt/feature-new", path)

	// Sync
	err = proxy.Sync(context.Background(), "feature-new")
	require.NoError(t, err)

	// Merge
	prNum, err := proxy.MergePRForBranch(context.Background(), "feature-new", wt.MergeOptions{MergeMethod: "squash"})
	require.NoError(t, err)
	assert.Equal(t, 99, prNum)

	// Remove
	err = proxy.Remove(context.Background(), "feature-new", true)
	require.NoError(t, err)
}
