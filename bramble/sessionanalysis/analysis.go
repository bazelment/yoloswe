// Package sessionanalysis provides structured analysis of Claude Code JSONL
// session files. It parses raw JSONL into a hierarchy of Sessions and Turns,
// where each Turn represents a user input followed by the agent's response.
package sessionanalysis

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
	"github.com/bazelment/yoloswe/bramble/sessionmodel"
)

// Session represents a single Claude Code session parsed from a JSONL file.
type Session struct { //nolint:govet // fieldalignment: readability over packing
	ID        string    `json:"id"`
	FilePath  string    `json:"file_path"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Model     string    `json:"model,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
	GitBranch string    `json:"git_branch,omitempty"`
	Turns     []Turn    `json:"turns"`
	Summary   string    `json:"summary,omitempty"`
}

// ToolCountsAggregate returns aggregated tool call counts across all turns.
func (s *Session) ToolCountsAggregate() map[string]int {
	counts := make(map[string]int)
	for i := range s.Turns {
		for _, tc := range s.Turns[i].ToolCalls {
			counts[tc.Name]++
		}
	}
	return counts
}

// FormatToolCounts returns a compact string like "Bash:10, Read:5, Edit:3".
func (s *Session) FormatToolCounts() string {
	return formatCountMap(s.ToolCountsAggregate())
}

// Duration returns the total session duration.
func (s *Session) Duration() time.Duration {
	if s.StartTime.IsZero() || s.EndTime.IsZero() {
		return 0
	}
	return s.EndTime.Sub(s.StartTime)
}

// TotalToolCalls returns the count of tool invocations across all turns.
func (s *Session) TotalToolCalls() int {
	n := 0
	for i := range s.Turns {
		n += len(s.Turns[i].ToolCalls)
	}
	return n
}

// Turn represents one user-agent interaction: the user's input followed by
// the agent's response (text, tool calls, errors).
type Turn struct { //nolint:govet // fieldalignment: readability over packing
	Number    int       `json:"number"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`

	// UserInput is the text the user typed.
	UserInput string `json:"user_input"`

	// Response is the concatenated agent text output for this turn.
	Response string `json:"response"`

	// ResponseSummary is a shortened version if Response exceeds the word limit.
	// Empty if the response is short enough.
	ResponseSummary string `json:"response_summary,omitempty"`

	// ToolCalls lists tools invoked during this turn.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// Errors collects any errors that occurred during this turn.
	Errors []string `json:"errors,omitempty"`

	// CostUSD is the cumulative cost reported at end of turn (0 if unavailable).
	CostUSD float64 `json:"cost_usd,omitempty"`

	// DurationMs is the turn duration in milliseconds (from result message).
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// ResponseWordCount returns the word count of the response text.
func (t *Turn) ResponseWordCount() int {
	return len(strings.Fields(t.Response))
}

// FormatToolCounts returns a compact string like "Bash:3, Read:2".
func (t *Turn) FormatToolCounts() string {
	if len(t.ToolCalls) == 0 {
		return ""
	}
	counts := make(map[string]int)
	for _, tc := range t.ToolCalls {
		counts[tc.Name]++
	}
	return formatCountMap(counts)
}

// formatCountMap formats a name→count map as "Name:N, ..." sorted by count descending.
func formatCountMap(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	type kv struct {
		name  string
		count int
	}
	var pairs []kv
	for name, count := range counts {
		pairs = append(pairs, kv{name, count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].count > pairs[j].count
	})
	var parts []string
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%s:%d", p.name, p.count))
	}
	return strings.Join(parts, ", ")
}

// ToolCall represents a single tool invocation within a turn.
type ToolCall struct {
	ID       string                 `json:"id,omitempty"`
	Name     string                 `json:"name"`
	Input    map[string]interface{} `json:"input,omitempty"`
	State    string                 `json:"state"` // "running", "complete", "error"
	Duration time.Duration          `json:"duration,omitempty"`
}

// Config controls analysis behavior.
type Config struct { //nolint:govet // fieldalignment: readability over packing
	// Since filters sessions to only include those started after this time.
	// Zero value means no filtering.
	Since time.Time

	// SkipEmpty filters out turns with empty agent responses (e.g. local
	// commands, slash commands).
	SkipEmpty bool

	// MinTurns excludes sessions with fewer than this many turns.
	MinTurns int
}

