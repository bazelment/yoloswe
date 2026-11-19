package model

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Canonical read-only investigation tool names for CLI agent sessions.
const (
	ToolReadFile = "read.file"
	ToolReadCmd  = "read.cmd"

	defaultReadFileMaxBytes = 16 * 1024
	ceilingReadFileMaxBytes = 64 * 1024
	defaultReadCmdTimeout   = 15 * time.Second
)

// InvestigateConfig configures the read-only investigation tools attached to an
// agent session's ToolRegistry.
type InvestigateConfig struct {
	CommandRunner func(ctx context.Context, dir, program string, args []string) ([]byte, error)
	RunDir        string
	RepoDir       string
	ExtraRoots    []string
}

// HasRoots reports whether any run/repo directory or command runner is configured.
func (c InvestigateConfig) HasRoots() bool {
	return strings.TrimSpace(c.RunDir) != "" ||
		strings.TrimSpace(c.RepoDir) != "" ||
		len(c.ExtraRoots) > 0 ||
		c.CommandRunner != nil
}

func (c InvestigateConfig) allowedRoots() []string {
	var roots []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		p = filepath.Clean(p)
		if !slices.Contains(roots, p) {
			roots = append(roots, p)
		}
	}
	add(c.RunDir)
	add(c.RepoDir)
	for _, r := range c.ExtraRoots {
		add(r)
	}
	return roots
}

// ReadFileParams holds parameters for the read.file investigation tool.
type ReadFileParams struct {
	Path     string `json:"path" jsonschema:"required,description=Relative or absolute path within the run directory or repository checkout (e.g. OBJECTIVE.md, PROGRESS.md, state.json)"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes to read (default 16384)"`
}

// ReadCmdParams holds parameters for the read.cmd investigation tool.
type ReadCmdParams struct {
	Program string   `json:"program" jsonschema:"required,enum=gh,enum=nebula,enum=git,description=CLI program to run (gh, nebula, or git)"`
	Args    []string `json:"args" jsonschema:"required,description=Read-only command arguments (e.g. [\"pr\",\"view\",\"123\"], [\"post\",\"get\",\"<id>\"], or [\"status\",\"--short\"])"`
}

// RegisterInvestigationTools adds read.file and read.cmd to reg when cfg.HasRoots() is true.
func RegisterInvestigationTools(reg *ToolRegistry, cfg InvestigateConfig) {
	if reg == nil || !cfg.HasRoots() {
		return
	}
	AddTool(reg, ToolReadFile, "Read a file or list a directory in the held run directory (e.g. OBJECTIVE.md, PROGRESS.md, ESCALATIONS.md, state.json, reports/) or repository checkout", func(_ context.Context, p ReadFileParams) (string, error) {
		return executeReadFile(cfg, p)
	})
	AddTool(reg, ToolReadCmd, "Run a read-only gh, nebula, or git inspection command (e.g. gh pr view/list/checks, nebula post get, nebula terminal logs, nebula mail list, git status/log/diff/show)", func(ctx context.Context, p ReadCmdParams) (string, error) {
		return executeReadCmd(ctx, cfg, p)
	})
}

// ExecuteReadFile reads a file or lists a directory within cfg's allowed roots.
func ExecuteReadFile(cfg InvestigateConfig, p ReadFileParams) (string, error) {
	return executeReadFile(cfg, p)
}

// ExecuteReadCmd runs a validated read-only command within cfg's allowed roots.
func ExecuteReadCmd(ctx context.Context, cfg InvestigateConfig, p ReadCmdParams) (string, error) {
	return executeReadCmd(ctx, cfg, p)
}

func executeReadFile(cfg InvestigateConfig, p ReadFileParams) (string, error) {
	rawPath := strings.TrimSpace(p.Path)
	if rawPath == "" {
		return "", errors.New("read.file: path is required")
	}
	roots := cfg.allowedRoots()
	if len(roots) == 0 {
		return "", errors.New("read.file: no run or repository directory configured")
	}

	resolved, err := resolveSafePath(roots, rawPath)
	if err != nil {
		return "", err
	}

	limit := p.MaxBytes
	if limit <= 0 {
		limit = defaultReadFileMaxBytes
	}
	if limit > ceilingReadFileMaxBytes {
		limit = ceilingReadFileMaxBytes
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("read.file %q: %w", rawPath, err)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(resolved)
		if err != nil {
			return "", fmt.Errorf("read.file dir %q: %w", rawPath, err)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Directory %s (%d entries):\n", rawPath, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				fmt.Fprintf(&b, "- %s/\n", e.Name())
			} else if fi, sErr := e.Info(); sErr == nil {
				fmt.Fprintf(&b, "- %s (%d bytes)\n", e.Name(), fi.Size())
			} else {
				fmt.Fprintf(&b, "- %s\n", e.Name())
			}
			if b.Len() > limit+1 {
				break
			}
		}
		dirOut := strings.TrimSpace(b.String())
		if len(dirOut) > limit {
			return dirOut[:limit] + "\n...[truncated]", nil
		}
		return dirOut, nil
	}

	f, err := os.Open(resolved)
	if err != nil {
		return "", fmt.Errorf("read.file %q: %w", rawPath, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", fmt.Errorf("read.file %q: %w", rawPath, err)
	}
	if len(data) > limit {
		return string(data[:limit]) + "\n...[truncated]", nil
	}
	return string(data), nil
}

