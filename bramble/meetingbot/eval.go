package meetingbot

import (
	"context"
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

// DefaultInteractions cover operational, product, and follow-up questions.
func DefaultInteractions() []Interaction {
	return []Interaction{
		{Question: "What is the most likely root cause pattern behind the sandbox or preview failures?"},
		{Question: "What should we tell the team about staging versus production for demos and testing?"},
		{Question: "What changed for customer workflow priorities, and what should we do next?"},
		{Question: "What are the highest priority follow-up actions and risks?"},
	}
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

	results := make([]InteractionResult, 0, len(interactions))
	for _, interaction := range interactions {
		start := time.Now()
		answer, err := bot.AnswerQuestion(ctx, interaction.Question)
		if err != nil {
			return FileEvaluation{}, err
		}
		results = append(results, InteractionResult{
			Answer:       answer,
			TotalLatency: time.Since(start),
		})
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
