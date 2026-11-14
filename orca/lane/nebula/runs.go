package nebula

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// runLabel is the run field `nebula run create --orchestrator-label` writes.
type runLabel struct {
	Label string `json:"externalOrchestratorLabel"`
}

// RunOrchestrator returns runID's external orchestrator label ("" when unset).
func (d *Driver) RunOrchestrator(ctx context.Context, runID string) (string, error) {
	var v runLabel
	if err := d.doJSON(ctx, http.MethodGet, "/api/code-runs/"+url.PathEscape(runID), nil, &v); err != nil {
		return "", fmt.Errorf("nebula run %q: %w", runID, err)
	}
	return v.Label, nil
}

// SetRunOrchestrator sets runID's external orchestrator label.
func (d *Driver) SetRunOrchestrator(ctx context.Context, runID, label string) error {
	if err := d.doJSON(ctx, http.MethodPut, "/api/code-runs/"+url.PathEscape(runID), runLabel{Label: label}, nil); err != nil {
		return fmt.Errorf("nebula label run %q: %w", runID, err)
	}
	return nil
}
