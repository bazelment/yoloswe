// Package replay provides unified parsing of session logs (Claude and Codex)
// into bramble's OutputLine format for replay rendering.
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bazelment/yoloswe/bramble/session"
)

// Format identifies the session log format.
type Format string

const (
	FormatClaude   Format = "claude"
	FormatCodex    Format = "codex"
	FormatRawJSONL Format = "raw_jsonl" // ~/.claude/projects/ native format
)

// Result holds the parsed output from any session log format.
type Result struct { //nolint:govet // fieldalignment: readability over packing
	Lines  []session.OutputLine
	Prompt string
	Status session.SessionStatus
	Format Format
}

// Parse auto-detects the log format and returns a unified Result.
func Parse(path string) (*Result, error) {
	format, err := DetectFormat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to detect log format: %w", err)
	}

	switch format {
	case FormatClaude:
		return parseClaudeLog(path)
	case FormatCodex:
		return parseCodexLog(path)
	case FormatRawJSONL:
		return parseRawJSONL(path)
	default:
		return nil, fmt.Errorf("unsupported log format: %q", format)
	}
}

// DetectFormat determines the session format from the path.
func DetectFormat(path string) (Format, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	if info.IsDir() {
		// Directory with messages.jsonl = Claude format
		if _, err := os.Stat(filepath.Join(path, "messages.jsonl")); err == nil {
			return FormatClaude, nil
		}
		return "", fmt.Errorf("directory missing messages.jsonl")
	}

	// Single file - check header for format
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	const maxScanTokenSize = 10 * 1024 * 1024 // 10 MB, matches LoadFromRawJSONL
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxScanTokenSize), maxScanTokenSize)
	if scanner.Scan() {
		line := scanner.Bytes()

		// Check for Codex format header
		var header struct {
			Format string `json:"format"`
		}
		if json.Unmarshal(line, &header) == nil && header.Format == "codex" {
			return FormatCodex, nil
		}

		// Check for Claude session JSONL (has "direction" field)
		var claudeCheck struct {
			Direction string `json:"direction"`
		}
		if json.Unmarshal(line, &claudeCheck) == nil &&
			(claudeCheck.Direction == "sent" || claudeCheck.Direction == "received") {
			return FormatClaude, nil
		}

		// Check for raw JSONL (~/.claude/projects/) — has known envelope types.
		// The first line is often file-history-snapshot (no sessionId),
		// so check the type field alone.
		var rawCheck struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &rawCheck) == nil {
			switch rawCheck.Type {
			case "file-history-snapshot", "queue-operation", "pr-link":
				return FormatRawJSONL, nil
			case "user", "assistant", "system", "result", "progress":
				// These also appear in SDK recorder format, but SDK recorder
				// lines have "direction" which we already checked above.
				return FormatRawJSONL, nil
			}
		}
	}

	return "", fmt.Errorf("unable to detect log format for %s", path)
}
