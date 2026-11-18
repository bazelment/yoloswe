package adopt

// This file contains the deliberately small, loss-aware corpus sanitizer. It
// keeps the host run's decision surface while replacing prose and dropping
// files that can contain credentials. The sanitizer never copies a checkout.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// Token patterns deliberately require a non-word boundary and realistic
	// lengths. Short identifiers such as task-1, disk-full and xoxp-test are
	// ordinary run vocabulary, not credentials.
	sanitizeSecretRE      = regexp.MustCompile(`(?:^|[^[:word:]])(?:(?:ghp_|gho_|github_pat_)[A-Za-z0-9_-]{20,}|sk-(?:proj-|ant-)?[A-Za-z0-9_-]{20,}|AIza[A-Za-z0-9_-]{35,}|xox[baprs]-[A-Za-z0-9-]{10,}|BEGIN [^-\n]*PRIVATE KEY)`)
	sanitizeGithubTokenRE = regexp.MustCompile(`(?:^|\W)(?:ghp_|gho_|github_pat_)[A-Za-z0-9_-]{20,}`)
	sanitizeAPITokenRE    = regexp.MustCompile(`(?:^|\W)sk-(?:proj-|ant-)?[A-Za-z0-9_-]{20,}`)
	sanitizeGoogleTokenRE = regexp.MustCompile(`(?:^|\W)AIza[A-Za-z0-9_-]{35,}`)
	sanitizeSlackTokenRE  = regexp.MustCompile(`(?:^|\W)xox[baprs]-[A-Za-z0-9-]{10,}`)
	sanitizePrivateKeyRE  = regexp.MustCompile(`(?:^|\W)BEGIN [^-\n]*PRIVATE KEY`)
	sanitizeSHARE         = regexp.MustCompile(`(?i)\b[0-9a-f]{7,64}\b`)
	sanitizePRRE          = regexp.MustCompile(`#\d+`)
)

// sanitizeVocabulary is derived from the values the importer actually
// observes. It prevents the sanitizer and importer from growing separate
// hand-maintained lists of decision words.
type sanitizeVocabulary struct {
	exact  map[string]string
	tokens map[string]string
}

func newSanitizeVocabulary() *sanitizeVocabulary {
	return &sanitizeVocabulary{exact: map[string]string{}, tokens: map[string]string{}}
}

func (v *sanitizeVocabulary) add(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	v.exact[strings.ToLower(value)] = value
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return !(r == '#' || r == '/' || r == '_' || r == '-' || r == '.' || r == ':' || r == '+' || r == '&' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		if token != "" {
			v.tokens[strings.ToLower(token)] = token
		}
	}
}

func (v *sanitizeVocabulary) hasExact(value string) (string, bool) {
	got, ok := v.exact[strings.ToLower(strings.TrimSpace(value))]
	return got, ok
}

// SanitizeDir writes a credential-free, shape-preserving run copy. The output
// may be passed directly to DryRunDir. Existing output is rejected to avoid
// accidentally mixing two corpus snapshots.
func SanitizeDir(input, output string) error {
	input = filepath.Clean(strings.TrimSpace(input))
	output = filepath.Clean(strings.TrimSpace(output))
	if input == "" || output == "" || input == "." || output == "." {
		return errors.New("sanitize: input and output directories are required")
	}
	var pathErr error
	input, output, pathErr = sanitizeDirectories(input, output)
	if pathErr != nil {
		return pathErr
	}
	for _, rel := range []string{"state.json", "EVENTS"} {
		if _, err := sanitizeSourcePath(input, rel); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(input, "state.json")); err != nil {
		return fmt.Errorf("sanitize: input is not a run directory: %w", err)
	}
	// EVENTS is part of the kept decision surface. A credential in any of its
	// columns must refuse before prose replacement can hide it.
	if data, readErr := os.ReadFile(filepath.Join(input, "EVENTS")); readErr == nil {
		if err := rejectEventSourceSecrets(data); err != nil {
			return err
		}
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
	vocab, err := sanitizeVocabularyFor(input, allow)
	if err != nil {
		return fmt.Errorf("sanitize: build vocabulary: %w", err)
	}
	if err := copySanitizedFiles(input, tmp, allow, vocab); err != nil {
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

// VerifySanitized previews both directories and checks the invariant promised
// by the corpus format: sanitization may change prose, but never a lane's
// move or reason class.
func VerifySanitized(ctx context.Context, input, output string) (err error) {
	if strings.TrimSpace(input) == "" || strings.TrimSpace(output) == "" {
		return errors.New("sanitize verify: input and output directories are required")
	}
	input, output, err = sanitizeDirectories(input, output)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// A drifting corpus is never a usable artifact. Remove it just as a
			// refusal before publishing leaves no output behind.
			_ = os.RemoveAll(output)
		}
	}()
	service := &Service{}
	want, err := service.DryRunDir(ctx, input)
	if err != nil {
		return fmt.Errorf("sanitize verify: input preview: %w", err)
	}
	got, err := service.DryRunDir(ctx, output)
	if err != nil {
		return fmt.Errorf("sanitize verify: output preview: %w", err)
	}
	wantByID := make(map[string]*DryRunLane, len(want.Lanes))
	gotByID := make(map[string]*DryRunLane, len(got.Lanes))
	for i := range want.Lanes {
		wantByID[want.Lanes[i].ID] = &want.Lanes[i]
	}
	for i := range got.Lanes {
		gotByID[got.Lanes[i].ID] = &got.Lanes[i]
	}
	var diffs []string
	for id, before := range wantByID {
		after, ok := gotByID[id]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("lane %s: missing from sanitized output", id))
			continue
		}
		if before.Move != after.Move || before.ReasonClass != after.ReasonClass {
			diffs = append(diffs, fmt.Sprintf("lane %s: move %s -> %s; reason_class %s -> %s", id, before.Move, after.Move, before.ReasonClass, after.ReasonClass))
		}
	}
	for id := range gotByID {
		if _, ok := wantByID[id]; !ok {
			diffs = append(diffs, fmt.Sprintf("lane %s: added in sanitized output", id))
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		return fmt.Errorf("sanitize verify: decision differences:\n%s", strings.Join(diffs, "\n"))
	}
	return nil
}

