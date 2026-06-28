package meetingbot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

// ProviderAgentClient dispatches meeting-bot roles to the repo's existing
// Codex/Claude/Gemini provider abstraction.
type ProviderAgentClient struct{}

func (ProviderAgentClient) Run(ctx context.Context, req AgentRequest) (AgentResponse, error) {
	start := time.Now()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	modelID := strings.TrimSpace(req.Model)
	if modelID == "" {
		return AgentResponse{}, fmt.Errorf("missing model for %s", req.Role)
	}
	m, ok := agent.ResolveModel(modelID)
	if !ok {
		return AgentResponse{}, fmt.Errorf("unknown model %q; expected one of registered models or prefixes %s", modelID, agent.KnownModelPrefixes())
	}

	provider, err := agent.NewProviderForModel(m)
	if err != nil {
		return AgentResponse{}, err
	}
	defer provider.Close()

	opts := []agent.ExecuteOption{
		agent.WithProviderModel(modelID),
		agent.WithProviderSystemPrompt(req.SystemPrompt),
	}
	if req.WorkDir != "" {
		opts = append(opts, agent.WithProviderWorkDir(req.WorkDir))
	}
	if req.PermissionMode != "" {
		opts = append(opts, agent.WithProviderPermissionMode(req.PermissionMode))
	}
	if req.Effort != "" && req.Effort != agent.EffortAuto {
		opts = append(opts, agent.WithProviderEffort(req.Effort))
	}
	// Meeting-bot requests are bounded single-turn jobs. This prevents a
	// research layer from drifting into an open-ended agent loop.
	opts = append(opts, agent.WithProviderMaxTurns(4))

	result, err := provider.Execute(ctx, req.Prompt, nil, opts...)
	if err != nil {
		return AgentResponse{Latency: time.Since(start), Model: modelID, Provider: m.Provider}, err
	}
	if result == nil {
		return AgentResponse{Latency: time.Since(start), Model: modelID, Provider: m.Provider}, fmt.Errorf("provider returned no result for %s", req.Role)
	}
	if !result.Success {
		// A failed turn must surface as an error even when the provider left
		// result.Error nil (e.g. an unresolved tool loop that stopped without a
		// terminal error). Treating it as success would let downstream answer
		// and summary paths skip fallbacks and emit a partial turn as final.
		failErr := result.Error
		if failErr == nil {
			if result.UnresolvedToolError != nil {
				ute := result.UnresolvedToolError
				failErr = fmt.Errorf("provider turn unsuccessful for %s: unresolved tool %q (%s)", req.Role, ute.Tool, ute.Reason)
			} else {
				failErr = fmt.Errorf("provider turn unsuccessful for %s", req.Role)
			}
		}
		return AgentResponse{
			Latency:  time.Since(start),
			Text:     result.Text,
			Model:    modelID,
			Provider: m.Provider,
			Usage:    result.Usage,
		}, failErr
	}
	return AgentResponse{
		Latency:  time.Since(start),
		Text:     strings.TrimSpace(result.Text),
		Model:    modelID,
		Provider: m.Provider,
		Usage:    result.Usage,
	}, nil
}

func roleSystemPrompt(role AgentRole) string {
	switch role {
	case RoleFastAnswer:
		return `You are a meeting copilot answering during a live discussion.
Answer directly from the transcript snippets and cached research provided by the caller.
Use concise, evidence-grounded prose. If the evidence is incomplete, say what is uncertain and what to verify next.
Do not invent facts, owners, or decisions. Keep the answer under 250 words unless the question asks for detail.`
	case RoleInternalResearch:
		return `You are an internal context researcher for a live meeting.
Extract background understanding, unstated dependencies, likely project context, risks, and decisions from the transcript evidence.
Return crisp bullets with source timestamps or speaker names where available.`
	case RoleCodebaseResearch:
		return `You are a read-only codebase researcher for a live meeting.
Inspect the repository when helpful. Connect the discussion to concrete code paths, docs, commands, or architecture already present.
Do not edit files. Cite file paths for codebase findings and explicitly say when the repo does not contain enough evidence.`
	case RoleWebResearch:
		return `You are a public-web researcher for a live meeting.
Use public internet research tools if they are available in this environment. Return only durable, source-backed findings with URLs or source names.
If web access is unavailable, say so plainly and provide no guessed public facts.`
	case RoleSummary:
		return `You are a senior staff-level meeting summarizer.
Produce a high-signal post-meeting summary after cross-referencing transcript evidence with cached internal, codebase, and public-web research.
Separate decisions, action items, risks/blockers, and useful background. Attribute owners when the transcript supports it. Do not pad.`
	default:
		return "You are a precise meeting assistant. Use the supplied evidence only."
	}
}
