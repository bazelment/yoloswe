package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// CLIClient invokes a locally authenticated claude or codex CLI. It relies on
// the CLI's authentication and never reads credential files.
type CLIClient struct {
	provider, path string
	extra          []string
	mu             sync.Mutex
	total          Usage
}

// CLIClientOption configures a CLI client.
type CLIClientOption func(*CLIClient)

// WithCLIPath overrides the executable, for example for deterministic tests.
func WithCLIPath(path string) CLIClientOption { return func(c *CLIClient) { c.path = path } }

// WithCLIExtraArgs adds provider-specific command arguments.
func WithCLIExtraArgs(args ...string) CLIClientOption {
	return func(c *CLIClient) { c.extra = append([]string(nil), args...) }
}

// NewCLIClient constructs a client for claude-cli or codex-cli.
func NewCLIClient(provider string, opts ...CLIClientOption) *CLIClient {
	c := &CLIClient{provider: strings.ToLower(provider)}
	if c.provider == "claude-cli" {
		c.path = os.Getenv("ORCA_CLAUDE_CLI")
		if c.path == "" {
			c.path = "claude"
		}
	} else {
		c.path = os.Getenv("ORCA_CODEX_CLI")
		if c.path == "" {
			c.path = "codex"
		}
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// TotalUsage returns usage across completed calls.
func (c *CLIClient) TotalUsage() Usage { c.mu.Lock(); defer c.mu.Unlock(); return c.total }

// ResetUsage clears accumulated usage.
func (c *CLIClient) ResetUsage() { c.mu.Lock(); defer c.mu.Unlock(); c.total = Usage{} }

// RunToolLoop executes model-selected tools through the supplied registry.
func (c *CLIClient) RunToolLoop(ctx context.Context, req Request, reg *ToolRegistry, opts ...ToolLoopOption) (*ToolLoopResult, error) {
	return RunToolLoop(ctx, c, req, reg, opts...)
}

type cliEnvelope struct {
	Result       string  `json:"result"`
	Model        string  `json:"model"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        Usage   `json:"usage"`
}
type cliReply struct {
	Text  *string         `json:"text"`
	Tool  string          `json:"tool"`
	Name  string          `json:"name"`
	ID    string          `json:"id"`
	Input json.RawMessage `json:"input"`
}

// CreateMessage invokes one CLI turn, or runs the registry loop when requested.
func (c *CLIClient) CreateMessage(ctx context.Context, req Request) (*Response, error) {
	if req.ToolRegistry != nil {
		return runToolLoopResponse(ctx, c, req)
	}
	if c.provider != "claude-cli" && c.provider != "codex-cli" {
		return nil, fmt.Errorf("orca/model: unsupported CLI provider %q", c.provider)
	}
	prompt, err := buildCLIPrompt(req)
	if err != nil {
		return nil, err
	}
	args := append([]string{}, c.extra...)
	if c.provider == "claude-cli" {
		args = append(args, "-p", "--no-session-persistence", "--output-format", "json", "--tools", "")
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.ReasoningEffort != "" {
			args = append(args, "--effort", req.ReasoningEffort)
		}
	} else {
		args = append(args, "exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check", "--ephemeral")
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.ReasoningEffort != "" {
			args = append(args, "-c", "model_reasoning_effort="+req.ReasoningEffort)
		}
		args = append(args, "-")
	}
	cmd := exec.CommandContext(ctx, c.path, args...)
	// Keep native CLI tools away from the repository and its settings. Authentication
	// still comes from the caller's environment, while the working directory is empty.
	workDir, err := os.MkdirTemp("", "orca-cli-")
	if err != nil {
		return nil, fmt.Errorf("orca/model: create CLI workdir: %w", err)
	}
	defer os.RemoveAll(workDir)
	cmd.Dir = workDir
	if strings.ContainsRune(c.path, os.PathSeparator) && !strings.HasPrefix(c.path, string(os.PathSeparator)) {
		if abs, err := os.Getwd(); err == nil {
			cmd.Path = filepath.Join(abs, c.path)
		}
	}
	cmd.Stdin = strings.NewReader(prompt)
	var out bytes.Buffer
	cmd.Stdout = &out
	// CLI diagnostics may include auth or endpoint details. Report the exit
	// status rather than copying untrusted stderr into persisted decisions.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("orca/model: %s invocation failed: %w", c.provider, err)
	}
	var env cliEnvelope
	if c.provider == "codex-cli" {
		env, err = decodeCodexJSONL(out.Bytes())
	} else {
		err = json.Unmarshal(out.Bytes(), &env)
		if err == nil && env.IsError {
			err = fmt.Errorf("CLI reported an unsuccessful result")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("orca/model: %s response: %w", c.provider, err)
	}
	if strings.TrimSpace(env.Result) == "" {
		return nil, fmt.Errorf("orca/model: %s returned no assistant message", c.provider)
	}
	resp := &Response{Model: req.Model, Usage: env.Usage, StopReason: StopReasonEndTurn, Turns: 1}
	if env.Model != "" {
		resp.Model = env.Model
	}
	if resp.Usage.CostUSD == 0 && env.TotalCostUSD > 0 {
		resp.Usage.CostUSD = env.TotalCostUSD
	}
	if resp.Usage.CostUSD == 0 {
		modelName := resp.Model
		if modelName == "" {
			modelName = "sonnet"
		}
		resp.Usage.CostUSD = CalculateCost(modelName, resp.Usage, nil)
	}
	var reply cliReply
	var replies []cliReply
	text := strings.TrimSpace(env.Result)
	// Both CLIs occasionally fence their otherwise valid JSON reply.
	if strings.HasPrefix(text, "```json\n") && strings.HasSuffix(text, "\n```") {
		text = strings.TrimSuffix(strings.TrimPrefix(text, "```json\n"), "\n```")
	}
	if strings.HasPrefix(text, "[") && json.Unmarshal([]byte(text), &replies) == nil && len(replies) > 0 {
		var blocks []ContentBlock
		seenIDs := make(map[string]bool, len(replies))
		unnamed := 0
		for i, r := range replies {
			name := strings.TrimSpace(r.Tool)
			if name == "" {
				name = strings.TrimSpace(r.Name)
			}
			if name == "" {
				unnamed++
				continue
			}
			if len(r.Input) == 0 {
				r.Input = json.RawMessage("{}")
			}
			var input map[string]json.RawMessage
			if err := json.Unmarshal(r.Input, &input); err != nil || input == nil {
				return nil, fmt.Errorf("orca/model: CLI tool input must be a JSON object")
			}
			id := strings.TrimSpace(r.ID)
			if id == "" || seenIDs[id] {
				for seq := i + 1; ; seq++ {
					candidate := fmt.Sprintf("cli-tool-%d", seq)
					if !seenIDs[candidate] {
						id = candidate
						break
					}
				}
			}
			seenIDs[id] = true
			blocks = append(blocks, NewToolUseBlock(id, name, r.Input))
		}
		if len(blocks) > 0 && unnamed > 0 {
			return nil, fmt.Errorf("orca/model: CLI tool array entry missing tool name")
		}
		if len(blocks) > 0 {
			resp.StopReason = StopReasonToolUse
			resp.Message = NewAssistantMessage(blocks...)
		} else {
			resp.Message = NewAssistantMessage(NewTextBlock(env.Result))
		}
	} else if json.Unmarshal([]byte(text), &reply) == nil && (reply.Tool != "" || reply.Name != "") {
		name := reply.Tool
		if name == "" {
			name = reply.Name
		}
		if len(reply.Input) == 0 {
			reply.Input = json.RawMessage("{}")
		}
		var input map[string]json.RawMessage
		if err := json.Unmarshal(reply.Input, &input); err != nil || input == nil {
			return nil, fmt.Errorf("orca/model: CLI tool input must be a JSON object")
		}
		if reply.ID == "" {
			reply.ID = "cli-tool-1"
		}
		resp.StopReason = StopReasonToolUse
		resp.Message = NewAssistantMessage(NewToolUseBlock(reply.ID, name, reply.Input))
	} else if reply.Text != nil {
		resp.Message = NewAssistantMessage(NewTextBlock(*reply.Text))
	} else {
		resp.Message = NewAssistantMessage(NewTextBlock(env.Result))
	}
	c.mu.Lock()
	c.total = c.total.Add(resp.Usage)
	c.mu.Unlock()
	return resp, nil
}

// Codex exec emits event JSONL, not a single JSON response. Only completed
// assistant items carry our textual tool protocol; native command events are
// CLI activity and must never be dispatched again as orca tools.
func decodeCodexJSONL(data []byte) (cliEnvelope, error) {
	var env cliEnvelope
	dec := json.NewDecoder(bytes.NewReader(data))
	completed := false
	for {
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage struct {
				Input  int64 `json:"input_tokens"`
				Cached int64 `json:"cached_input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		err := dec.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return env, fmt.Errorf("invalid Codex JSONL: %w", err)
		}
		switch event.Type {
		case "error", "turn.failed":
			return env, fmt.Errorf("codex reported %s", event.Type)
		case "item.completed":
			if event.Item.Type == "agent_message" {
				env.Result = event.Item.Text
			}
		case "turn.completed":
			completed = true
			// Codex input_tokens includes cache hits; Usage counts those separately.
			env.Usage = env.Usage.Add(Usage{InputTokens: max(0, event.Usage.Input-event.Usage.Cached), CacheReadInputTokens: event.Usage.Cached, OutputTokens: event.Usage.Output})
		}
	}
	if !completed {
		return env, fmt.Errorf("codex stream ended before turn.completed")
	}
	return env, nil
}

func buildCLIPrompt(req Request) (string, error) {
	// Preserve full typed blocks: Message.Text drops function calls, results,
	// error flags and IDs, all of which the next model turn needs.
	messages, err := json.Marshal(req.Messages)
	if err != nil {
		return "", err
	}
	tools, err := json.Marshal(req.Tools)
	if err != nil {
		return "", err
	}
	return `You are an orca model adapter. Follow the system instructions below and the supplied conversation. Native CLI tools are disabled by design; every orchestrator tool listed in TOOL DEFINITIONS is available in this session and is invoked by returning JSON. Select tools only from TOOL DEFINITIONS. Return ONLY JSON with {"text":"..."}, {"tool":"name","id":"unique-call-id","input":{...}}, or a JSON array of such tool-call objects. A tool response will be supplied in the next conversation turn; use its result before choosing the next action. Tool arguments must match the input_schema. Do not wrap JSON in markdown.
SYSTEM:
` + req.System + "\nCONVERSATION:\n" + string(messages) + "\nTOOL DEFINITIONS:\n" + string(tools) + "\n", nil
}
