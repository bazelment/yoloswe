package meetingbot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Interaction is one simulated live question.
type Interaction struct {
	Question string
}

// InteractionResult records answer quality inputs and latency.
type InteractionResult struct {
	Answer       Answer
	TotalLatency time.Duration
}

// FileEvaluation is the evaluation result for one meeting transcript.
type FileEvaluation struct { //nolint:govet // fieldalignment: report fields are grouped by meaning.
	Summary      Summary
	Path         string
	Events       int
	Topics       []Topic
	Interactions []InteractionResult
}

// QualityGateConfig controls automated evaluation pass/fail checks.
type QualityGateConfig struct {
	OpeningLatencyBudget time.Duration
	MaxAnswerLatency     time.Duration
	MaxSummaryLatency    time.Duration
	MinEvents            int
	MinInteractions      int
	RequireNoErrors      bool
	RequireNormalStatus  bool
}

// QualityGateCheck is one automated gate assertion.
type QualityGateCheck struct {
	Name   string
	Status string
	Detail string
}

// QualityGateResult aggregates automated eval checks.
type QualityGateResult struct {
	Checks []QualityGateCheck
	Passed bool
}

// DefaultInteractions cover operational, product, and follow-up questions.
func DefaultInteractions() []Interaction {
	return []Interaction{
		{Question: "What is the most likely root cause pattern behind the sandbox or preview failures?"},
		{Question: "What should we tell the team about staging versus production for demos and testing?"},
		{Question: "What changed for customer workflow priorities, and what should we do next?"},
		{Question: "What are the highest priority follow-up actions and risks?"},
	}
}

// DefaultQualityGateConfig returns the deterministic local/real smoke gate.
// minInteractions must be the number of interactions the evaluation actually
// runs (the count passed to EvaluateFile). Hard-coding the default set's size
// here would spuriously FAIL the interaction-count check whenever a caller runs
// a smaller custom --question list.
func DefaultQualityGateConfig(cfg Config, openingBudget time.Duration, minInteractions int) QualityGateConfig {
	if openingBudget == 0 {
		openingBudget = 10 * time.Second
	}
	if minInteractions < 1 {
		minInteractions = 1
	}
	return QualityGateConfig{
		OpeningLatencyBudget: openingBudget,
		MaxAnswerLatency:     cfg.FastAnswerTimeout + openingBudget,
		MaxSummaryLatency:    cfg.SummaryTimeout,
		MinEvents:            1,
		MinInteractions:      minInteractions,
		RequireNoErrors:      true,
		RequireNormalStatus:  true,
	}
}

