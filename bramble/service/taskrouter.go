package service

import (
	"context"
	"strings"

	"github.com/bazelment/yoloswe/wt/taskrouter"
)

// TaskRouterService abstracts task-to-worktree routing.
type TaskRouterService interface {
	Route(ctx context.Context, req taskrouter.RouteRequest) (*taskrouter.RouteProposal, error)
}

// LocalTaskRouterService implements TaskRouterService using the local taskrouter.Router.
type LocalTaskRouterService struct {
	router *taskrouter.Router
}

// NewLocalTaskRouterService creates a new local task router service.
// router may be nil; Route will fall back to heuristic routing in that case.
func NewLocalTaskRouterService(router *taskrouter.Router) *LocalTaskRouterService {
	return &LocalTaskRouterService{router: router}
}

// Route analyzes the task and worktrees to propose a routing decision.
func (s *LocalTaskRouterService) Route(ctx context.Context, req taskrouter.RouteRequest) (*taskrouter.RouteProposal, error) {
	if s.router == nil {
		return heuristicRoute(req.Prompt, len(req.Worktrees) > 0), nil
	}
	return s.router.Route(ctx, req)
}

// heuristicRoute returns a default route proposal when no AI router is available.
func heuristicRoute(prompt string, hasWorktrees bool) *taskrouter.RouteProposal {
	reasoning := "First feature branch for this repo"
	if hasWorktrees {
		reasoning = "Creating new branch for this task"
	}
	return &taskrouter.RouteProposal{
		Action:    taskrouter.ActionCreateNew,
		Worktree:  suggestBranchName(prompt),
		Parent:    "main",
		Reasoning: reasoning,
	}
}

// suggestBranchName generates a simple branch name from a prompt.
func suggestBranchName(prompt string) string {
	words := strings.Fields(strings.ToLower(prompt))
	if len(words) > 4 {
		words = words[:4]
	}

	commonWords := map[string]bool{"a": true, "an": true, "the": true, "to": true, "for": true, "and": true, "or": true, "in": true, "on": true, "with": true}
	filtered := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.Trim(w, ".,!?;:")
		if !commonWords[w] && len(w) > 1 {
			filtered = append(filtered, w)
		}
	}

	if len(filtered) == 0 {
		return "feature-new"
	}

	return "feature-" + strings.Join(filtered, "-")
}
