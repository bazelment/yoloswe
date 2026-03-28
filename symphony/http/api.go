package symphttp

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// stateResponse matches the spec Section 13.7.2 GET /api/v1/state shape.
type stateResponse struct {
	Counts      map[string]int  `json:"counts"`
	GeneratedAt string          `json:"generated_at"`
	Running     []runningJSON   `json:"running"`
	Retrying    []retryJSON     `json:"retrying"`
	RateLimits  json.RawMessage `json:"rate_limits"`
	CodexTotals codexTotalsJSON `json:"codex_totals"`
}

type runningJSON struct {
	LastEventAt     *string         `json:"last_event_at"`
	IssueID         string          `json:"issue_id"`
	IssueIdentifier string          `json:"issue_identifier"`
	State           string          `json:"state"`
	SessionID       string          `json:"session_id"`
	LastEvent       string          `json:"last_event"`
	LastMessage     string          `json:"last_message"`
	StartedAt       string          `json:"started_at"`
	Tokens          codexTotalsJSON `json:"tokens"`
	TurnCount       int             `json:"turn_count"`
}

type retryJSON struct {
	IssueID         string `json:"issue_id"`
	IssueIdentifier string `json:"issue_identifier"`
	DueAt           string `json:"due_at"`
	Error           string `json:"error"`
	Attempt         int    `json:"attempt"`
}

type codexTotalsJSON struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}

	snap, err := s.orch.RequestSnapshot(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}

	running := make([]runningJSON, 0, len(snap.Running))
	for i := range snap.Running {
		rs := &snap.Running[i]
		rj := runningJSON{
			IssueID:         rs.IssueID,
			IssueIdentifier: rs.IssueIdentifier,
			State:           rs.State,
			SessionID:       rs.SessionID,
			TurnCount:       rs.TurnCount,
			LastEvent:       rs.LastEvent,
			LastMessage:     rs.LastMessage,
			StartedAt:       rs.StartedAt.UTC().Format(time.RFC3339),
			Tokens: codexTotalsJSON{
				InputTokens:  rs.Tokens.InputTokens,
				OutputTokens: rs.Tokens.OutputTokens,
				TotalTokens:  rs.Tokens.TotalTokens,
			},
		}
		if rs.LastEventAt != nil {
			t := rs.LastEventAt.UTC().Format(time.RFC3339)
			rj.LastEventAt = &t
		}
		running = append(running, rj)
	}

	retrying := make([]retryJSON, 0, len(snap.Retrying))
	for _, rt := range snap.Retrying {
		retrying = append(retrying, retryJSON{
			IssueID:         rt.IssueID,
			IssueIdentifier: rt.IssueIdentifier,
			Attempt:         rt.Attempt,
			DueAt:           rt.DueAt.UTC().Format(time.RFC3339),
			Error:           rt.Error,
		})
	}

	resp := stateResponse{
		GeneratedAt: snap.GeneratedAt.UTC().Format(time.RFC3339),
		Counts: map[string]int{
			"running":  len(snap.Running),
			"retrying": len(snap.Retrying),
		},
		Running:  running,
		Retrying: retrying,
		CodexTotals: codexTotalsJSON{
			InputTokens:    snap.Totals.InputTokens,
			OutputTokens:   snap.Totals.OutputTokens,
			TotalTokens:    snap.Totals.TotalTokens,
			SecondsRunning: snap.Totals.SecondsRunning,
		},
		RateLimits: snap.RateLimits,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}

	// Extract identifier from /api/v1/{identifier}
	identifier := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if identifier == "" {
		writeError(w, http.StatusBadRequest, "missing_identifier", "issue identifier is required")
		return
	}

	snap, err := s.orch.RequestSnapshot(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}

	// Search running sessions.
	for i := range snap.Running {
		rs := &snap.Running[i]
		if rs.IssueIdentifier == identifier {
			rj := runningJSON{
				IssueID:         rs.IssueID,
				IssueIdentifier: rs.IssueIdentifier,
				State:           rs.State,
				SessionID:       rs.SessionID,
				TurnCount:       rs.TurnCount,
				LastEvent:       rs.LastEvent,
				LastMessage:     rs.LastMessage,
				StartedAt:       rs.StartedAt.UTC().Format(time.RFC3339),
				Tokens: codexTotalsJSON{
					InputTokens:  rs.Tokens.InputTokens,
					OutputTokens: rs.Tokens.OutputTokens,
					TotalTokens:  rs.Tokens.TotalTokens,
				},
			}
			if rs.LastEventAt != nil {
				t := rs.LastEventAt.UTC().Format(time.RFC3339)
				rj.LastEventAt = &t
			}
			resp := map[string]any{
				"issue_identifier": rs.IssueIdentifier,
				"issue_id":         rs.IssueID,
				"status":           "running",
				"running":          rj,
				"retry":            nil,
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}

	// Search retry queue.
	for _, rt := range snap.Retrying {
		if rt.IssueIdentifier == identifier {
			resp := map[string]any{
				"issue_identifier": rt.IssueIdentifier,
				"issue_id":         rt.IssueID,
				"status":           "retrying",
				"running":          nil,
				"retry": retryJSON{
					IssueID:         rt.IssueID,
					IssueIdentifier: rt.IssueIdentifier,
					Attempt:         rt.Attempt,
					DueAt:           rt.DueAt.UTC().Format(time.RFC3339),
					Error:           rt.Error,
				},
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}

	writeError(w, http.StatusNotFound, "issue_not_found", "issue identifier not found in current state: "+identifier)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}

	err := s.orch.RequestRefresh(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "refresh_failed", err.Error())
		return
	}

	resp := map[string]any{
		"queued":       true,
		"coalesced":    false,
		"requested_at": time.Now().UTC().Format(time.RFC3339),
		"operations":   []string{"poll", "reconcile"},
	}
	writeJSON(w, http.StatusAccepted, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{
		Error: errorDetail{
			Code:    code,
			Message: message,
		},
	})
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}
