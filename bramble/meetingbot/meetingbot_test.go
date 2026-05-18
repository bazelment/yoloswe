package meetingbot

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestParseTranscript(t *testing.T) {
	input := strings.NewReader(`[00:02-00:05] Speaker A: You can start.
continuation line

[00:06-00:50] Speaker B: Hey, folks.`)

	events, err := ParseTranscript(input)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "Speaker A", events[0].Speaker)
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
	byRole := make(map[AgentRole]AgentRequest, len(requests))
	for _, req := range requests {
		byRole[req.Role] = req
	}
	require.Equal(t, "sonnet", byRole[RoleInternalResearch].Model)
	require.Equal(t, "gpt-5.3-codex", byRole[RoleCodebaseResearch].Model)
	require.Equal(t, "plan", byRole[RoleCodebaseResearch].PermissionMode)
	require.Equal(t, "gpt-5.2", byRole[RoleWebResearch].Model)
	require.Len(t, bot.Evidence(), 3)
}

func TestLiveProfilesValidateBoundaries(t *testing.T) {
	liveSafe := LiveSafeConfig()
	require.NoError(t, liveSafe.Validate())
	require.False(t, liveSafe.AutoResearch)
	require.NotContains(t, liveSafe.ResearchScopes, ScopeWeb)

	liveSafe.AutoResearch = true
	require.ErrorIs(t, liveSafe.Validate(), errLiveAutoResearch)

	liveSafe = LiveSafeConfig()
	liveSafe.ResearchScopes = append(liveSafe.ResearchScopes, ScopeWeb)
	require.ErrorIs(t, liveSafe.Validate(), errLiveSafeWeb)

	liveWeb := LiveWebConfig()
	require.NoError(t, liveWeb.Validate())
	require.Contains(t, liveWeb.ResearchScopes, ScopeWeb)

	bot, err := NewValidated(LocalAgentClient{}, Config{Profile: ProfileLiveSafe})
	require.NoError(t, err)
	require.NotContains(t, bot.cfg.ResearchScopes, ScopeWeb)
}

func TestAutoResearchRequiresScheduler(t *testing.T) {
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
	require.ErrorIs(t, bot.Observe(context.Background(), events[1]), errAutoResearchScheduler)
	require.Empty(t, client.Requests())
}

func TestObserveQueuesResearchWhenSchedulerConfigured(t *testing.T) {
	client := &recordingClient{}
	scheduler := &recordingScheduler{}
	cfg := ReplayConfig()
	cfg.AutoResearch = true
	cfg.ResearchChunkEvents = 2
	cfg.MaxResearchTopics = 1
	cfg.ResearchScopes = []ResearchScope{ScopeInternal}
	cfg.ResearchScheduler = scheduler

	bot := New(client, cfg)
	events := sampleEvents()
	require.NoError(t, bot.Observe(context.Background(), events[0]))
	require.Empty(t, scheduler.Work())
	require.NoError(t, bot.Observe(context.Background(), events[1]))

	require.Empty(t, client.Requests())
	work := scheduler.Work()
	require.Len(t, work, 1)
	require.Equal(t, []ResearchScope{ScopeInternal}, work[0].Job.Scopes)
	require.Equal(t, 0, work[0].Job.TranscriptStart)
	require.Equal(t, 1, work[0].Job.TranscriptEnd)
	require.Len(t, work[0].Snapshot.Events, 2)
}

func TestObserveQueuesOnlyNewResearchRanges(t *testing.T) {
	scheduler := &recordingScheduler{}
	cfg := ReplayConfig()
	cfg.AutoResearch = true
	cfg.ResearchChunkEvents = 2
	cfg.MaxResearchTopics = 1
	cfg.ResearchScopes = []ResearchScope{ScopeInternal}
	cfg.ResearchScheduler = scheduler

	bot := New(LocalAgentClient{}, cfg)
	events, err := ParseTranscript(strings.NewReader(`[00:01-00:02] Speaker A: sandbox preview runtime state
[00:02-00:03] Speaker A: sandbox preview runtime state
[00:03-00:04] Speaker A: sandbox preview runtime state
[00:04-00:05] Speaker A: sandbox preview runtime state`))
	require.NoError(t, err)
	for _, event := range events {
		require.NoError(t, bot.Observe(context.Background(), event))
	}

	work := scheduler.Work()
	require.Len(t, work, 2)
	require.Equal(t, 0, work[0].Job.TranscriptStart)
	require.Equal(t, 1, work[0].Job.TranscriptEnd)
	require.Equal(t, 2, work[1].Job.TranscriptStart)
	require.Equal(t, 3, work[1].Job.TranscriptEnd)
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

	require.Less(t, answer.OpeningReadinessLatency, 10*time.Millisecond)
	require.Contains(t, answer.Opening, "preview")
	require.Contains(t, answer.Opening, "[")
	require.Contains(t, answer.Text, answer.Opening)
	require.Equal(t, OutputStatusNormal, answer.Status)
	require.Equal(t, OutputStatusNormal, answer.Validation.Status)
	require.NotZero(t, answer.OpeningReadinessLatency)
	require.NotZero(t, answer.TimeToFinalValidatedAnswer)

	requests := client.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, RoleFastAnswer, requests[0].Role)
	require.Equal(t, agent.EffortLow, requests[0].Effort)
	require.Equal(t, "gpt-5.3-codex", requests[0].Model)
}

