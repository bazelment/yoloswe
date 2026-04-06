package tui

import (
	"github.com/bazelment/yoloswe/jiradozer"
)

// issueStatusMsg delivers a status update from the orchestrator.
type issueStatusMsg struct {
	Status jiradozer.IssueStatus
}

// statusTickMsg triggers a periodic refresh of the orchestrator snapshot.
type statusTickMsg struct{}
