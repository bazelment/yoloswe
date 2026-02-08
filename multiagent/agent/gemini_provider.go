package agent

import (
	"context"
	"fmt"

	"github.com/bazelment/yoloswe/wt"
)

// GeminiProvider is a skeleton implementation of the Provider interface for Google Gemini.
// To complete, import a Gemini Go SDK (e.g., google.golang.org/genai) and implement Execute.
type GeminiProvider struct {
	events chan AgentEvent
}

// NewGeminiProvider creates a new Gemini provider stub.
func NewGeminiProvider() *GeminiProvider {
	return &GeminiProvider{
		events: make(chan AgentEvent, 100),
	}
}

func (p *GeminiProvider) Name() string { return "gemini" }

func (p *GeminiProvider) Execute(_ context.Context, _ string, _ *wt.WorktreeContext, _ ...ExecuteOption) (*AgentResult, error) {
	return nil, fmt.Errorf("gemini provider not yet implemented")
}

func (p *GeminiProvider) Events() <-chan AgentEvent { return p.events }

func (p *GeminiProvider) Close() error {
	close(p.events)
	return nil
}