func TestAnswerQuestionStreamEmitsOpeningBeforeFinal(t *testing.T) {
	client := &recordingClient{}
	cfg := DefaultConfig()
	cfg.AutoResearch = false
	bot := New(client, cfg)
	require.NoError(t, bot.IngestTranscript(context.Background(), sampleEvents()))

	stream := &recordingAnswerStream{}
	answer, err := bot.AnswerQuestionStream(context.Background(), "What should we do about preview failures?", stream)
	require.NoError(t, err)
	require.Equal(t, answer.Opening, stream.opening)
	require.Equal(t, answer.Text, stream.final.Text)
	require.Equal(t, []string{"opening", "final"}, stream.Events())
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
	require.NotEqual(t, OutputStatusInvalid, summary.Status)

	requests := client.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, RoleSummary, requests[0].Role)
	require.Equal(t, "gpt-5.5", requests[0].Model)
	require.Equal(t, agent.EffortHigh, requests[0].Effort)
}

func TestEvaluateQualityGatePassesLocalEvaluation(t *testing.T) {
	path := writeSampleTranscript(t)
	cfg := ReplayConfig()
	cfg.MaxResearchTopics = 1
	cfg.ResearchScopes = []ResearchScope{ScopeInternal}

	result, err := EvaluateFile(context.Background(), path, LocalAgentClient{}, cfg, nil)
	require.NoError(t, err)
	gate := EvaluateQualityGate([]FileEvaluation{result}, DefaultQualityGateConfig(cfg, 10*time.Second))
	require.True(t, gate.Passed, gate.Checks)
}

func TestEvaluateQualityGateFailsSlowOpening(t *testing.T) {
	result := FileEvaluation{
		Path:   "slow.txt",
		Events: 1,
		Interactions: []InteractionResult{{
			TotalLatency: 2 * time.Second,
			Answer: Answer{
				OpeningReadinessLatency: 2 * time.Second,
				Opening:                 "Based on [00:01], this is grounded.",
				Text:                    "Based on [00:01], this is grounded.",
				Status:                  OutputStatusNormal,
			},
		}},
		Summary: Summary{
			Latency: 10 * time.Millisecond,
			Text:    "Executive summary\n\nDecisions\n\nAction items\n\nRisks/blockers\n\nBackground/context\n",
			Status:  OutputStatusNormal,
		},
	}
	gate := EvaluateQualityGate([]FileEvaluation{result}, QualityGateConfig{
		OpeningLatencyBudget: time.Second,
		MaxAnswerLatency:     5 * time.Second,
		MaxSummaryLatency:    time.Second,
		MinEvents:            1,
		MinInteractions:      1,
		RequireNormalStatus:  true,
	})
	require.False(t, gate.Passed)
}

func sampleEvents() []MeetingEvent {
	input := strings.NewReader(`[00:02-00:05] Speaker A: You can start.
[00:54-01:13] Speaker B: There were deployment-related issues because new services did not have proper setup in staging.
[03:43-03:56] Speaker B: Today I will continue following up the custom app build part.
[11:45-11:56] Speaker A: There is a problem with preview; two apps out of three work, but one is weird.
[12:11-12:26] Speaker C: First resolve why request deployment failed and remove the production-only flag.
[13:30-13:42] Speaker C: This is an auth preview problem; quick fix would be removing the full screen viewer.
[11:09-11:33] Speaker D: Customers want multi-department approval workflows with human review and approvals.`)
	events, err := ParseTranscript(input)
	if err != nil {
		panic(err)
	}
	return events
}

func writeSampleTranscript(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, event := range sampleEvents() {
		b.WriteString(formatEvent(event))
		b.WriteByte('\n')
	}
	path := t.TempDir() + "/sample.txt"
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))
	return path
}

type recordingClient struct { //nolint:govet // fieldalignment: test fixture readability.
	mu       sync.Mutex
	requests []AgentRequest
	delay    time.Duration
}

type recordingScheduler struct { //nolint:govet // fieldalignment: test fixture readability.
	mu   sync.Mutex
	work []ResearchWork
}

func (s *recordingScheduler) Enqueue(_ context.Context, work ResearchWork) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.work = append(s.work, work)
	return nil
}

func (s *recordingScheduler) Work() []ResearchWork {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ResearchWork(nil), s.work...)
}

type recordingAnswerStream struct { //nolint:govet // fieldalignment: test fixture readability.
	mu      sync.Mutex
	events  []string
	opening string
	final   Answer
}

func (s *recordingAnswerStream) OnOpening(_ context.Context, opening string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "opening")
	s.opening = opening
	return nil
}

func (s *recordingAnswerStream) OnFinal(_ context.Context, answer Answer, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "final")
	s.final = answer
	return nil
}

func (s *recordingAnswerStream) Events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
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
