package meetingbot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestParseTranscript(t *testing.T) {
	input := strings.NewReader(`[00:02-00:05] Igor: You can start.
continuation line

[00:06-00:50] Ming: Hey, folks.`)

	events, err := ParseTranscript(input)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "Igor", events[0].Speaker)
	require.Contains(t, events[0].Text, "continuation line")
	require.Equal(t, 2*time.Second, events[0].Start)
	require.Equal(t, 50*time.Second, events[1].End)
}

func TestBuildBackgroundUsesLayeredResearchAgents(t *testing.T) {
	client := &recordingClient{}
	cfg := DefaultConfig()
	cfg.AutoResearch = false
	cfg.MaxResearchTopics = 1
	cfg.ResearchScopes = []ResearchScope{ScopeInternal, ScopeCodebase, ScopeWeb}
	cfg.ResearchModel = "sonnet"
	cfg.CodeResearchModel = "gpt-5.3-codex"
	cfg.WebResearchModel = "gpt-5.2"

	bot := New(client, cfg)
	require.NoError(t, bot.IngestTranscript(context.Background(), sampleEvents()))
	require.NoError(t, bot.BuildBackground(context.Background()))

	requests := client.Requests()
	require.Len(t, requests, 3)
	require.Equal(t, RoleInternalResearch, requests[0].Role)
	require.Equal(t, "sonnet", requests[0].Model)
	require.Equal(t, RoleCodebaseResearch, requests[1].Role)
	require.Equal(t, "gpt-5.3-codex", requests[1].Model)
	require.Equal(t, "plan", requests[1].PermissionMode)
	require.Equal(t, RoleWebResearch, requests[2].Role)
	require.Equal(t, "gpt-5.2", requests[2].Model)
	require.Len(t, bot.Evidence(), 3)
}

func TestObserveRunsBackgroundResearchAtChunkBoundary(t *testing.T) {
	client := &recordingClient{}
	cfg := DefaultConfig()
	cfg.AutoResearch = true
	cfg.ResearchChunkEvents = 2
	cfg.MaxResearchTopics = 1
	cfg.ResearchScopes = []ResearchScope{ScopeInternal}

	bot := New(client, cfg)
	events := sampleEvents()
	require.NoError(t, bot.Observe(context.Background(), events[0]))
	require.Empty(t, client.Requests())
	require.NoError(t, bot.Observe(context.Background(), events[1]))

	requests := client.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, RoleInternalResearch, requests[0].Role)
}

func TestAnswerQuestionStreamsFastOpeningBeforeSlowAgent(t *testing.T) {
	client := &recordingClient{delay: 25 * time.Millisecond}
	cfg := DefaultConfig()
	cfg.AutoResearch = false
	cfg.FastAnswerModel = "gpt-5.3-codex"
	cfg.FastAnswerEffort = agent.EffortLow
	cfg.FastAnswerTimeout = time.Second

	bot := New(client, cfg)
	require.NoError(t, bot.IngestTranscript(context.Background(), sampleEvents()))
	answer, err := bot.AnswerQuestion(context.Background(), "What should we do about preview failures?")
	require.NoError(t, err)

	require.Less(t, answer.First10WordsLatency, 10*time.Millisecond)
	require.Contains(t, answer.Opening, "preview")
	require.Contains(t, answer.Text, answer.Opening)

	requests := client.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, RoleFastAnswer, requests[0].Role)
	require.Equal(t, agent.EffortLow, requests[0].Effort)
	require.Equal(t, "gpt-5.3-codex", requests[0].Model)
}

func TestSummarizeMeetingUsesHighEffortSummaryAgent(t *testing.T) {
	client := &recordingClient{}
	cfg := DefaultConfig()
	cfg.AutoResearch = false
	cfg.SummaryModel = "gpt-5.5"
	cfg.SummaryEffort = agent.EffortHigh

	bot := New(client, cfg)
	require.NoError(t, bot.IngestTranscript(context.Background(), sampleEvents()))
	summary, err := bot.SummarizeMeeting(context.Background())
	require.NoError(t, err)
	require.Contains(t, summary.Text, "summary")

	requests := client.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, RoleSummary, requests[0].Role)
	require.Equal(t, "gpt-5.5", requests[0].Model)
	require.Equal(t, agent.EffortHigh, requests[0].Effort)
}

func sampleEvents() []MeetingEvent {
	input := strings.NewReader(`[00:02-00:05] Igor: You can start.
[00:54-01:13] Ming: There were deployment-related issues because new services did not have proper setup in staging.
[03:43-03:56] Ming: Today I will continue following up the custom app build part.
[11:45-11:56] Igor: There is a problem with preview; two apps out of three work, but one is weird.
[12:11-12:26] Ashudeep: First resolve why request deployment failed and remove the production-only flag.
[13:30-13:42] Ashudeep: This is an auth preview problem; quick fix would be removing the full screen viewer.
[11:09-11:33] Sri: Customers want multi-department approval workflows with human review and approvals.`)
	events, err := ParseTranscript(input)
	if err != nil {
		panic(err)
	}
	return events
}

type recordingClient struct { //nolint:govet // fieldalignment: test fixture readability.
	mu       sync.Mutex
	requests []AgentRequest
	delay    time.Duration
}

func (c *recordingClient) Run(ctx context.Context, req AgentRequest) (AgentResponse, error) {
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return AgentResponse{}, ctx.Err()
		}
	}
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()

	text := "summary response"
	if req.Role == RoleFastAnswer {
		text = "final answer response"
	}
	if req.Role == RoleInternalResearch || req.Role == RoleCodebaseResearch || req.Role == RoleWebResearch {
		text = "research response for " + string(req.Role)
	}
	return AgentResponse{Text: text, Model: req.Model, Provider: "test"}, nil
}

func (c *recordingClient) Requests() []AgentRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]AgentRequest(nil), c.requests...)
}
