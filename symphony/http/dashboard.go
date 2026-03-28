package symphttp

import (
	"html/template"
	"net/http"
	"time"
)

const dashboardTemplate = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta http-equiv="refresh" content="5">
  <title>Symphony Dashboard</title>
  <style>
    body { font-family: monospace; margin: 2em; background: #1a1a2e; color: #e0e0e0; }
    h1 { color: #0f3460; }
    h2 { color: #16213e; margin-top: 1.5em; }
    table { border-collapse: collapse; width: 100%; margin-top: 0.5em; }
    th, td { border: 1px solid #333; padding: 6px 10px; text-align: left; }
    th { background: #16213e; }
    .totals { display: flex; gap: 2em; margin-top: 0.5em; }
    .totals div { background: #16213e; padding: 8px 16px; border-radius: 4px; }
    .totals .label { font-size: 0.8em; color: #999; }
    .totals .value { font-size: 1.2em; font-weight: bold; }
    .empty { color: #666; font-style: italic; }
    .generated { color: #666; font-size: 0.85em; margin-top: 1em; }
  </style>
</head>
<body>
  <h1>Symphony Dashboard</h1>

  <h2>Totals</h2>
  <div class="totals">
    <div><div class="label">Input Tokens</div><div class="value">{{.Totals.InputTokens}}</div></div>
    <div><div class="label">Output Tokens</div><div class="value">{{.Totals.OutputTokens}}</div></div>
    <div><div class="label">Total Tokens</div><div class="value">{{.Totals.TotalTokens}}</div></div>
    <div><div class="label">Runtime</div><div class="value">{{printf "%.1f" .Totals.SecondsRunning}}s</div></div>
  </div>

  <h2>Running Sessions ({{len .Running}})</h2>
  {{if .Running}}
  <table>
    <tr>
      <th>Identifier</th>
      <th>State</th>
      <th>Session</th>
      <th>Turns</th>
      <th>Last Event</th>
      <th>Tokens (in/out)</th>
      <th>Started</th>
    </tr>
    {{range .Running}}
    <tr>
      <td>{{.IssueIdentifier}}</td>
      <td>{{.State}}</td>
      <td>{{.SessionID}}</td>
      <td>{{.TurnCount}}</td>
      <td>{{.LastEvent}}</td>
      <td>{{.Tokens.InputTokens}} / {{.Tokens.OutputTokens}}</td>
      <td>{{.StartedAt.Format "15:04:05"}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}
  <p class="empty">No running sessions.</p>
  {{end}}

  <h2>Retry Queue ({{len .Retrying}})</h2>
  {{if .Retrying}}
  <table>
    <tr>
      <th>Identifier</th>
      <th>Attempt</th>
      <th>Due At</th>
      <th>Error</th>
    </tr>
    {{range .Retrying}}
    <tr>
      <td>{{.IssueIdentifier}}</td>
      <td>{{.Attempt}}</td>
      <td>{{.DueAt.Format "15:04:05"}}</td>
      <td>{{.Error}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}
  <p class="empty">No issues in retry queue.</p>
  {{end}}

  <p class="generated">Generated at {{.GeneratedAt.Format "2006-01-02 15:04:05 UTC"}}</p>
</body>
</html>`

var dashTmpl = template.Must(template.New("dashboard").Parse(dashboardTemplate))

type dashboardData struct {
	GeneratedAt time.Time
	Running     []dashRunning
	Retrying    []dashRetrying
	Totals      dashTotals
}

type dashRunning struct {
	StartedAt       time.Time
	IssueIdentifier string
	State           string
	SessionID       string
	LastEvent       string
	Tokens          dashTotals
	TurnCount       int
}

type dashRetrying struct {
	DueAt           time.Time
	IssueIdentifier string
	Error           string
	Attempt         int
}

type dashTotals struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	SecondsRunning float64
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}

	snap, err := s.orch.RequestSnapshot(r.Context())
	if err != nil {
		http.Error(w, "failed to get snapshot", http.StatusInternalServerError)
		return
	}

	data := dashboardData{
		GeneratedAt: snap.GeneratedAt,
		Totals: dashTotals{
			InputTokens:    snap.Totals.InputTokens,
			OutputTokens:   snap.Totals.OutputTokens,
			TotalTokens:    snap.Totals.TotalTokens,
			SecondsRunning: snap.Totals.SecondsRunning,
		},
	}

	for i := range snap.Running {
		rs := &snap.Running[i]
		data.Running = append(data.Running, dashRunning{
			IssueIdentifier: rs.IssueIdentifier,
			State:           rs.State,
			SessionID:       rs.SessionID,
			TurnCount:       rs.TurnCount,
			LastEvent:       rs.LastEvent,
			StartedAt:       rs.StartedAt,
			Tokens: dashTotals{
				InputTokens:  rs.Tokens.InputTokens,
				OutputTokens: rs.Tokens.OutputTokens,
				TotalTokens:  rs.Tokens.TotalTokens,
			},
		})
	}

	for _, rt := range snap.Retrying {
		data.Retrying = append(data.Retrying, dashRetrying{
			IssueIdentifier: rt.IssueIdentifier,
			Attempt:         rt.Attempt,
			DueAt:           rt.DueAt,
			Error:           rt.Error,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashTmpl.Execute(w, data); err != nil {
		s.logger.Error("dashboard template error", "error", err)
	}
}
