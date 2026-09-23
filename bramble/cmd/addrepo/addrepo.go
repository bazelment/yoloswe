// Package addrepo provides the "bramble add-repo" subcommand.
package addrepo

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/wt"
)

// NewCmd returns the "bramble add-repo" command. resolveWTRoot must be the
// same resolver the repo picker uses, so added repos land where it looks.
func NewCmd(resolveWTRoot func() (string, error)) *cobra.Command {
	return newCmd(resolveWTRoot, newManagerInitializer)
}

func newCmd(resolveWTRoot func() (string, error), newInit func(root string) repoInitializer) *cobra.Command {
	return &cobra.Command{
		Use:          "add-repo <repo>",
		Short:        "Add a GitHub repository to the repo list",
		SilenceUsage: true,
		Long: `Clone a GitHub repository into WT_ROOT as a bare repo plus its default-branch
worktree. The new repo then shows up in the bramble repo picker, the same as
adding it from that screen.

<repo> is owner/name, an https URL, or an SSH URL:

  bramble add-repo owner/repo
  bramble add-repo https://github.com/owner/repo
  bramble add-repo git@github.com:owner/repo.git

WT_ROOT selects the destination (default: ~/worktrees).`,
		Example: `  bramble add-repo bazelment/yoloswe
  bramble add-repo https://github.com/bazelment/yoloswe.git`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wtRoot, err := resolveWTRoot()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return addRepo(ctx, args[0], wtRoot, cmd.OutOrStdout(), newInit)
		},
	}
}

type repoInitializer interface {
	Init(ctx context.Context, url string) (string, error)
}

func newManagerInitializer(wtRoot string) repoInitializer {
	return wt.NewManager(wtRoot, "", wt.WithOutput(wt.NewOutput(os.Stderr, false)))
}

func addRepo(ctx context.Context, raw, wtRoot string, stdout io.Writer, newInit func(root string) repoInitializer) error {
	if strings.TrimSpace(wtRoot) == "" {
		return fmt.Errorf("WT_ROOT is empty")
	}
	cloneURL, err := parseGitHubRepo(raw)
	if err != nil {
		return err
	}
	repoName := wt.GetRepoNameFromURL(cloneURL)
	mainPath, err := newInit(wtRoot).Init(ctx, cloneURL)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "added %s at %s\n", repoName, mainPath); err != nil {
		return err
	}
	return nil
}

var (
	ownerRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

func parseGitHubRepo(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("repository is required")
	}

	if cloneURL, ok, err := parseSCP(raw); ok || err != nil {
		return cloneURL, err
	}

	if !strings.Contains(raw, "://") {
		lower := strings.ToLower(raw)
		switch {
		case strings.HasPrefix(lower, "github.com/"), strings.HasPrefix(lower, "www.github.com/"):
			raw = "https://" + raw
		case ownerRepoPattern.MatchString(raw):
			owner, repo, err := splitOwnerRepo(raw)
			if err != nil {
				return "", err
			}
			return httpsClone(owner, repo), nil
		default:
			return "", fmt.Errorf("invalid GitHub repository %q", raw)
		}
	}

	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid GitHub repository %q", raw)
	}
	if !isGitHubHost(u.Hostname()) {
		return "", fmt.Errorf("%q is not a github.com repository", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid GitHub repository %q", raw)
	}
	owner, repo, err := splitOwnerRepo(strings.Trim(u.Path, "/"))
	if err != nil {
		return "", fmt.Errorf("expected owner/name in %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http", "git":
		// GitHub's retired git protocol clones over HTTPS.
		return httpsClone(owner, repo), nil
	case "ssh":
		return sshClone(owner, repo), nil
	default:
		return "", fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
}

func parseSCP(raw string) (string, bool, error) {
	if strings.Contains(raw, "://") || !strings.Contains(raw, ":") {
		return "", false, nil
	}
	userHost, path, ok := strings.Cut(raw, ":")
	if !ok || userHost == "" || path == "" || strings.Contains(userHost, "/") {
		return "", false, nil
	}
	user, host, ok := strings.Cut(userHost, "@")
	if !ok || user == "" || host == "" {
		return "", false, nil
	}
	if !isGitHubHost(host) {
		return "", true, fmt.Errorf("%q is not a github.com repository", raw)
	}
	owner, repo, err := splitOwnerRepo(path)
	if err != nil {
		return "", true, fmt.Errorf("expected owner/name in %q", raw)
	}
	return sshClone(owner, repo), true, nil
}

func splitOwnerRepo(path string) (string, string, error) {
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", fmt.Errorf("expected owner/name, got %q", path)
	}
	if !namePattern.MatchString(owner) || !namePattern.MatchString(repo) || repo == "." || repo == ".." || owner == "." || owner == ".." {
		return "", "", fmt.Errorf("invalid GitHub repository name %q", path)
	}
	return owner, repo, nil
}

func isGitHubHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || host == "www.github.com"
}

func httpsClone(owner, repo string) string {
	return "https://github.com/" + owner + "/" + repo + ".git"
}

func sshClone(owner, repo string) string {
	return "git@github.com:" + owner + "/" + repo + ".git"
}
