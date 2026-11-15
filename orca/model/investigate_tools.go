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
		}
		return strings.TrimSpace(b.String()), nil
	}

	limit := p.MaxBytes
	if limit <= 0 {
		limit = defaultReadFileMaxBytes
	}
	if limit > ceilingReadFileMaxBytes {
		limit = ceilingReadFileMaxBytes
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
		cand := filepath.Clean(filepath.Join(root, rawPath))
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
// read-only gh, nebula, or git inspection command.
func ValidateReadOnlyCommand(program string, args []string) error {
	_, _, err := normalizeReadOnlyCommand(program, args)
	return err
}

func normalizeReadOnlyCommand(program string, args []string) (string, []string, error) {
	prog := strings.TrimSpace(program)
	var cleanArgs []string
	for _, a := range args {
		if trimmed := strings.TrimSpace(a); trimmed != "" {
			cleanArgs = append(cleanArgs, trimmed)
		}
	}
	if len(cleanArgs) == 0 {
		return "", nil, errors.New("read.cmd: args are required")
	}

	switch prog {
	case "gh":
		for _, arg := range cleanArgs {
			if arg == "-w" || arg == "--web" {
				return "", nil, fmt.Errorf("read.cmd: interactive flag %q is not permitted", arg)
			}
		}
		sub := cleanArgs[0]
		switch sub {
		case "pr":
			if len(cleanArgs) < 2 {
				return "", nil, errors.New("read.cmd: gh pr requires a read-only subcommand (view, list, checks, status, diff)")
			}
			switch cleanArgs[1] {
			case "view", "list", "checks", "status", "diff":
				return prog, cleanArgs, nil
			default:
				return "", nil, fmt.Errorf("read.cmd: gh pr %q is not a permitted read-only subcommand", cleanArgs[1])
			}
		case "run", "issue":
			if len(cleanArgs) < 2 {
				return "", nil, fmt.Errorf("read.cmd: gh %s requires a read-only subcommand (view, list, status)", sub)
			}
			switch cleanArgs[1] {
			case "view", "list", "status":
				return prog, cleanArgs, nil
			default:
				return "", nil, fmt.Errorf("read.cmd: gh %s %q is not a permitted read-only subcommand", sub, cleanArgs[1])
			}
		case "repo":
			if len(cleanArgs) < 2 || cleanArgs[1] != "view" {
				return "", nil, errors.New("read.cmd: only 'gh repo view' is permitted")
			}
			return prog, cleanArgs, nil
		default:
			return "", nil, fmt.Errorf("read.cmd: gh %q is not a permitted read-only command", sub)
		}

	case "nebula":
		if len(cleanArgs) < 2 {
			return "", nil, errors.New("read.cmd: nebula requires a resource and read-only action (e.g. post get, mail list, terminal logs)")
		}
		res, action := cleanArgs[0], cleanArgs[1]
		switch res {
		case "post":
			if action == "get" || action == "list" {
				return prog, cleanArgs, nil
			}
		case "mail":
			if action == "list" || action == "get" || action == "read" || action == "status" {
				return prog, cleanArgs, nil
			}
		case "terminal":
			if action == "get" || action == "list" || action == "logs" || action == "status" || action == "read" {
				return prog, cleanArgs, nil
			}
		case "session", "run":
			if action == "get" || action == "list" || action == "status" {
				return prog, cleanArgs, nil
			}
		}
		return "", nil, fmt.Errorf("read.cmd: nebula %s %s is not a permitted read-only command", res, action)

	case "git":
		return normalizeGitReadOnlyCommand(prog, cleanArgs)

	default:
		return "", nil, fmt.Errorf("read.cmd: program %q is not permitted (only gh, nebula, and git are allowed)", prog)
	}
}

func normalizeGitReadOnlyCommand(prog string, cleanArgs []string) (string, []string, error) {
	sub := cleanArgs[0]
	if strings.HasPrefix(sub, "-") {
		return "", nil, fmt.Errorf("read.cmd: git global flag %q is not permitted before subcommand", sub)
	}
	switch sub {
	case "status", "log", "diff", "show", "rev-parse":
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

func executeReadCmd(ctx context.Context, cfg InvestigateConfig, p ReadCmdParams) (string, error) {
	prog, cleanArgs, err := normalizeReadOnlyCommand(p.Program, p.Args)
	if err != nil {
		return "", err
	}
	dir := cmp.Or(strings.TrimSpace(cfg.RepoDir), strings.TrimSpace(cfg.RunDir))
	if prog == "git" {
		if err := validateGitRootConfinement(cfg.allowedRoots(), dir, cleanArgs); err != nil {
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
		cmd.Env = scrubbedReadCmdEnv(os.Environ())
		out, err = cmd.CombinedOutput()
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
