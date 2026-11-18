package adopt

// This file contains the deliberately small, loss-aware corpus sanitizer. It
// keeps the host run's decision surface while replacing prose and dropping
// files that can contain credentials. The sanitizer never copies a checkout.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	sanitizeSecretRE = regexp.MustCompile(`(?i)(?:ghp_[A-Za-z0-9_\-]+|sk-[A-Za-z0-9_\-]+|AIza[A-Za-z0-9_\-]+|xox[baprs]-[A-Za-z0-9_\-]+|BEGIN [^-\n]*PRIVATE KEY)`)
	sanitizeSHARE    = regexp.MustCompile(`(?i)\b[0-9a-f]{7,64}\b`)
	sanitizePRRE     = regexp.MustCompile(`#\d+`)
	sanitizeTokenRE  = regexp.MustCompile(`(?i)\b(?:still\s+open|measurement\s+lane:\s*never\s+opens\s+a\s+pr|never\s+opens\s+a\s+pr|merge[_ -]?gate|exact\s+head|build\s+and\s+test|lint|soak|a/b|ci|checks?|review|integrat(?:e|ed|ion)|merged|closed|open|no[- ]pr|phase|status|done|blocked|working|running|planned|failed|needs[- ](?:input|swe|human)|hold[- ]advance|unmerged|target|base|swe|clean|analysis|plan|test|build|deploy|verdict|pass|passed|green|required|must|result|safety)\b`)
)

// SanitizeDir writes a credential-free, shape-preserving run copy. The output
// may be passed directly to DryRunDir. Existing output is rejected to avoid
// accidentally mixing two corpus snapshots.
func SanitizeDir(input, output string) error {
	input = filepath.Clean(strings.TrimSpace(input))
	output = filepath.Clean(strings.TrimSpace(output))
	if input == "" || output == "" || input == "." || output == "." {
		return errors.New("sanitize: input and output directories are required")
	}
	if input == output {
		return errors.New("sanitize: output must differ from input")
	}
	if rel, err := filepath.Rel(input, output); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "." {
		return errors.New("sanitize: output must not be inside input")
	}
	if _, err := os.Stat(filepath.Join(input, "state.json")); err != nil {
		return fmt.Errorf("sanitize: input is not a run directory: %w", err)
	}
	if info, err := os.Stat(output); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("sanitize: output already exists: %s", output)
		}
		entries, readErr := os.ReadDir(output)
		if readErr != nil {
			return fmt.Errorf("sanitize: read output: %w", readErr)
		}
		if len(entries) != 0 {
			return fmt.Errorf("sanitize: output already exists and is not empty: %s", output)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sanitize: stat output: %w", err)
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return fmt.Errorf("sanitize: create output: %w", err)
	}
	if err := filepath.WalkDir(input, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(input, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if secretPath(rel) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		dst := filepath.Join(output, rel)
		if entry.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		cleaned := sanitizeFile(filepath.Base(path), rel, data)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, cleaned, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("sanitize: copy: %w", err)
	}
	if err := writeTopologyScript(input, output); err != nil {
		return err
	}
	return rejectSecrets(output)
}

// Sanitize is a short alias for callers that do not need the directory suffix.
func Sanitize(input, output string) error { return SanitizeDir(input, output) }

func secretPath(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts {
		lower := strings.ToLower(part)
		if sanitizeSecretRE.MatchString(part) {
			return true
		}
		if lower == "secrets" || lower == ".creds" || lower == "lanes.env" || lower == ".orca-lease.json" || lower == "gh-usage.jsonl" {
			return true
		}
		if strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".pem") {
			return true
		}
	}
	return false
}

func sanitizeFile(base, rel string, data []byte) []byte {
	if base == "state.json" {
		if out, err := sanitizeStateJSON(data); err == nil {
			return out
		}
	}
	if strings.HasSuffix(strings.ToLower(base), ".json") && (strings.Contains(base, "spawn") || strings.Contains(base, "attn")) {
		if out, err := sanitizeSpawnJSON(data); err == nil {
			return out
		}
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return []byte("synthetic binary omitted\n")
	}
	if base == "EVENTS" {
		return []byte(sanitizeEvents(string(data)))
	}
	if strings.HasPrefix(base, ".hold-advance") || strings.Contains(base, "attn") || strings.Contains(base, "handled") || strings.HasSuffix(base, ".md") || base == "env.sh" || filepath.Ext(rel) == "" {
		return []byte(sanitizeText(string(data)))
	}
	return []byte(sanitizeText(string(data)))
}

func sanitizeStateJSON(data []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	value = sanitizeJSONValue("", value, false)
	return json.MarshalIndent(value, "", "  ")
}

func sanitizeSpawnJSON(data []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	value = sanitizeJSONValue("spawn", value, true)
	return json.MarshalIndent(value, "", "  ")
}

func sanitizeJSONValue(key string, value any, spawn bool) any {
	switch x := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = sanitizeJSONValue(k, v, spawn || strings.EqualFold(k, "spawn"))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = sanitizeJSONValue(key, v, spawn)
		}
		return out
	case string:
		if preserveJSONString(key, spawn) {
			return sanitizeJSONString(key, x, spawn)
		}
		return neutralText(x)
	default:
		return value
	}
}

