// Package taskrouter provides AI-powered routing for task-to-worktree assignment.
// It analyzes task descriptions and existing worktrees to propose the best worktree
// for a new task - either using an existing worktree or creating a new one.
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

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex/render"
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
	Name      string // Branch/worktree name
	Path      string // Filesystem path
	Goal      string // Branch goal description (if set)
	Parent    string // Parent branch (for cascading branches)
	IsDirty   bool   // Has uncommitted changes
	IsAhead   bool   // Has unpushed commits
	IsMerged  bool   // PR has been merged
	PRState   string // PR state (open, merged, closed, "")
	LastCommit string // Last commit message
}

// RouteRequest contains the input for a routing decision.
type RouteRequest struct {
	// Prompt is the task description from the user.
	Prompt string
	// Worktrees is the list of existing worktrees in the repo.
	Worktrees []WorktreeInfo
	// CurrentWT is the currently selected worktree (provides context).
	CurrentWT string
	// RepoName is the repository name.
	RepoName string
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
	// Model is the Codex model to use (default: gpt-5.2-codex).
	Model string
	// WorkDir is the working directory for Codex.
	WorkDir string
	// Verbose enables verbose output.
	Verbose bool
	// NoColor disables colored output.
	NoColor bool
}

// Router routes tasks to worktrees using AI.
type Router struct {
	config   Config
	client   *codex.Client
	output   io.Writer
	renderer *render.Renderer
}

// New creates a new task router.
func New(config Config) *Router {
	if config.Model == "" {
		config.Model = "gpt-5.2-codex"
	}
	return &Router{
		config:   config,
		output:   os.Stdout,
		renderer: render.NewRenderer(os.Stdout, config.Verbose, config.NoColor),
	}
}

// SetOutput sets the output writer.
func (r *Router) SetOutput(w io.Writer) {
	r.output = w
	r.renderer = render.NewRenderer(w, r.config.Verbose, r.config.NoColor)
}

// Start initializes the Codex client.
func (r *Router) Start(ctx context.Context) error {
	r.client = codex.NewClient(
		codex.WithClientName("task-router"),
		codex.WithClientVersion("1.0.0"),
	)
	return r.client.Start(ctx)
}

// Stop shuts down the Codex client.
func (r *Router) Stop() error {
	if r.client != nil {
		return r.client.Stop()
	}
	return nil
}

// Route analyzes the task and worktrees to propose a routing decision.
func (r *Router) Route(ctx context.Context, req RouteRequest) (*RouteProposal, error) {
	if r.client == nil {
		return nil, fmt.Errorf("router not started")
	}

	// Build the prompt
	prompt := buildRoutingPrompt(req)

	// Create a thread
	thread, err := r.client.CreateThread(ctx,
		codex.WithModel(r.config.Model),
		codex.WithWorkDir(r.config.WorkDir),
		codex.WithApprovalPolicy(codex.ApprovalPolicyOnFailure),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create thread: %w", err)
	}

	// Wait for thread to be ready
	if err := thread.WaitReady(ctx); err != nil {
		return nil, fmt.Errorf("thread not ready: %w", err)
	}

	// Send the message
	if _, err := thread.SendMessage(ctx, prompt); err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	// Collect the response
	var responseText strings.Builder

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-r.client.Events():
			if !ok {
				return nil, fmt.Errorf("event channel closed")
			}

			switch e := event.(type) {
			case codex.TextDeltaEvent:
				if e.ThreadID == thread.ID() {
					responseText.WriteString(e.Delta)
					if r.config.Verbose {
						r.renderer.Text(e.Delta)
					}
				}
			case codex.TurnCompletedEvent:
				if e.ThreadID == thread.ID() {
					// Parse the response
					return parseRouteResponse(responseText.String())
				}
			case codex.ErrorEvent:
				return nil, fmt.Errorf("codex error: %v", e.Error)
			}
		}
	}
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