func resolveSafePath(roots []string, rawPath string) (string, error) {
	if filepath.IsAbs(rawPath) {
		clean := filepath.Clean(rawPath)
		if !isWithinAnyRoot(roots, clean) {
			return "", fmt.Errorf("read.file: path %q is outside allowed run/repo directories", rawPath)
		}
		if err := verifySymlinkConfinement(roots, clean, rawPath); err != nil {
			return "", err
		}
		if isSensitiveInvestigationPath(clean) {
			return "", fmt.Errorf("read.file: path %q is restricted (sensitive or database file)", rawPath)
		}
		return clean, nil
	}

	var firstCandidate string
	for _, root := range roots {
		candidates := []string{filepath.Clean(filepath.Join(root, rawPath))}
		if prefix, rest, ok := strings.Cut(filepath.ToSlash(rawPath), "/"); ok && rest != "" {
			base := filepath.Base(root)
			if prefix == base || (isDatedRunDirBase(base) && strings.HasSuffix(base, "-"+prefix)) {
				candidates = append(candidates, filepath.Clean(filepath.Join(root, filepath.FromSlash(rest))))
			}
		}
		for _, cand := range candidates {
			if !isWithinRoot(root, cand) {
				continue
			}
			if firstCandidate == "" {
				firstCandidate = cand
			}
			if _, err := os.Lstat(cand); err == nil {
				if err := verifySymlinkConfinement(roots, cand, rawPath); err != nil {
					return "", err
				}
				if isSensitiveInvestigationPath(cand) {
					return "", fmt.Errorf("read.file: path %q is restricted (sensitive or database file)", rawPath)
				}
				return cand, nil
			}
		}
	}
	if firstCandidate == "" {
		return "", fmt.Errorf("read.file: path %q escapes allowed run/repo directories", rawPath)
	}
	if err := verifySymlinkConfinement(roots, firstCandidate, rawPath); err != nil {
		return "", err
	}
	if isSensitiveInvestigationPath(firstCandidate) {
		return "", fmt.Errorf("read.file: path %q is restricted (sensitive or database file)", rawPath)
	}
	return firstCandidate, nil
}

func isDatedRunDirBase(base string) bool {
	if len(base) < 10 || base[8] != '-' {
		return false
	}
	for i := range 8 {
		if base[i] < '0' || base[i] > '9' {
			return false
		}
	}
	return true
}

func isSensitiveInvestigationPath(target string) bool {
	if realTarget, err := filepath.EvalSymlinks(target); err == nil {
		if hasSensitivePathComponent(realTarget) {
			return true
		}
	}
	return hasSensitivePathComponent(target)
}