// sanitizeDirectories resolves aliases before any output removal. A corpus
// copy must be disjoint from its source, including through symlinked parents.
func sanitizeDirectories(input, output string) (string, string, error) {
	resolve := func(path string) (string, error) {
		abs, err := filepath.Abs(strings.TrimSpace(path))
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if errors.Is(err, os.ErrNotExist) {
			parent, parentErr := filepath.EvalSymlinks(filepath.Dir(abs))
			if parentErr != nil {
				return "", parentErr
			}
			return filepath.Join(parent, filepath.Base(abs)), nil
		}
		return resolved, err
	}
	in, err := resolve(input)
	if err != nil {
		return "", "", fmt.Errorf("sanitize: resolve input: %w", err)
	}
	out, err := resolve(output)
	if err != nil {
		return "", "", fmt.Errorf("sanitize: resolve output: %w", err)
	}
	for _, pair := range [][2]string{{in, out}, {out, in}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return "", "", err
		}
		if rel == "." || filepath.IsLocal(rel) {
			return "", "", errors.New("sanitize: input and output must be disjoint directories")
		}
	}
	// Keep the output spelling for removal: RemoveAll on a final symlink
	// removes the link, not the directory to which it points.
	outPath, err := filepath.Abs(strings.TrimSpace(output))
	if err != nil {
		return "", "", err
	}
	return in, outPath, nil
}

