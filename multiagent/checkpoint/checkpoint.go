// Package checkpoint provides session state persistence for error recovery.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// Phase represents the current phase in the mission workflow.
type Phase string

const (
	PhaseNotStarted Phase = "not_started"
	PhaseDesigning  Phase = "designing"
	PhaseBuilding   Phase = "building"
	PhaseReviewing  Phase = "reviewing"
	PhaseCompleted  Phase = "completed"
	PhaseFailed     Phase = "failed"
)

// Checkpoint captures the state of a mission for recovery.
type Checkpoint struct {
	LastUpdated    time.Time                `json:"last_updated"`
	ReviewResponse *protocol.ReviewResponse `json:"review_response,omitempty"`
	BuildResponse  *protocol.BuildResponse  `json:"build_response,omitempty"`
	DesignResponse *protocol.DesignResponse `json:"design_response,omitempty"`
	Phase          Phase                    `json:"phase"`
	Version        string                   `json:"version"`
	Mission        string                   `json:"mission"`
	SessionID      string                   `json:"session_id"`
	LastError      string                   `json:"last_error,omitempty"`
	FilesCreated   []string                 `json:"files_created"`
	FilesModified  []string                 `json:"files_modified"`
	IterationCount int                      `json:"iteration_count"`
	TotalCost      float64                  `json:"total_cost"`
}

// CheckpointFileName is the standard checkpoint file name.
const CheckpointFileName = "checkpoint.json"

// Manager handles checkpoint persistence.
type Manager struct {
	current    *Checkpoint
	sessionDir string
	filePath   string
}

// NewManager creates a new checkpoint manager.
func NewManager(sessionDir, sessionID string) *Manager {
	return &Manager{
		sessionDir: sessionDir,
		filePath:   filepath.Join(sessionDir, sessionID, CheckpointFileName),
		current: &Checkpoint{
			Version:     "1.0",
			SessionID:   sessionID,
			Phase:       PhaseNotStarted,
			LastUpdated: time.Now(),
		},
	}
}

// SetMission sets the mission for the checkpoint.
func (m *Manager) SetMission(mission string) {
	m.current.Mission = mission
}

// StartDesign marks the beginning of the design phase.
func (m *Manager) StartDesign() error {
	m.current.Phase = PhaseDesigning
	m.current.LastUpdated = time.Now()
	return m.save()
}

// CompleteDesign records a successful design phase.
func (m *Manager) CompleteDesign(response *protocol.DesignResponse, cost float64) error {
	m.current.DesignResponse = response
	m.current.TotalCost += cost
	m.current.LastUpdated = time.Now()
	return m.save()
}

// StartBuild marks the beginning of the build phase.
func (m *Manager) StartBuild() error {
	m.current.Phase = PhaseBuilding
	m.current.LastUpdated = time.Now()
	return m.save()
}

// CompleteBuild records a successful build phase.
func (m *Manager) CompleteBuild(response *protocol.BuildResponse, cost float64) error {
	m.current.BuildResponse = response
	m.current.TotalCost += cost
	// Accumulate file lists
	m.current.FilesCreated = append(m.current.FilesCreated, response.FilesCreated...)
	m.current.FilesModified = append(m.current.FilesModified, response.FilesModified...)
	m.current.LastUpdated = time.Now()
	return m.save()
}

// StartReview marks the beginning of the review phase.
func (m *Manager) StartReview() error {
	m.current.Phase = PhaseReviewing
	m.current.LastUpdated = time.Now()
	return m.save()
}

// CompleteReview records a successful review phase.
func (m *Manager) CompleteReview(response *protocol.ReviewResponse, cost float64) error {
	m.current.ReviewResponse = response
	m.current.TotalCost += cost
	m.current.LastUpdated = time.Now()
	return m.save()
}

// StartIteration marks the beginning of a new iteration (after critical issues).
func (m *Manager) StartIteration() error {
	m.current.IterationCount++
	m.current.Phase = PhaseBuilding
	// Clear build/review responses for new iteration
	m.current.BuildResponse = nil
	m.current.ReviewResponse = nil
	m.current.LastUpdated = time.Now()
	return m.save()
}

// Complete marks the mission as successfully completed.
func (m *Manager) Complete() error {
	m.current.Phase = PhaseCompleted
	m.current.LastUpdated = time.Now()
	return m.save()
}

// Fail marks the mission as failed with an error message.
func (m *Manager) Fail(err error) error {
	m.current.Phase = PhaseFailed
	if err != nil {
		m.current.LastError = err.Error()
	}
	m.current.LastUpdated = time.Now()
	return m.save()
}

// Current returns the current checkpoint state.
func (m *Manager) Current() *Checkpoint {
	return m.current
}

// save writes the checkpoint to disk.
func (m *Manager) save() error {
	// Ensure directory exists
	dir := filepath.Dir(m.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create checkpoint directory: %w", err)
	}

	data, err := json.MarshalIndent(m.current, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint: %w", err)
	}

	if err := os.WriteFile(m.filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write checkpoint: %w", err)
	}

	return nil
}

// Load reads a checkpoint from disk.
func Load(sessionDir, sessionID string) (*Checkpoint, error) {
	filePath := filepath.Join(sessionDir, sessionID, CheckpointFileName)

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No checkpoint exists
		}
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint: %w", err)
	}

	return &checkpoint, nil
}

// Exists checks if a checkpoint exists for the given session.
func Exists(sessionDir, sessionID string) bool {
	filePath := filepath.Join(sessionDir, sessionID, CheckpointFileName)
	_, err := os.Stat(filePath)
	return err == nil
}

// Delete removes the checkpoint file.
func Delete(sessionDir, sessionID string) error {
	filePath := filepath.Join(sessionDir, sessionID, CheckpointFileName)
	err := os.Remove(filePath)
	if os.IsNotExist(err) {
		return nil // Already deleted
	}
	return err
}

// CanResume returns true if the checkpoint can be resumed.
func (c *Checkpoint) CanResume() bool {
	switch c.Phase {
	case PhaseDesigning, PhaseBuilding, PhaseReviewing:
		return true
	case PhaseFailed:
		// Can resume from failed state if we have design
		return c.DesignResponse != nil
	default:
		return false
	}
}

// ResumePhase returns the phase to resume from.
func (c *Checkpoint) ResumePhase() Phase {
	switch c.Phase {
	case PhaseDesigning:
		return PhaseDesigning
	case PhaseBuilding:
		// If we have a build response, move to review
		if c.BuildResponse != nil {
			return PhaseReviewing
		}
		return PhaseBuilding
	case PhaseReviewing:
		// If review found critical issues, resume building
		if c.ReviewResponse != nil && c.ReviewResponse.HasCriticalIssues() {
			return PhaseBuilding
		}
		return PhaseReviewing
	case PhaseFailed:
		// Resume from the last successful phase
		if c.ReviewResponse != nil {
			return PhaseBuilding // Retry build after failed review iteration
		}
		if c.BuildResponse != nil {
			return PhaseReviewing
		}
		if c.DesignResponse != nil {
			return PhaseBuilding
		}
		return PhaseDesigning
	default:
		return PhaseNotStarted
	}
}