// EvaluateQualityGate returns an automated pass/fail result for evaluations.
func EvaluateQualityGate(results []FileEvaluation, cfg QualityGateConfig) QualityGateResult {
	gate := QualityGateResult{Passed: true}
	add := func(name string, passed bool, detail string) {
		status := "pass"
		if !passed {
			status = "fail"
			gate.Passed = false
		}
		gate.Checks = append(gate.Checks, QualityGateCheck{Name: name, Status: status, Detail: detail})
	}

	add("evaluation files present", len(results) > 0, fmt.Sprintf("files=%d", len(results)))
	for resultIndex := range results {
		result := results[resultIndex]
		prefix := result.Path
		add(prefix+" events", result.Events >= cfg.MinEvents, fmt.Sprintf("events=%d min=%d", result.Events, cfg.MinEvents))
		add(prefix+" interactions", len(result.Interactions) >= cfg.MinInteractions, fmt.Sprintf("interactions=%d min=%d", len(result.Interactions), cfg.MinInteractions))
		for i := range result.Interactions {
			interaction := result.Interactions[i]
			name := fmt.Sprintf("%s interaction %d", prefix, i+1)
			answer := interaction.Answer
			add(name+" opening latency", cfg.OpeningLatencyBudget == 0 || answer.OpeningReadinessLatency <= cfg.OpeningLatencyBudget, fmt.Sprintf("opening=%s budget=%s", answer.OpeningReadinessLatency.Round(time.Millisecond), cfg.OpeningLatencyBudget))
			add(name+" answer latency", cfg.MaxAnswerLatency == 0 || interaction.TotalLatency <= cfg.MaxAnswerLatency, fmt.Sprintf("total=%s budget=%s", interaction.TotalLatency.Round(time.Millisecond), cfg.MaxAnswerLatency))
			add(name+" text", strings.TrimSpace(answer.Opening) != "" && strings.TrimSpace(answer.Text) != "", "opening and final answer are non-empty")
			if cfg.RequireNoErrors {
				add(name+" model error", answer.Error == "", firstNonEmpty(answer.Error, "none"))
			}
			if cfg.RequireNormalStatus {
				add(name+" status", answer.Status == OutputStatusNormal, fmt.Sprintf("status=%s reason=%s", answer.Status, answer.Validation.Reason))
			} else {
				add(name+" status", answer.Status != OutputStatusInvalid, fmt.Sprintf("status=%s", answer.Status))
			}
		}
		add(prefix+" summary latency", cfg.MaxSummaryLatency == 0 || result.Summary.Latency <= cfg.MaxSummaryLatency, fmt.Sprintf("latency=%s budget=%s", result.Summary.Latency.Round(time.Millisecond), cfg.MaxSummaryLatency))
		add(prefix+" summary sections", hasRequiredSummarySections(result.Summary.Text), "required sections present")
		if cfg.RequireNoErrors {
			add(prefix+" summary model error", result.Summary.Error == "", firstNonEmpty(result.Summary.Error, "none"))
		}
		if cfg.RequireNormalStatus {
			add(prefix+" summary status", result.Summary.Status == OutputStatusNormal, fmt.Sprintf("status=%s reason=%s", result.Summary.Status, result.Summary.Validation.Reason))
		} else {
			add(prefix+" summary status", result.Summary.Status != OutputStatusInvalid, fmt.Sprintf("status=%s", result.Summary.Status))
		}
	}
	return gate
}

// EvaluateFile simulates ingesting one note, doing background research, asking
// several live questions, and generating a post-meeting summary.
func EvaluateFile(ctx context.Context, path string, client AgentClient, cfg Config, interactions []Interaction) (FileEvaluation, error) {
	events, err := LoadTranscriptFile(path)
	if err != nil {
		return FileEvaluation{}, err
	}
	cfg.AutoResearch = false
	bot := New(client, cfg)
	if err := bot.IngestTranscript(ctx, events); err != nil {
		return FileEvaluation{}, err
	}
	if err := bot.BuildBackground(ctx); err != nil {
		return FileEvaluation{}, err
	}
	if len(interactions) == 0 {
		interactions = DefaultInteractions()
	}

	results, err := evaluateInteractions(ctx, bot, interactions, cfg.AnswerConcurrency)
	if err != nil {
		return FileEvaluation{}, err
	}
	summary, err := bot.SummarizeMeeting(ctx)
	if err != nil {
		return FileEvaluation{}, err
	}

	return FileEvaluation{
		Path:         path,
		Events:       len(events),
		Topics:       bot.Topics(),
		Interactions: results,
		Summary:      summary,
	}, nil
}

func evaluateInteractions(ctx context.Context, bot *Bot, interactions []Interaction, configuredConcurrency int) ([]InteractionResult, error) {
	if len(interactions) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]InteractionResult, len(interactions))
	concurrency := boundedConcurrency(configuredConcurrency, len(interactions))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	stopped := false
	for i, interaction := range interactions {
		if stopped {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			stopped = true
		}
		if stopped {
			break
		}
		wg.Add(1)
		go func(i int, interaction Interaction) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			answer, err := bot.AnswerQuestion(ctx, interaction.Question)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
				return
			}
			results[i] = InteractionResult{
				Answer:       answer,
				TotalLatency: time.Since(start),
			}
		}(i, interaction)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}