// sanitizeSourcePath excludes ledger traversal and symlinked files/directories
// before the importer or copy step can read outside the approved run.
func sanitizeSourcePath(root, rel string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", errors.New("sanitize: file path escapes run directory")
	}
	path := root
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("sanitize: stat %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("sanitize: symlink in kept path %s", rel)
		}
	}
	return path, nil
}

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
	for _, p := range state.Config.Phases {
		if p.Name != "" {
			if err := validatePathComponent("phase", p.Name); err != nil {
				return nil, err
			}
			phases[p.Name] = true
		}
	}
	for _, task := range state.Tasks {
		if task.ID == "" {
			continue
		}
		if err := validatePathComponent("lane id", task.ID); err != nil {
			return nil, err
		}
		lanes[task.ID] = true
		nebula = nebula || task.SessionID != "" || len(task.Terminals) > 0
		if task.Phase != "" {
			if err := validatePathComponent("phase", task.Phase); err != nil {
				return nil, err
			}
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
				for laneID := range lanes {
					for phase := range phases {
						for _, candidatePath := range reportCandidates(input, laneID, phase, 1) {
							candidateRel, relErr := filepath.Rel(input, candidatePath)
							if relErr == nil && filepath.Clean(candidateRel) == filepath.Clean(rel) {
								allow[rel] = true
							}
						}
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
	// Include every report path the importer can select, including legacy
	// reports/<lane>.<phase>.md paths and nested report locations.
	for laneID := range lanes {
		for phase := range phases {
			for _, candidatePath := range reportCandidates(input, laneID, phase, 1) {
				if rel, relErr := filepath.Rel(input, candidatePath); relErr == nil {
					if info, statErr := os.Stat(candidatePath); statErr == nil && info.Mode().IsRegular() {
						allow[rel] = true
					}
				}
			}
		}
	}
	for rel := range allow {
		if _, err := sanitizeSourcePath(input, rel); err != nil {
			return nil, err
		}
	}
	return allow, nil
}

func rejectEventSourceSecrets(data []byte) error {
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, "\t", 4)
		for i, part := range parts {
			if !sanitizeSecretRE.MatchString(part) {
				continue
			}
			column := "detail"
			switch i {
			case 0:
				column = "timestamp"
			case 1:
				column = "lane-id"
			case 2:
				column = "event-kind"
			}
			return fmt.Errorf("sanitize: kept file EVENTS column %s still matches %s pattern", column, secretPatternClass([]byte(part)))
		}
	}
	return nil
}

func sanitizeVocabularyFor(input string, allow map[string]bool) (*sanitizeVocabulary, error) {
	v := newSanitizeVocabulary()
	run, err := Read(input)
	if err != nil {
		return nil, err
	}
	for _, value := range []string{run.ID, run.Format, run.TargetBranch, run.BaseBranch, run.MergeGate, run.IntegrateRule} {
		v.add(value)
	}
	for _, phase := range run.Phases {
		v.add(phase)
	}
	for _, branch := range run.HistoricalBranches {
		v.add(branch)
	}
	for i := range run.Lanes {
		ln := &run.Lanes[i]
		for _, value := range []string{ln.ID, ln.Phase, ln.Status, ln.Branch, ln.Session, ln.Terminal, ln.TerminalState, ln.PR, ln.PRState, ln.MergeSHA, ln.Head, ln.Checks, ln.WaitingOn} {
			v.add(value)
		}
		v.add(ln.Next)
		for _, value := range []string{ln.Report, ln.LastEvent} {
			for _, token := range strings.Fields(value) {
				if sanitizePRRE.MatchString(token) || sanitizeSHARE.MatchString(token) {
					v.add(token)
				}
			}
		}
		v.add(ln.holdAdvanceText)
	}
	// EVENTS kinds and their first detail words are precisely the fields the
	// importer parses for decisions. Preserve them dynamically.
	if data, readErr := os.ReadFile(filepath.Join(input, "EVENTS")); readErr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(line, "\t", 4)
			if len(parts) >= 3 {
				v.add(parts[2])
			}
			if len(parts) == 4 {
				if fields := strings.Fields(parts[3]); len(fields) > 0 {
					v.add(fields[0])
				}
				detail := parts[3]
				// Preserve words matched by the importer's own event parsers.
				for _, parser := range integrationVocabularyREs {
					for _, match := range parser.FindAllString(detail, -1) {
						v.add(match)
					}
				}
			}
		}
	}
	// Report metadata is parsed by lane.ParsePhaseReport and
	// applyReportMetadata. Keep its values and all numeric/sha references.
	for rel := range allow {
		if !strings.HasSuffix(strings.ToLower(rel), ".md") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(input, rel))
		if readErr != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "outcome", "verdict", "head", "sha", "commit", "checks", "ci", "pr state", "merged", "integrated":
				v.add(strings.TrimSpace(value))
			}
		}
	}
	return v, nil
}

func copySanitizedFiles(input, output string, allow map[string]bool, vocab *sanitizeVocabulary) error {
	for rel := range allow {
		src, err := sanitizeSourcePath(input, rel)
		if err != nil {
			return err
		}
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
		cleaned := sanitizeFile(filepath.Base(src), rel, data, vocab)
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

func sanitizeFile(base, rel string, data []byte, vocab *sanitizeVocabulary) []byte {
	if base == "state.json" {
		if out, err := sanitizeStateJSON(data, vocab); err == nil {
			return out
		}
	}
	if strings.HasSuffix(strings.ToLower(base), ".json") && (strings.Contains(base, "spawn") || strings.Contains(base, "attn")) {
		if out, err := sanitizeSpawnJSON(data, vocab); err == nil {
			return out
		}
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return []byte("synthetic binary omitted\n")
	}
	if base == "EVENTS" {
		return []byte(sanitizeEvents(string(data), vocab))
	}
	if strings.HasPrefix(base, ".hold-advance") || strings.Contains(base, "attn") || strings.Contains(base, "handled") || strings.HasSuffix(base, ".md") || base == "env.sh" || filepath.Ext(rel) == "" {
		return []byte(sanitizeText(string(data), vocab))
	}
	return []byte(sanitizeText(string(data), vocab))
}

func sanitizeStateJSON(data []byte, vocab *sanitizeVocabulary) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	value = sanitizeJSONValue("", value, false, vocab)
	return json.MarshalIndent(value, "", "  ")
}

func sanitizeSpawnJSON(data []byte, vocab *sanitizeVocabulary) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	value = sanitizeJSONValue("spawn", value, true, vocab)
	return json.MarshalIndent(value, "", "  ")
}