func preserveJSONString(key string, spawn bool) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if spawn && (key == "terminal_id" || key == "session_id") {
		return true
	}
	switch key {
	case "id", "phase", "status", "branch", "target", "base", "merge_sha", "pr", "pr_head", "pr_state", "terminal_state", "mode", "checks", "waiting_on", "deadline", "merge_gate", "merge_rule", "no_pr", "round":
		return true
	default:
		return false
	}
}

func sanitizeJSONString(key, value string, spawn bool) string {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "id", "branch", "target", "base", "waiting_on", "deadline":
		return sanitizeRef(value)
	case "merge_gate", "merge_rule":
		if sanitizeGateRE.MatchString(strings.TrimSpace(value)) && !sanitizeSecretRE.MatchString(value) {
			return strings.TrimSpace(value)
		}
		return sanitizeDecisionTokens(value)
	case "session_id", "terminal_id":
		if spawn {
			return sanitizeRef(value)
		}
	}
	return sanitizeDecisionTokens(value)
}

var sanitizeGateRE = regexp.MustCompile(`(?i)^[a-z0-9/&+_:. -]+$`)

func sanitizeRef(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	value = sanitizeSecretRE.ReplaceAllString(value, "synthetic")
	value = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-/_ .:", r) {
			return r
		}
		return '-'
	}, value)
	return strings.Trim(value, " .")
}

func neutralText(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return "synthetic text"
}

func sanitizeDecisionTokens(s string) string {
	var tokens []string
	for _, re := range []*regexp.Regexp{sanitizePRRE, sanitizeSHARE, sanitizeTokenRE} {
		tokens = append(tokens, re.FindAllString(s, -1)...)
	}
	if len(tokens) == 0 {
		return neutralText(s)
	}
	sort.Slice(tokens, func(i, j int) bool { return len(tokens[i]) < len(tokens[j]) })
	seen := map[string]bool{}
	var out []string
	for _, token := range tokens {
		key := strings.ToLower(token)
		if !seen[key] {
			seen[key] = true
			out = append(out, token)
		}
	}
	return strings.Join(out, " ")
}

func sanitizeText(s string) string {
	var out []string
	for _, line := range strings.SplitAfter(s, "\n") {
		trim := strings.TrimSuffix(line, "\n")
		prefix := ""
		value := trim
		if key, rest, ok := strings.Cut(trim, ":"); ok {
			prefix, value = key+":", rest
		}
		clean := sanitizeDecisionTokens(value)
		if clean == "synthetic text" || clean == "" {
			clean = "synthetic text"
		}
		if prefix != "" {
			clean = prefix + " " + clean
		}
		if strings.TrimSpace(trim) == "" {
			clean = ""
		}
		out = append(out, clean+map[bool]string{true: "\n", false: ""}[strings.HasSuffix(line, "\n")])
	}
	return strings.Join(out, "")
}

func sanitizeEvents(s string) string {
	var out []string
	for _, line := range strings.SplitAfter(s, "\n") {
		ending := ""
		if strings.HasSuffix(line, "\n") {
			ending, line = "\n", strings.TrimSuffix(line, "\n")
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) >= 3 {
			if len(parts) == 4 {
				parts[3] = sanitizeDecisionTokens(parts[3])
			}
			line = strings.Join(parts, "\t")
		} else {
			line = sanitizeText(line)
		}
		out = append(out, line+ending)
	}
	return strings.Join(out, "")
}

func writeTopologyScript(input, output string) error {
	state, _ := os.ReadFile(filepath.Join(input, "state.json"))
	var probe struct {
		Tasks []struct {
			Branch string `json:"branch"`
			PR     int    `json:"pr"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(state, &probe)
	var lines []string
	for _, task := range probe.Tasks {
		branch := strings.TrimSpace(task.Branch)
		if branch == "" {
			continue
		}
		subject := "synthetic lane"
		if task.PR > 0 {
			subject += fmt.Sprintf(" (#%d)", task.PR)
		}
		lines = append(lines, fmt.Sprintf("git -C \"$work\" checkout -B %s\n git -C \"$work\" -c user.name=synthetic -c user.email=synthetic@example.invalid commit --allow-empty -m %s >/dev/null\n git -C \"$work\" push -u origin %s >/dev/null", shellQuote(branch), shellQuote(subject), shellQuote(branch)))
	}
	script := "#!/bin/sh\nset -eu\n# Generated by orca adopt sanitize; no source tree is copied.\nout=${1:?bare repo path}\nwork=${2:-${out}.work}\ngit init --bare \"$out\" >/dev/null\ngit init \"$work\" >/dev/null\ngit -C \"$work\" remote add origin \"$out\" 2>/dev/null || true\n" + strings.Join(lines, "\n") + "\n"
	return os.WriteFile(filepath.Join(output, "git-topology.sh"), []byte(script), 0o755)
}

func shellQuote(s string) string { return strconv.Quote(s) }

func rejectSecrets(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if sanitizeSecretRE.Match(data) {
			return fmt.Errorf("sanitize: kept file %s still matches a secret pattern", path)
		}
		return nil
	})
}
