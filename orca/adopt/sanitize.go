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
	sanitizeTokenRE  = regexp.MustCompile(`(?i)\b(?:still\s+open|measurement\s+lane:\s*never\s+opens\s+a\s+pr|never\s+opens\s+a\s+pr|merge[_ -]?gate|exact\s+head|build\s+and\s+test|lint|soak|a/b|ci|checks?|review|integrat(?:e|ed|ion)|merged|closed|open|no[- ]pr|phase|status|done|blocked|working|running|planned|failed|needs[- ](?:input|swe|human)|hold[- ]advance|unmerged|target|base|swe|clean|analysis|plan|test|build|deploy|verdict|pass|passed|green|required|must|result|safety|next|merge|complete|remaining|work|none|n/a)\b`)
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
	tmp, err := os.MkdirTemp(filepath.Dir(output), "."+filepath.Base(output)+".sanitize-")
	if err != nil {
		return fmt.Errorf("sanitize: create temporary output: %w", err)
	}
	defer os.RemoveAll(tmp)
	allow, err := sanitizeAllowlist(input)
	if err != nil {
		return fmt.Errorf("sanitize: build allowlist: %w", err)
	}
	if err := copySanitizedFiles(input, tmp, allow); err != nil {
		return err
	}
	if err := rejectSecrets(tmp); err != nil {
		return err
	}
	if info, statErr := os.Stat(output); statErr == nil && info.IsDir() {
		if removeErr := os.Remove(output); removeErr != nil {
			return fmt.Errorf("sanitize: replace output: %w", removeErr)
		}
	}
	if err := os.Rename(tmp, output); err != nil {
		return fmt.Errorf("sanitize: publish output: %w", err)
	}
	return nil
}

// Sanitize is a short alias for callers that do not need the directory suffix.
func Sanitize(input, output string) error { return SanitizeDir(input, output) }

type sanitizeTask struct {
	Terminals map[string]map[string]string `json:"terminals"`
	ID        string                       `json:"id"`
	Phase     string                       `json:"phase"`
	SessionID string                       `json:"session_id"`
}

// sanitizeAllowlist contains only files read by Read and the dry-run preview.
// In particular, prompts, briefs, history and random host scratch files never
// enter a corpus snapshot.
func sanitizeAllowlist(input string) (map[string]bool, error) {
	allow := map[string]bool{"state.json": true, "EVENTS": true, "OBJECTIVE.md": true, "CONTEXT.md": true, "ESCALATIONS.md": true}
	data, err := os.ReadFile(filepath.Join(input, "state.json"))
	if err != nil {
		return nil, err
	}
	var state struct {
		Tasks  []sanitizeTask `json:"tasks"`
		Config struct {
			Orchestrator string `json:"orchestrator_session_id"`
			Phases       []struct {
				Name string `json:"name"`
			} `json:"phases"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	lanes := map[string]bool{}
	phases := map[string]bool{}
	nebula := state.Config.Orchestrator != ""
	reportBest := map[string]struct {
		rel   string
		round int
		mtime int64
	}{}
	for _, p := range state.Config.Phases {
		if p.Name != "" {
			phases[p.Name] = true
		}
	}
	for _, task := range state.Tasks {
		if task.ID == "" {
			continue
		}
		lanes[task.ID] = true
		nebula = nebula || task.SessionID != "" || len(task.Terminals) > 0
		if task.Phase != "" {
			phases[task.Phase] = true
		}
	}
	for laneID := range lanes {
		allow[laneID+".hold-advance"] = true
	}
	// Walk only the small set of root/report names whose lane and phase are
	// present in state.json. This also permits all current spawn/attn markers,
	// while dropping .prev-* and unknown files.
	err = filepath.WalkDir(input, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			rel, relErr := filepath.Rel(input, path)
			if relErr == nil && rel != "." && secretPath(rel) {
				return fs.SkipDir
			}
			if filepath.Base(path) == "reports" && path != input {
				return nil
			}
			return nil
		}
		rel, relErr := filepath.Rel(input, path)
		if relErr != nil || secretPath(rel) || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		base := filepath.Base(rel)
		if strings.HasPrefix(rel, "reports"+string(filepath.Separator)) {
			if nebula {
				if laneID, phase, round, ok := reportLanePhase(base, lanes, phases); ok {
					info, infoErr := entry.Info()
					if infoErr != nil {
						return infoErr
					}
					key := laneID + "\x00" + phase
					candidate := struct {
						rel   string
						round int
						mtime int64
					}{rel: rel, round: round, mtime: info.ModTime().UnixNano()}
					best, exists := reportBest[key]
					if !exists || candidate.round > best.round || (candidate.round == best.round && candidate.mtime > best.mtime) {
						reportBest[key] = candidate
					}
				}
			}
			return nil
		}
		for laneID := range lanes {
			for phase := range phases {
				prefix := laneID + "." + phase
				if base == prefix+".spawn.json" || strings.HasPrefix(base, prefix+".nebula-attn.handled-") || strings.HasPrefix(base, prefix+".nebula-attn.answered-") {
					allow[rel] = true
					return nil
				}
				if base == laneID+"."+phase+".md" {
					allow[rel] = true
					return nil
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, candidate := range reportBest {
		allow[candidate.rel] = true
	}
	return allow, nil
}

func reportLanePhase(base string, lanes, phases map[string]bool) (string, string, int, bool) {
	if !strings.HasSuffix(base, ".md") || !strings.Contains(base, ".r") {
		return "", "", 0, false
	}
	trimmed := strings.TrimSuffix(base, ".md")
	for laneID := range lanes {
		prefix := laneID + "."
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		rest := strings.TrimPrefix(trimmed, prefix)
		for phase := range phases {
			if strings.HasPrefix(rest, phase+".r") {
				rawRound := strings.TrimPrefix(rest, phase+".r")
				rawRound = strings.TrimSuffix(rawRound, ".md")
				if strings.Contains(rawRound, ".prev-") {
					return "", "", 0, false
				}
				round, err := strconv.Atoi(rawRound)
				if err == nil {
					return laneID, phase, round, true
				}
			}
		}
	}
	return "", "", 0, false
}

func copySanitizedFiles(input, output string, allow map[string]bool) error {
	for rel := range allow {
		src := filepath.Join(input, rel)
		info, err := os.Stat(src)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sanitize: stat %s: %w", rel, err)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("sanitize: read %s: %w", rel, err)
		}
		cleaned := sanitizeFile(filepath.Base(src), rel, data)
		dst := filepath.Join(output, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, cleaned, 0o644); err != nil {
			return fmt.Errorf("sanitize: write %s: %w", rel, err)
		}
	}
	return nil
}

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
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = filepath.Base(path)
			}
			return fmt.Errorf("sanitize: kept file %s still matches %s pattern", filepath.ToSlash(rel), secretPatternClass(data))
		}
		return nil
	})
}

func secretPatternClass(data []byte) string {
	text := string(data)
	switch {
	case regexp.MustCompile(`(?i)ghp_[A-Za-z0-9_-]+`).MatchString(text):
		return "github-token"
	case regexp.MustCompile(`(?i)sk-[A-Za-z0-9_-]+`).MatchString(text):
		return "api-token"
	case regexp.MustCompile(`(?i)AIza[A-Za-z0-9_-]+`).MatchString(text):
		return "google-token"
	case regexp.MustCompile(`(?i)xox[baprs]-[A-Za-z0-9_-]+`).MatchString(text):
		return "slack-token"
	case regexp.MustCompile(`(?i)BEGIN [^-\n]*PRIVATE KEY`).MatchString(text):
		return "private-key"
	default:
		return "secret"
	}
}