func sanitizeJSONValue(key string, value any, spawn bool, vocab *sanitizeVocabulary) any {
	switch x := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = sanitizeJSONValue(k, v, spawn || strings.EqualFold(k, "spawn"), vocab)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = sanitizeJSONValue(key, v, spawn, vocab)
		}
		return out
	case string:
		if vocab != nil {
			if exact, ok := vocab.hasExact(x); ok {
				return exact
			}
		}
		if preserveJSONString(key, spawn) {
			return sanitizeJSONString(key, x, spawn, vocab)
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

func sanitizeJSONString(key, value string, spawn bool, vocab *sanitizeVocabulary) string {
	if exact, ok := vocab.hasExact(value); ok {
		return exact
	}
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "id", "branch", "target", "base", "waiting_on", "deadline":
		return sanitizeRef(value)
	case "merge_gate", "merge_rule":
		if sanitizeGateRE.MatchString(strings.TrimSpace(value)) && !sanitizeSecretRE.MatchString(value) {
			return strings.TrimSpace(value)
		}
		return sanitizeDecisionTokens(value, vocab)
	case "session_id", "terminal_id":
		if spawn {
			return sanitizeRef(value)
		}
	}
	return sanitizeDecisionTokens(value, vocab)
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

func sanitizeDecisionTokens(s string, vocab *sanitizeVocabulary) string {
	if vocab != nil {
		if exact, ok := vocab.hasExact(s); ok {
			return exact
		}
	}
	var tokens []string
	for _, field := range strings.Fields(s) {
		trimmed := strings.Trim(field, "()[]{}<>,;\"'`:")
		if matches := sanitizePRRE.FindAllString(trimmed, -1); len(matches) > 0 {
			tokens = append(tokens, matches...)
			continue
		}
		if matches := sanitizeSHARE.FindAllString(trimmed, -1); len(matches) > 0 {
			tokens = append(tokens, matches...)
			continue
		}
		if vocab != nil {
			if token, ok := vocab.tokens[strings.ToLower(trimmed)]; ok {
				tokens = append(tokens, token)
			}
		}
	}
	if len(tokens) == 0 {
		return neutralText(s)
	}
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

func sanitizeText(s string, vocab *sanitizeVocabulary) string {
	var out []string
	for _, line := range strings.SplitAfter(s, "\n") {
		trim := strings.TrimSuffix(line, "\n")
		if vocab != nil {
			if exact, ok := vocab.hasExact(strings.TrimSpace(trim)); ok {
				out = append(out, exact+map[bool]string{true: "\n", false: ""}[strings.HasSuffix(line, "\n")])
				continue
			}
		}
		prefix := ""
		value := trim
		if key, rest, ok := strings.Cut(trim, ":"); ok {
			prefix, value = key+":", rest
		}
		clean := sanitizeDecisionTokens(value, vocab)
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

func sanitizeEvents(s string, vocab *sanitizeVocabulary) string {
	var out []string
	for _, line := range strings.SplitAfter(s, "\n") {
		ending := ""
		if strings.HasSuffix(line, "\n") {
			ending, line = "\n", strings.TrimSuffix(line, "\n")
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) >= 3 {
			if len(parts) == 4 {
				parts[3] = sanitizeDecisionTokens(parts[3], vocab)
			}
			line = strings.Join(parts, "\t")
		} else {
			line = sanitizeText(line, vocab)
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
			return fmt.Errorf("sanitize: kept file %s column %s still matches %s pattern", filepath.ToSlash(rel), sanitizeSecretColumn(filepath.ToSlash(rel), data), secretPatternClass(data))
		}
		return nil
	})
}

func sanitizeSecretColumn(rel string, data []byte) string {
	if rel == "EVENTS" {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(line, "\t", 4)
			for i, part := range parts {
				if !sanitizeSecretRE.MatchString(part) {
					continue
				}
				switch i {
				case 0:
					return "timestamp"
				case 1:
					return "lane-id"
				case 2:
					return "event-kind"
				default:
					return "detail"
				}
			}
		}
	}
	if strings.HasSuffix(strings.ToLower(rel), ".json") {
		return "structured-value"
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && sanitizeSecretRE.MatchString(value) {
			key = strings.ToLower(strings.TrimSpace(key))
			if key != "" {
				return key
			}
		}
	}
	return "report-text"
}

func secretPatternClass(data []byte) string {
	text := string(data)
	switch {
	case sanitizeGithubTokenRE.MatchString(text):
		return "github-token"
	case sanitizeAPITokenRE.MatchString(text):
		return "api-token"
	case sanitizeGoogleTokenRE.MatchString(text):
		return "google-token"
	case sanitizeSlackTokenRE.MatchString(text):
		return "slack-token"
	case sanitizePrivateKeyRE.MatchString(text):
		return "private-key"
	default:
		return "secret"
	}
}