// DefaultConfig returns the default analysis configuration.
func DefaultConfig() Config {
	return Config{
		SkipEmpty: true,
	}
}

// ParseSession parses a single JSONL file into a Session.
func ParseSession(path string) (*Session, error) {
	return ParseSessionWithConfig(path, DefaultConfig())
}

// ParseSessionWithConfig parses a single JSONL file with custom config.
func ParseSessionWithConfig(path string, cfg Config) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	sess := &Session{
		FilePath: path,
		ID:       strings.TrimSuffix(filepath.Base(path), ".jsonl"),
	}

	var (
		currentTurn *Turn
		turnNum     int
		gitBranch   string
	)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		msg, meta, err := sessionmodel.FromRawJSONL(line)
		if err != nil {
			continue
		}

		// Extract envelope metadata.
		if meta != nil {
			if sess.ID == "" && meta.SessionID != "" {
				sess.ID = meta.SessionID
			}
			if meta.GitBranch != "" {
				gitBranch = meta.GitBranch
			}
			if !meta.Timestamp.IsZero() {
				if sess.StartTime.IsZero() || meta.Timestamp.Before(sess.StartTime) {
					sess.StartTime = meta.Timestamp
				}
				if meta.Timestamp.After(sess.EndTime) {
					sess.EndTime = meta.Timestamp
				}
			}

			// Handle envelope-only types.
			if msg == nil {
				if meta.Type == "system" && meta.Subtype == "api_error" {
					if currentTurn != nil {
						errContent := "API error"
						if len(meta.ErrorJSON) > 0 {
							var errObj struct {
								Cause struct {
									Code string `json:"code"`
								} `json:"cause"`
							}
							if json.Unmarshal(meta.ErrorJSON, &errObj) == nil && errObj.Cause.Code != "" {
								errContent = fmt.Sprintf("API error: %s", errObj.Cause.Code)
							}
						}
						currentTurn.Errors = append(currentTurn.Errors, errContent)
					}
				}
				continue
			}
		}

		switch m := msg.(type) {
		case protocol.SystemMessage:
			if m.Subtype == "init" {
				sess.Model = m.Model
				sess.CWD = m.CWD
			}

		case protocol.UserMessage:
			// String content = new user prompt = new turn.
			if s, ok := m.Message.Content.AsString(); ok && s != "" {
				// Finalize previous turn.
				if currentTurn != nil {
					finalizeTurn(currentTurn, cfg)
					if !cfg.SkipEmpty || !isEmptyTurn(currentTurn) {
						sess.Turns = append(sess.Turns, *currentTurn)
					}
				}
				turnNum++
				currentTurn = &Turn{
					Number:    turnNum,
					UserInput: cleanUserInput(s, meta),
					StartTime: meta.Timestamp,
				}
			}
			// Block content with tool_result updates current turn's tool states.
			if blocks, ok := m.Message.Content.AsBlocks(); ok && currentTurn != nil {
				for _, block := range blocks {
					if tr, ok := block.(protocol.ToolResultBlock); ok {
						updateToolResult(currentTurn, tr)
					}
				}
			}

		case protocol.AssistantMessage:
			if currentTurn == nil {
				continue
			}
			blocks, ok := m.Message.Content.AsBlocks()
			if !ok {
				if s, ok := m.Message.Content.AsString(); ok && s != "" {
					currentTurn.Response += s
				}
				continue
			}
			for _, block := range blocks {
				switch b := block.(type) {
				case protocol.TextBlock:
					if b.Text != "" {
						if currentTurn.Response != "" {
							currentTurn.Response += "\n"
						}
						currentTurn.Response += b.Text
					}
				case protocol.ToolUseBlock:
					currentTurn.ToolCalls = append(currentTurn.ToolCalls, ToolCall{
						Name:  b.Name,
						ID:    b.ID,
						Input: b.Input,
						State: "running",
					})
				}
			}

		case protocol.ResultMessage:
			if currentTurn != nil {
				currentTurn.CostUSD = m.TotalCostUSD
				currentTurn.DurationMs = m.DurationMs
				if meta != nil && !meta.Timestamp.IsZero() {
					currentTurn.EndTime = meta.Timestamp
				}
				if m.IsError {
					currentTurn.Errors = append(currentTurn.Errors, "Turn ended with error")
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan JSONL: %w", err)
	}

	// Finalize the last turn.
	if currentTurn != nil {
		finalizeTurn(currentTurn, cfg)
		if !cfg.SkipEmpty || !isEmptyTurn(currentTurn) {
			sess.Turns = append(sess.Turns, *currentTurn)
		}
	}

	sess.GitBranch = gitBranch
	if len(sess.Turns) > 0 {
		sess.Summary = generateSessionSummary(sess)
	}

	return sess, nil
}

// ParseProject parses all JSONL files in a project directory, returning
// sessions sorted by start time.
func ParseProject(projectDir string) ([]*Session, error) {
	return ParseProjectWithConfig(projectDir, DefaultConfig())
}

// ParseProjectWithConfig parses all JSONL files with custom config.
func ParseProjectWithConfig(projectDir string, cfg Config) ([]*Session, error) {
	matches, err := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("glob JSONL files: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no JSONL files found in %s", projectDir)
	}

	var sessions []*Session
	for _, path := range matches {
		sess, err := ParseSessionWithConfig(path, cfg)
		if err != nil {
			continue // skip unparseable files
		}
		if len(sess.Turns) == 0 {
			continue // skip empty sessions
		}
		if cfg.MinTurns > 0 && len(sess.Turns) < cfg.MinTurns {
			continue
		}
		if !cfg.Since.IsZero() && sess.StartTime.Before(cfg.Since) {
			continue // skip sessions before the time filter
		}
		sessions = append(sessions, sess)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.Before(sessions[j].StartTime)
	})

	return sessions, nil
}

