// Package taskrouter provides AI-powered routing for task-to-worktree assignment.
// It analyzes task descriptions and existing worktrees to propose the best worktree
// for a new task - either using an existing worktree or creating a new one.
//
// This package uses the multiagent/agent.Provider interface so any backend
// (Claude, Codex, Gemini) can be plugged in for routing decisions.
package taskrouter

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

//go:embed prompt.go.tmpl
var routingPromptTemplate string

var routingTmpl = template.Must(template.New("routing").Parse(routingPromptTemplate))

// ProposalAction indicates whether to use an existing worktree or create a new one.
type ProposalAction string

const (
	// ActionUseExisting indicates the task should use an existing worktree.
	ActionUseExisting ProposalAction = "use_existing"
	// ActionCreateNew indicates a new worktree should be created.
	ActionCreateNew ProposalAction = "create_new"
)

// WorktreeInfo provides context about an existing worktree for routing decisions.
type WorktreeInfo struct {
	Name       string
	Path       string
	Goal       string
	Parent     string
	PRState    string
	LastCommit string
	IsDirty    bool
	IsAhead    bool
	IsMerged   bool
}

// RouteRequest contains the input for a routing decision.
type RouteRequest struct {
	Prompt    string
	CurrentWT string
	RepoName  string
	Worktrees []WorktreeInfo
}

// RouteProposal is the AI's recommendation for where to run a task.
type RouteProposal struct {
	// Action is either "use_existing" or "create_new".
	Action ProposalAction `json:"action"`
	// Worktree is the existing worktree name (for use_existing) or proposed branch name (for create_new).
	Worktree string `json:"worktree"`
	// Parent is the base branch for create_new (empty for use_existing).
	Parent string `json:"parent"`
	// Reasoning explains why this worktree was chosen.
	Reasoning string `json:"reasoning"`
}

// Config holds configuration for the router.
type Config struct {
	// Provider is the agent backend used for routing decisions.
	// If nil, Route() returns an error.
	Provider agent.Provider
	// WorkDir is the working directory for the provider.
	WorkDir string
}

// Router routes tasks to worktrees using AI.
type Router struct {
	output   io.Writer
	provider agent.Provider
	config   Config
}

// New creates a new task router.
func New(config Config) *Router {
	return &Router{
		config:   config,
		provider: config.Provider,
		output:   os.Stdout,
	}
}

// SetOutput sets the output writer.
func (r *Router) SetOutput(w io.Writer) {
	r.output = w
}

// Start initializes the router. For providers that implement LongRunningProvider,
// this calls Start. Otherwise it's a no-op.
func (r *Router) Start(ctx context.Context) error {
	if r.provider == nil {
		return fmt.Errorf("no provider configured for task router")
	}
	if lrp, ok := r.provider.(agent.LongRunningProvider); ok {
		return lrp.Start(ctx)
	}
	return nil
}

// Stop shuts down the router.
func (r *Router) Stop() error {
	if r.provider == nil {
		return nil
	}
	if lrp, ok := r.provider.(agent.LongRunningProvider); ok {
		return lrp.Stop()
	}
	return r.provider.Close()
}

// Route analyzes the task and worktrees to propose a routing decision.
func (r *Router) Route(ctx context.Context, req RouteRequest) (*RouteProposal, error) {
	if r.provider == nil {
		return nil, fmt.Errorf("router has no provider")
	}

	prompt := buildRoutingPrompt(req)

	result, err := r.provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(r.config.WorkDir),
	)
	if err != nil {
		return nil, fmt.Errorf("provider execution failed: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("provider error: %w", result.Error)
	}

	return parseRouteResponse(result.Text)
}

// buildRoutingPrompt creates the prompt for the AI router.
func buildRoutingPrompt(req RouteRequest) string {
	var buf bytes.Buffer
	if err := routingTmpl.Execute(&buf, req); err != nil {
		// Fallback to a simple prompt if template fails
		return fmt.Sprintf("Route task: %s", req.Prompt)
	}
	return buf.String()
}

// parseRouteResponse parses the AI response into a RouteProposal.
func parseRouteResponse(response string) (*RouteProposal, error) {
	// Try to find JSON in the response
	response = strings.TrimSpace(response)

	// Handle markdown code blocks
	if strings.HasPrefix(response, "```") {
		lines := strings.Split(response, "\n")
		var jsonLines []string
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(line, "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock {
				jsonLines = append(jsonLines, line)
			}
		}
		response = strings.Join(jsonLines, "\n")
	}

	// Find JSON object boundaries
	start := strings.Index(response, "{")
	end := strings.LastIndex(response, "}")
	if start == -1 || end == -1 || end < start {
		return nil, fmt.Errorf("no JSON object found in response: %s", response)
	}
	jsonStr := response[start : end+1]

	var proposal RouteProposal
	if err := json.Unmarshal([]byte(jsonStr), &proposal); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w (response: %s)", err, jsonStr)
	}

	// Validate
	if proposal.Action != ActionUseExisting && proposal.Action != ActionCreateNew {
		return nil, fmt.Errorf("invalid action: %s", proposal.Action)
	}
	if proposal.Worktree == "" {
		return nil, fmt.Errorf("worktree name is empty")
	}
	if proposal.Action == ActionCreateNew && proposal.Parent == "" {
		proposal.Parent = "main" // Default parent
	}

	return &proposal, nil
}

// MockRouter is a router that returns predefined responses for testing.
type MockRouter struct {
	Responses map[string]*RouteProposal
	Errors    map[string]error
}

// NewMockRouter creates a new mock router.
func NewMockRouter() *MockRouter {
	return &MockRouter{
		Responses: make(map[string]*RouteProposal),
		Errors:    make(map[string]error),
	}
}

// SetResponse sets the response for a given prompt prefix.
func (m *MockRouter) SetResponse(promptPrefix string, proposal *RouteProposal) {
	m.Responses[promptPrefix] = proposal
}

// SetError sets an error response for a given prompt prefix.
func (m *MockRouter) SetError(promptPrefix string, err error) {
	m.Errors[promptPrefix] = err
}

// Route returns the mocked response.
func (m *MockRouter) Route(ctx context.Context, req RouteRequest) (*RouteProposal, error) {
	// Check for exact match first
	if proposal, ok := m.Responses[req.Prompt]; ok {
		return proposal, nil
	}
	if err, ok := m.Errors[req.Prompt]; ok {
		return nil, err
	}

	// Check for prefix match
	for prefix, proposal := range m.Responses {
		if strings.HasPrefix(req.Prompt, prefix) {
			return proposal, nil
		}
	}
	for prefix, err := range m.Errors {
		if strings.HasPrefix(req.Prompt, prefix) {
			return nil, err
		}
	}

	// Default response
	return &RouteProposal{
		Action:    ActionCreateNew,
		Worktree:  "new-task",
		Parent:    "main",
		Reasoning: "Default mock response",
	}, nil
}