func hasSensitivePathComponent(p string) bool {
	clean := filepath.Clean(p)
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		lower := strings.ToLower(strings.TrimSpace(seg))
		if lower == ".git" || lower == ".env" || strings.HasPrefix(lower, ".env.") {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(clean))
	for _, suffix := range []string{".db", ".sqlite", ".sqlite3", ".db-wal", ".db-shm", ".sqlite-wal", ".sqlite-shm"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

func verifySymlinkConfinement(roots []string, candidate, rawPath string) error {
	cur := filepath.Clean(candidate)
	for {
		if _, err := os.Lstat(cur); err == nil {
			realCur, lErr := filepath.EvalSymlinks(cur)
			if lErr != nil {
				return fmt.Errorf("read.file: symlink %q resolves outside allowed directories: %w", rawPath, lErr)
			}
			if !isWithinAnyRoot(roots, realCur) {
				return fmt.Errorf("read.file: symlink %q resolves outside allowed directories", rawPath)
			}
			return nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}

func isWithinAnyRoot(roots []string, target string) bool {
	for _, root := range roots {
		if isWithinRoot(root, target) {
			return true
		}
		if realRoot, err := filepath.EvalSymlinks(root); err == nil && isWithinRoot(realRoot, target) {
			return true
		}
	}
	return false
}

func isWithinRoot(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ResolveGitTopLevel returns the git repository top-level directory containing
// dir, or "" when dir is not inside a git working tree.
func ResolveGitTopLevel(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil || strings.TrimSpace(dir) == "" {
			return ""
		}
	}
	cur := filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// ResolveRunRepoDir resolves the git repository checkout for a run directory when
// runDir is inside a git worktree, falling back to ResolveGitTopLevel(fallbackDir).
func ResolveRunRepoDir(runDir, fallbackDir string) string {
	if trimmed := strings.TrimSpace(runDir); trimmed != "" {
		if top := ResolveGitTopLevel(trimmed); top != "" {
			return top
		}
	}
	return ResolveGitTopLevel(fallbackDir)
}

// ValidateReadOnlyCommand checks that program and args form a permitted
// read-only gh, nebula, git, or local file inspection command.
func ValidateReadOnlyCommand(program string, args []string) error {
	_, _, _, err := normalizeReadOnlyCommand(program, args)
	return err
}

func normalizeReadOnlyCommand(program string, args []string) (string, []string, []string, error) {
	prog := strings.TrimSpace(program)
	var cleanArgs []string
	for _, a := range args {
		if trimmed := strings.TrimSpace(a); trimmed != "" {
			cleanArgs = append(cleanArgs, trimmed)
		}
	}
	if len(cleanArgs) == 0 {
		if prog == "ls" {
			return prog, []string{"."}, []string{"."}, nil
		}
		return "", nil, nil, errors.New("read.cmd: args are required")
	}

	switch prog {
	case "gh":
		for _, arg := range cleanArgs {
			if arg == "-w" || arg == "--web" {
				return "", nil, nil, fmt.Errorf("read.cmd: interactive flag %q is not permitted", arg)
			}
		}
		sub := cleanArgs[0]
		switch sub {
		case "pr":
			if len(cleanArgs) < 2 {
				return "", nil, nil, errors.New("read.cmd: gh pr requires a read-only subcommand (view, list, checks, status, diff)")
			}
			switch cleanArgs[1] {
			case "view", "list", "checks", "status", "diff":
				return prog, cleanArgs, nil, nil
			default:
				return "", nil, nil, fmt.Errorf("read.cmd: gh pr %q is not a permitted read-only subcommand", cleanArgs[1])
			}
		case "run", "issue":
			if len(cleanArgs) < 2 {
				return "", nil, nil, fmt.Errorf("read.cmd: gh %s requires a read-only subcommand (view, list, status)", sub)
			}
			switch cleanArgs[1] {
			case "view", "list", "status":
				return prog, cleanArgs, nil, nil
			default:
				return "", nil, nil, fmt.Errorf("read.cmd: gh %s %q is not a permitted read-only subcommand", sub, cleanArgs[1])
			}
		case "repo":
			if len(cleanArgs) < 2 || cleanArgs[1] != "view" {
				return "", nil, nil, errors.New("read.cmd: only 'gh repo view' is permitted")
			}
			return prog, cleanArgs, nil, nil
		default:
			return "", nil, nil, fmt.Errorf("read.cmd: gh %q is not a permitted read-only command", sub)
		}

	case "nebula":
		if len(cleanArgs) < 2 {
			return "", nil, nil, errors.New("read.cmd: nebula requires a resource and read-only action (e.g. post get, mail list, terminal logs)")
		}
		res, action := cleanArgs[0], cleanArgs[1]
		switch res {
		case "post":
			if action == "get" || action == "list" {
				return prog, cleanArgs, nil, nil
			}
		case "mail":
			if action == "list" || action == "get" || action == "read" || action == "status" {
				return prog, cleanArgs, nil, nil
			}
		case "terminal":
			if action == "get" || action == "list" || action == "logs" || action == "status" || action == "read" {
				return prog, cleanArgs, nil, nil
			}
		case "session", "run":
			if action == "get" || action == "list" || action == "status" {
				return prog, cleanArgs, nil, nil
			}
		}
		return "", nil, nil, fmt.Errorf("read.cmd: nebula %s %s is not a permitted read-only command", res, action)

	case "git":
		outProg, outArgs, err := normalizeGitReadOnlyCommand(prog, cleanArgs)
		return outProg, outArgs, nil, err

	case "sha256sum", "shasum", "wc", "ls", "cat", "head", "tail", "stat":
		return normalizeFileUtilCommand(prog, cleanArgs)

	case "rg", "grep":
		return normalizeSearchCommand(prog, cleanArgs)

	case "jq":
		return normalizeJQCommand(prog, cleanArgs)

	default:
		return "", nil, nil, fmt.Errorf("read.cmd: program %q is not permitted (only gh, nebula, git, jq, rg, grep, sha256sum, shasum, wc, ls, cat, head, tail, and stat are allowed)", prog)
	}
}

func validateRelativePathArg(prog, arg string) error {
	if filepath.IsAbs(arg) || strings.HasPrefix(arg, "~") || containsParentTraversal(arg) {
		return fmt.Errorf("read.cmd: %s path argument %q escapes allowed directories", prog, arg)
	}
	if isSensitiveInvestigationPath(arg) {
		return fmt.Errorf("read.cmd: %s path argument %q is restricted", prog, arg)
	}
	return nil
}

func normalizeFileUtilCommand(prog string, cleanArgs []string) (string, []string, []string, error) {
	var outArgs, pathArgs []string
	afterDoubleDash := false
	expectNumericValue := false
	for _, arg := range cleanArgs {
		if expectNumericValue {
			if !isDigitsOnly(arg) {
				return "", nil, nil, fmt.Errorf("read.cmd: %s numeric flag value %q must be a positive integer", prog, arg)
			}
			outArgs = append(outArgs, arg)
			expectNumericValue = false
			continue
		}
		if !afterDoubleDash && arg == "--" {
			afterDoubleDash = true
			outArgs = append(outArgs, arg)
			continue
		}
		if !afterDoubleDash && strings.HasPrefix(arg, "-") {
			takesNum, ok := isAllowedFileUtilFlag(prog, arg)
			if !ok {
				return "", nil, nil, fmt.Errorf("read.cmd: %s flag %q is not permitted in read-only mode", prog, arg)
			}
			outArgs = append(outArgs, arg)
			expectNumericValue = takesNum
			continue
		}
		if err := validateRelativePathArg(prog, arg); err != nil {
			return "", nil, nil, err
		}
		outArgs = append(outArgs, arg)
		pathArgs = append(pathArgs, arg)
	}
	if expectNumericValue {
		return "", nil, nil, fmt.Errorf("read.cmd: %s flag requires a numeric value", prog)
	}
	if len(pathArgs) == 0 {
		if prog == "ls" {
			outArgs = append(outArgs, ".")
			pathArgs = append(pathArgs, ".")
		} else {
			return "", nil, nil, fmt.Errorf("read.cmd: %s requires at least one file path argument", prog)
		}
	}
	return prog, outArgs, pathArgs, nil
}

func isAllowedFileUtilFlag(prog, flag string) (takesNumericValue bool, allowed bool) {
	switch prog {
	case "sha256sum":
		switch flag {
		case "-b", "--binary", "-t", "--text", "--tag":
			return false, true
		}
	case "shasum":
		switch flag {
		case "-a":
			return true, true
		case "-b", "--binary", "-t", "--text", "-p", "--portable", "-U", "--UNIVERSAL":
			return false, true
		}
		if strings.HasPrefix(flag, "-a") && isDigitsOnly(flag[2:]) {
			return false, true
		}
	case "wc":
		switch flag {
		case "-l", "--lines", "-w", "--words", "-c", "--bytes", "-m", "--chars", "-L", "--max-line-length",
			"-lw", "-wl", "-lc", "-cl", "-wc", "-cw", "-lwc":
			return false, true
		}
	case "ls":
		switch flag {
		case "-l", "-a", "-A", "-h", "-1", "-d", "-F", "-R", "-r", "-S", "-t",
			"-la", "-al", "-lh", "-hl", "-lah", "-alh", "-lA", "-Al", "-lR", "-Rl", "-lt", "-tl", "-ltr", "-lrt",
			"--all", "--almost-all", "--human-readable", "--directory", "--recursive", "--reverse":
			return false, true
		}
	case "cat":
		switch flag {
		case "-n", "--number", "-b", "--number-nonblank", "-s", "--squeeze-blank", "-E", "--show-ends", "-T", "--show-tabs", "-A", "--show-all":
			return false, true
		}
	case "head", "tail":
		switch flag {
		case "-n", "--lines", "-c", "--bytes":
			return true, true
		case "-q", "--quiet", "--silent", "-v", "--verbose":
			return false, true
		}
		if (strings.HasPrefix(flag, "-n") && isDigitsOnly(flag[2:])) ||
			(strings.HasPrefix(flag, "--lines=") && isDigitsOnly(strings.TrimPrefix(flag, "--lines="))) ||
			(strings.HasPrefix(flag, "-c") && isDigitsOnly(flag[2:])) ||
			(strings.HasPrefix(flag, "--bytes=") && isDigitsOnly(strings.TrimPrefix(flag, "--bytes="))) ||
			(len(flag) > 1 && flag[0] == '-' && isDigitsOnly(flag[1:])) {
			return false, true
		}
	case "stat":
		switch flag {
		case "-L", "--dereference", "-t", "--terse":
			return false, true
		}
	}
	return false, false
}

func normalizeSearchCommand(prog string, cleanArgs []string) (string, []string, []string, error) {
	var outArgs, pathArgs []string
	if prog == "rg" {
		if !slices.Contains(cleanArgs, "--no-config") {
			outArgs = append(outArgs, "--no-config")
		}
		if !slices.Contains(cleanArgs, "--no-follow") {
			outArgs = append(outArgs, "--no-follow")
		}
	}
	afterDoubleDash := false
	expectNumericValue := false
	expectPatternValue := false
	hasPattern := false
	for _, arg := range cleanArgs {
		if expectNumericValue {
			if !isDigitsOnly(arg) {
				return "", nil, nil, fmt.Errorf("read.cmd: %s numeric flag value %q must be a positive integer", prog, arg)
			}
			outArgs = append(outArgs, arg)
			expectNumericValue = false
			continue
		}
		if expectPatternValue {
			outArgs = append(outArgs, arg)
			expectPatternValue = false
			hasPattern = true
			continue
		}
		if !afterDoubleDash && arg == "--" {
			afterDoubleDash = true
			outArgs = append(outArgs, arg)
			continue
		}
		if !afterDoubleDash && strings.HasPrefix(arg, "-") {
			takesNum, takesPat, ok := isAllowedSearchFlag(prog, arg)
			if !ok {
				return "", nil, nil, fmt.Errorf("read.cmd: %s flag %q is not permitted in read-only mode", prog, arg)
			}
			outArgs = append(outArgs, arg)
			expectNumericValue = takesNum
			expectPatternValue = takesPat
			if strings.HasPrefix(arg, "--regexp=") {
				hasPattern = true
			}
			continue
		}
		if !hasPattern {
			hasPattern = true
			outArgs = append(outArgs, arg)
			continue
		}
		if err := validateRelativePathArg(prog, arg); err != nil {
			return "", nil, nil, err
		}
		outArgs = append(outArgs, arg)
		pathArgs = append(pathArgs, arg)
	}
	if expectNumericValue || expectPatternValue {
		return "", nil, nil, fmt.Errorf("read.cmd: %s flag is missing its required value", prog)
	}
	if !hasPattern {
		return "", nil, nil, fmt.Errorf("read.cmd: %s requires a search pattern", prog)
	}
	if len(pathArgs) == 0 {
		return "", nil, nil, fmt.Errorf("read.cmd: %s requires at least one file path argument", prog)
	}
	return prog, outArgs, pathArgs, nil
}

func isAllowedSearchFlag(prog, flag string) (takesNumericValue, takesPatternValue, allowed bool) {
	switch flag {
	case "-e", "--regexp":
		return false, true, true
	case "-C", "--context", "-A", "--after-context", "-B", "--before-context", "-m", "--max-count":
		return true, false, true
	case "-n", "--line-number", "-i", "--ignore-case", "-F", "--fixed-strings", "-w", "--word-regexp",
		"-c", "--count", "-l", "--files-with-matches", "-v", "--invert-match", "-H", "--with-filename",
		"-h", "--no-filename",
		"-ni", "-in", "-Fni", "-nF", "-Fn", "--color=never":
		return false, false, true
	}
	if prog == "grep" && (flag == "-E" || flag == "--extended-regexp") {
		return false, false, true
	}
	if strings.HasPrefix(flag, "--regexp=") && len(flag) > len("--regexp=") {
		return false, false, true
	}
	for _, pfx := range []string{"-C", "-A", "-B", "-m", "--context=", "--after-context=", "--before-context=", "--max-count="} {
		if strings.HasPrefix(flag, pfx) && isDigitsOnly(strings.TrimPrefix(flag, pfx)) {
			return false, false, true
		}
	}
	if prog == "rg" {
		switch flag {
		case "-S", "--smart-case", "--no-heading", "--no-config", "--no-follow", "--vimgrep":
			return false, false, true
		}
	}
	return false, false, false
}

func normalizeJQCommand(prog string, cleanArgs []string) (string, []string, []string, error) {
	var outArgs, pathArgs []string
	afterDoubleDash := false
	hasFilter := false
	for _, arg := range cleanArgs {
		if !afterDoubleDash && arg == "--" {
			afterDoubleDash = true
			outArgs = append(outArgs, arg)
			continue
		}
		if !afterDoubleDash && strings.HasPrefix(arg, "-") {
			switch arg {
			case "-r", "--raw-output", "-c", "--compact-output", "-e", "--exit-status",
				"-s", "--slurp", "-S", "--sort-keys", "-M", "--monochrome-output",
				"-j", "--join-output", "-a", "--ascii-output", "-rc", "-cr", "-rs", "-sr", "-cs", "-sc":
				outArgs = append(outArgs, arg)
				continue
			default:
				return "", nil, nil, fmt.Errorf("read.cmd: jq flag %q is not permitted in read-only mode", arg)
			}
		}
		if !hasFilter {
			if err := validateJQFilter(arg); err != nil {
				return "", nil, nil, err
			}
			hasFilter = true
			outArgs = append(outArgs, arg)
			continue
		}
		if err := validateRelativePathArg(prog, arg); err != nil {
			return "", nil, nil, err
		}
		outArgs = append(outArgs, arg)
		pathArgs = append(pathArgs, arg)
	}
	if !hasFilter {
		return "", nil, nil, errors.New("read.cmd: jq requires a filter expression and at least one file path argument")
	}
	if len(pathArgs) == 0 {
		return "", nil, nil, errors.New("read.cmd: jq requires at least one file path argument")
	}
	return prog, outArgs, pathArgs, nil
}

func validateJQFilter(filter string) error {
	if strings.Contains(filter, "$ENV") {
		return errors.New("read.cmd: jq filter may not access $ENV")
	}
	if strings.Contains(filter, `\(`) {
		return errors.New("read.cmd: jq string interpolation is not permitted in read-only mode")
	}
	// Tokenize identifier runs outside string literals to reject builtin env/module/file builtins.
	inSingle, inDouble, escaped := false, false, false
	for i := 0; i < len(filter); i++ {
		ch := filter[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && (inSingle || inDouble) {
			escaped = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if inSingle || inDouble || !isJQIdentStart(ch) {
			continue
		}
		// If preceded by '.' (e.g. .env or .inputs), it is a JSON field access, not a builtin call.
		prevNonSpace := byte(0)
		for k := i - 1; k >= 0; k-- {
			if filter[k] != ' ' && filter[k] != '\t' && filter[k] != '\n' && filter[k] != '\r' {
				prevNonSpace = filter[k]
				break
			}
		}
		j := i + 1
		for j < len(filter) && isJQIdentCont(filter[j]) {
			j++
		}
		ident := filter[i:j]
		i = j - 1
		if prevNonSpace == '.' {
			continue
		}
		switch ident {
		case "env", "include", "import", "input", "inputs", "input_filename", "debug", "stderr", "halt", "halt_error":
			return fmt.Errorf("read.cmd: jq builtin %q is not permitted in read-only mode", ident)
		}
	}
	return nil
}

func isJQIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == '$'
}

func isJQIdentCont(b byte) bool {
	return isJQIdentStart(b) || (b >= '0' && b <= '9')
}

func normalizeGitReadOnlyCommand(prog string, cleanArgs []string) (string, []string, error) {
	sub := cleanArgs[0]
	if strings.HasPrefix(sub, "-") {
		return "", nil, fmt.Errorf("read.cmd: git global flag %q is not permitted before subcommand", sub)
	}
	switch sub {
	case "status", "log", "diff", "show", "rev-parse", "ls-tree":
	case "branch":
		for _, arg := range cleanArgs[1:] {
			switch arg {
			case "-a", "--all", "-r", "--remotes", "-v", "-vv", "--list", "-l", "--show-current":
			default:
				return "", nil, fmt.Errorf("read.cmd: git branch argument %q is not permitted in read-only mode", arg)
			}
		}
		return prog, cleanArgs, nil
	default:
		return "", nil, fmt.Errorf("read.cmd: git %q is not a permitted read-only subcommand (allowed: status, log, diff, show, rev-parse, branch)", sub)
	}

	outArgs := []string{sub}
	if sub == "diff" || sub == "show" || sub == "log" {
		if !slices.Contains(cleanArgs[1:], "--no-ext-diff") {
			outArgs = append(outArgs, "--no-ext-diff")
		}
		if !slices.Contains(cleanArgs[1:], "--no-textconv") {
			outArgs = append(outArgs, "--no-textconv")
		}
	}

	afterDoubleDash := false
	expectCountValue := false
	for _, arg := range cleanArgs[1:] {
		if expectCountValue {
			if !isDigitsOnly(arg) {
				return "", nil, fmt.Errorf("read.cmd: git numeric flag value %q must be a positive integer", arg)
			}
			outArgs = append(outArgs, arg)
			expectCountValue = false
			continue
		}
		if !afterDoubleDash && arg == "--" {
			afterDoubleDash = true
			outArgs = append(outArgs, arg)
			continue
		}
		if !afterDoubleDash && strings.HasPrefix(arg, "-") {
			takesCount, ok := isAllowedGitSubcommandFlag(sub, arg)
			if !ok {
				return "", nil, fmt.Errorf("read.cmd: git %s flag %q is not permitted in read-only mode", sub, arg)
			}
			outArgs = append(outArgs, arg)
			expectCountValue = takesCount
			continue
		}
		if err := validateGitPositionalArg(arg, afterDoubleDash); err != nil {
			return "", nil, err
		}
		outArgs = append(outArgs, arg)
	}
	if expectCountValue {
		return "", nil, fmt.Errorf("read.cmd: git %s flag requires a numeric count argument", sub)
	}
	return prog, outArgs, nil
}

func isAllowedGitSubcommandFlag(sub, flag string) (takesNumericValue bool, allowed bool) {
	switch sub {
	case "status":
		switch flag {
		case "-s", "--short", "-b", "--branch", "--porcelain", "--porcelain=v1", "--porcelain=v2",
			"-u", "--untracked-files", "--untracked-files=no", "--untracked-files=normal", "--untracked-files=all":
			return false, true
		}
	case "log", "diff", "show":
		switch flag {
		case "-n", "--max-count":
			return true, true
		case "--oneline", "--stat", "--numstat", "--shortstat", "--name-only", "--name-status",
			"-p", "-u", "--patch", "--no-patch", "-s", "--decorate", "--no-decorate", "--graph",
			"--abbrev-commit", "--no-ext-diff", "--no-textconv", "--cached", "--staged":
			return false, true
		}
		if strings.HasPrefix(flag, "-n") && isDigitsOnly(flag[2:]) {
			return false, true
		}
		if strings.HasPrefix(flag, "--max-count=") && isDigitsOnly(strings.TrimPrefix(flag, "--max-count=")) {
			return false, true
		}
		if strings.HasPrefix(flag, "-U") && isDigitsOnly(flag[2:]) {
			return false, true
		}
		if strings.HasPrefix(flag, "--unified=") && isDigitsOnly(strings.TrimPrefix(flag, "--unified=")) {
			return false, true
		}
		if len(flag) > 1 && flag[0] == '-' && isDigitsOnly(flag[1:]) {
			return false, true
		}
	case "rev-parse":
		switch flag {
		case "--verify", "--short", "--abbrev-ref", "--symbolic-full-name", "--show-toplevel", "--is-inside-work-tree":
			return false, true
		}
	case "ls-tree":
		switch flag {
		case "-r", "--name-only", "--name-status", "--full-name", "--full-tree", "-d", "-t":
			return false, true
		}
	}
	return false, false
}

func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func containsParentTraversal(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

func validateGitPositionalArg(arg string, isPathspecOnly bool) error {
	trimmed := strings.TrimSpace(arg)
	if trimmed == "" {
		return errors.New("read.cmd: empty git positional argument")
	}
	if !isPathspecOnly && strings.Contains(trimmed, ":") {
		_, pathPart, _ := strings.Cut(trimmed, ":")
		if pathPart == "" || filepath.IsAbs(pathPart) || strings.HasPrefix(pathPart, "~") || containsParentTraversal(pathPart) {
			return fmt.Errorf("read.cmd: git revision path %q is outside allowed directories", arg)
		}
		if isSensitiveInvestigationPath(pathPart) {
			return fmt.Errorf("read.cmd: git revision path %q is restricted (sensitive or database file)", arg)
		}
		return nil
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "~") || containsParentTraversal(trimmed) {
		return fmt.Errorf("read.cmd: git path argument %q escapes allowed run/repo directories", arg)
	}
	if isSensitiveInvestigationPath(trimmed) {
		return fmt.Errorf("read.cmd: git path argument %q is restricted (sensitive or database file)", arg)
	}
	return nil
}

func validateGitRootConfinement(roots []string, baseDir string, args []string) error {
	if len(roots) == 0 || len(args) == 0 {
		return nil
	}
	sub := args[0]
	root := cmp.Or(strings.TrimSpace(baseDir), roots[0])
	afterDoubleDash := false
	expectCountValue := false
	for _, arg := range args[1:] {
		if expectCountValue {
			expectCountValue = false
			continue
		}
		if !afterDoubleDash && arg == "--" {
			afterDoubleDash = true
			continue
		}
		if !afterDoubleDash && strings.HasPrefix(arg, "-") {
			takesCount, _ := isAllowedGitSubcommandFlag(sub, arg)
			expectCountValue = takesCount
			continue
		}
		candPath := arg
		if !afterDoubleDash && strings.Contains(arg, ":") {
			_, candPath, _ = strings.Cut(arg, ":")
		}
		joined := filepath.Clean(filepath.Join(root, candPath))
		if !isWithinAnyRoot(roots, joined) {
			return fmt.Errorf("read.cmd: git path %q resolves outside allowed roots", arg)
		}
		if err := verifySymlinkConfinement(roots, joined, candPath); err != nil {
			return err
		}
		if isSensitiveInvestigationPath(joined) {
			return fmt.Errorf("read.cmd: git path %q is restricted (sensitive or database file)", arg)
		}
	}
	return nil
}

func validateCommandPathConfinement(roots []string, baseDir, prog string, pathArgs []string) error {
	if len(roots) == 0 || len(pathArgs) == 0 {
		return nil
	}
	root := cmp.Or(strings.TrimSpace(baseDir), roots[0])
	for _, rawPath := range pathArgs {
		joined := filepath.Clean(filepath.Join(root, rawPath))
		if !isWithinAnyRoot(roots, joined) {
			return fmt.Errorf("read.cmd: %s path %q resolves outside allowed roots", prog, rawPath)
		}
		if err := verifySymlinkConfinement(roots, joined, rawPath); err != nil {
			return err
		}
		if isSensitiveInvestigationPath(joined) {
			return fmt.Errorf("read.cmd: %s path %q is restricted (sensitive or database file)", prog, rawPath)
		}
		if prog == "grep" || prog == "rg" {
			if info, err := os.Stat(joined); err == nil && info.IsDir() {
				return fmt.Errorf("read.cmd: %s path %q is a directory; specify explicit file paths in read-only mode", prog, rawPath)
			}
		}
	}
	return nil
}

type limitedOutputBuffer struct {
	data  []byte
	limit int
}

func (b *limitedOutputBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	if remaining := b.limit - len(b.data); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return origLen, nil
}

func scrubbedReadCmdEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+4)
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_EXTERNAL_DIFF", "GIT_INDEX_FILE",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
			"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_EXEC_PATH",
			"GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS", "GIT_PAGER":
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
	)
}

func minimalReadCmdEnv(environ []string) []string {
	out := make([]string, 0, 8)
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "HOME":
			out = append(out, kv)
		}
	}
	return append(out, "RIPGREP_CONFIG_PATH=/dev/null")
}

func executeReadCmd(ctx context.Context, cfg InvestigateConfig, p ReadCmdParams) (string, error) {
	prog, cleanArgs, pathArgs, err := normalizeReadOnlyCommand(p.Program, p.Args)
	if err != nil {
		return "", err
	}
	dir := cmp.Or(strings.TrimSpace(cfg.RepoDir), strings.TrimSpace(cfg.RunDir))
	if prog == "git" {
		if err := validateGitRootConfinement(cfg.allowedRoots(), dir, cleanArgs); err != nil {
			return "", err
		}
	}
	if len(pathArgs) > 0 {
		if err := validateCommandPathConfinement(cfg.allowedRoots(), dir, prog, pathArgs); err != nil {
			return "", err
		}
	}
	var out []byte
	if cfg.CommandRunner != nil {
		out, err = cfg.CommandRunner(ctx, dir, prog, cleanArgs)
	} else {
		cmdCtx, cancel := context.WithTimeout(ctx, defaultReadCmdTimeout)
		defer cancel()
		cmd := exec.CommandContext(cmdCtx, prog, cleanArgs...)
		if dir != "" {
			cmd.Dir = dir
		}
		switch prog {
		case "gh", "nebula", "git":
			cmd.Env = scrubbedReadCmdEnv(os.Environ())
		default:
			cmd.Env = minimalReadCmdEnv(os.Environ())
		}
		outBuf := &limitedOutputBuffer{limit: defaultReadFileMaxBytes + 1}
		cmd.Stdout = outBuf
		cmd.Stderr = outBuf
		err = cmd.Run()
		out = outBuf.data
	}
	text := strings.TrimSpace(string(out))
	if len(text) > defaultReadFileMaxBytes {
		text = text[:defaultReadFileMaxBytes] + "\n...[truncated]"
	}
	if err != nil {
		if text != "" {
			return "", fmt.Errorf("read.cmd %s: %w: %s", prog, err, text)
		}
		return "", fmt.Errorf("read.cmd %s: %w", prog, err)
	}
	if text == "" {
		return "(no output)", nil
	}
	return text, nil
}