// ParseAllProjects parses sessions from all project directories.
func ParseAllProjects(cfg Config) ([]*Session, error) {
	projects, err := ListProjects()
	if err != nil {
		return nil, err
	}

	var all []*Session
	for _, p := range projects {
		sessions, err := ParseProjectWithConfig(p.Path, cfg)
		if err != nil {
			continue
		}
		all = append(all, sessions...)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].StartTime.Before(all[j].StartTime)
	})
	return all, nil
}

// ListProjects returns the available project directories under ~/.claude/projects/.
func ListProjects() ([]ProjectInfo, error) {
	base := filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read projects dir: %w", err)
	}

	var projects []ProjectInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		if len(matches) == 0 {
			continue
		}
		projects = append(projects, ProjectInfo{
			Name:         e.Name(),
			Path:         dir,
			SessionCount: len(matches),
		})
	}
	return projects, nil
}

// ProjectInfo describes a project directory.
type ProjectInfo struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	SessionCount int    `json:"session_count"`
}

// finalizeTurn applies post-processing to a completed turn.
func finalizeTurn(t *Turn, cfg Config) {
	t.Response = strings.TrimSpace(t.Response)
}

// updateToolResult updates the matching tool call state in the turn.
// It matches by tool use ID when available, falling back to the first
// running tool call for backward compatibility.
func updateToolResult(t *Turn, tr protocol.ToolResultBlock) {
	idx := -1
	if tr.ToolUseID != "" {
		for i := range t.ToolCalls {
			if t.ToolCalls[i].ID == tr.ToolUseID {
				idx = i
				break
			}
		}
	}
	if idx == -1 {
		// Fallback: match the first running tool call by order.
		for i := range t.ToolCalls {
			if t.ToolCalls[i].State == "running" {
				idx = i
				break
			}
		}
	}
	if idx == -1 {
		return
	}
	isError := tr.IsError != nil && *tr.IsError
	if isError {
		t.ToolCalls[idx].State = "error"
	} else {
		t.ToolCalls[idx].State = "complete"
	}
}

// isEmptyTurn returns true if a turn has no meaningful agent response, tool calls, or errors.
func isEmptyTurn(t *Turn) bool {
	return strings.TrimSpace(t.Response) == "" && len(t.ToolCalls) == 0 && len(t.Errors) == 0
}

// cleanUserInput uses envelope metadata to classify the message type and
// returns a cleaned representation. This avoids brittle XML tag parsing by
// relying on structured envelope fields (IsMeta, AgentName, SourceToolUseID).
func cleanUserInput(s string, meta *sessionmodel.RawEnvelopeMeta) string {
	if meta == nil {
		return strings.TrimSpace(s)
	}

	// Meta messages are system-generated (local command output, task notifications).
	if meta.IsMeta {
		return cleanMetaMessage(s)
	}

	// Agent messages are from teammate/subagent systems.
	if meta.AgentName != "" {
		return cleanAgentMessage(s, meta.AgentName)
	}

	// Human-typed message — check for task notifications (which lack
	// envelope metadata) and strip system-reminder injections.
	if strings.Contains(s, "<task-notification>") || strings.Contains(s, "<task-id>") {
		return cleanMetaMessage(s)
	}
	s = stripXMLTag(s, "system-reminder")
	return strings.TrimSpace(s)
}

// cleanMetaMessage extracts a readable label from a system meta-message.
func cleanMetaMessage(s string) string {
	// Task notifications: extract the <summary> content.
	if strings.Contains(s, "<task-notification>") || strings.Contains(s, "<task-id>") {
		if start := strings.Index(s, "<summary>"); start != -1 {
			inner := s[start+len("<summary>"):]
			if end := strings.Index(inner, "</summary>"); end != -1 {
				return "[task notification] " + strings.TrimSpace(inner[:end])
			}
		}
		return "[task notification]"
	}

	// Local command output: strip wrapper tags, keep the content.
	for _, tag := range []string{"local-command-caveat", "local-command-stdout", "command-name", "command-message", "command-args"} {
		s = stripXMLTag(s, tag)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "[system meta]"
	}
	return s
}

// cleanAgentMessage extracts a readable description from a teammate message.
func cleanAgentMessage(s string, agentName string) string {
	prefix := fmt.Sprintf("[%s] ", agentName)

	// Extract content from <teammate-message> wrapper if present.
	if tagStart := strings.Index(s, "<teammate-message"); tagStart != -1 {
		if start := strings.Index(s[tagStart:], ">"); start != -1 {
			inner := s[tagStart+start+1:]
			if end := strings.Index(inner, "</teammate-message>"); end != -1 {
				inner = inner[:end]
			}
			inner = strings.TrimSpace(inner)
			if taskIdx := strings.Index(inner, "Your task"); taskIdx != -1 {
				return prefix + strings.TrimSpace(inner[taskIdx:])
			}
			if len(inner) > 300 {
				inner = inner[:300] + "..."
			}
			return prefix + inner
		}
	}

	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return prefix + strings.TrimSpace(s)
}

// stripXMLTag removes a simple XML tag and returns the inner text.
func stripXMLTag(s, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	s = strings.ReplaceAll(s, open, "")
	s = strings.ReplaceAll(s, close, "")
	// Also handle tags with attributes.
	for {
		start := strings.Index(s, "<"+tag+" ")
		if start == -1 {
			break
		}
		end := strings.Index(s[start:], ">")
		if end == -1 {
			break
		}
		s = s[:start] + s[start+end+1:]
	}
	return s
}

// generateSessionSummary creates a brief summary of what the session accomplished.
func generateSessionSummary(sess *Session) string {
	if len(sess.Turns) == 0 {
		return ""
	}

	var b strings.Builder

	// Goal: first user input (truncated).
	firstInput := sess.Turns[0].UserInput
	if len(firstInput) > 200 {
		firstInput = firstInput[:200] + "..."
	}
	b.WriteString("Goal: " + firstInput)

	// Outcome: last turn's status.
	last := sess.Turns[len(sess.Turns)-1]
	b.WriteString("\n")
	if len(last.Errors) > 0 {
		b.WriteString(fmt.Sprintf("Outcome: Ended with errors (%d turns)", len(sess.Turns)))
	} else {
		b.WriteString(fmt.Sprintf("Outcome: Completed (%d turns)", len(sess.Turns)))
	}

	return b.String()
}
