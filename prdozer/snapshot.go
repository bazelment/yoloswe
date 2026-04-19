package prdozer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/wt"
)

// Coarse status-rollup classification used across snapshot and changeset.
const (
	StatusSuccess = "SUCCESS"
	StatusPending = "PENDING"
	StatusFailure = "FAILURE"
)

// Snapshot is a point-in-time view of a PR's state.
type Snapshot struct {
	TakenAt      time.Time
	BaseSHA      string
	StatusRollup string
	Comments     []CommentRef
	FailedRunIDs []int64
	PR           PRDetails
}

// PRDetails is the fields prdozer cares about from `gh pr view`.
type PRDetails struct {
	URL               string           `json:"url"`
	HeadRefName       string           `json:"headRefName"`
	BaseRefName       string           `json:"baseRefName"`
	HeadRefOid        string           `json:"headRefOid"`
	State             string           `json:"state"`
	ReviewDecision    string           `json:"reviewDecision"`
	Mergeable         string           `json:"mergeable"`
	StatusCheckRollup []statusCheckRow `json:"statusCheckRollup"`
	Number            int              `json:"number"`
	IsDraft           bool             `json:"isDraft"`
}

// statusCheckRow is a single entry in gh's statusCheckRollup. Some checks set
// `conclusion`, others use `state` — we collapse both downstream.
type statusCheckRow struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// CommentRef is a single comment we've observed; we keep enough to dedupe and
// classify human vs bot.
type CommentRef struct {
	Created time.Time `json:"created_at"`
	ID      string    `json:"id"`
	Source  string    `json:"source"`
	Author  string    `json:"author"`
	IsBot   bool      `json:"is_bot"`
	IsSelf  bool      `json:"is_self"`
}

// SnapshotOptions controls how a snapshot is taken.
type SnapshotOptions struct {
	CommentsSince time.Time
	Self          string
}

// TakeSnapshot fetches the current state of a PR via gh. The initial pr view
// call runs synchronously (we need its URL/HeadRefName to parameterize the
// follow-up calls); independent follow-ups run concurrently.
func TakeSnapshot(ctx context.Context, gh wt.GHRunner, dir string, prNumber int, opts SnapshotOptions) (*Snapshot, error) {
	pr, err := fetchPRDetails(ctx, gh, dir, prNumber)
	if err != nil {
		return nil, fmt.Errorf("pr view #%d: %w", prNumber, err)
	}
	owner, repo, err := repoSlugFromURL(pr.URL)
	if err != nil {
		return nil, fmt.Errorf("derive owner/repo from %s: %w", pr.URL, err)
	}

	var (
		wg                     sync.WaitGroup
		failed                 []int64
		comments               []CommentRef
		baseSHA                string
		failedErr, commentsErr error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		failed, failedErr = fetchFailedRunIDs(ctx, gh, dir, pr.HeadRefName)
	}()
	go func() {
		defer wg.Done()
		comments, commentsErr = fetchAllComments(ctx, gh, dir, owner, repo, prNumber, opts)
	}()
	go func() {
		defer wg.Done()
		// Base detection is best-effort — the changeset will skip the
		// BaseMoved signal if we can't read the SHA, so we swallow errors here.
		baseSHA, _ = fetchBaseSHA(ctx, gh, dir, pr.BaseRefName)
	}()
	wg.Wait()
	if failedErr != nil {
		return nil, fmt.Errorf("failed runs for %s: %w", pr.HeadRefName, failedErr)
	}
	if commentsErr != nil {
		return nil, fmt.Errorf("comments for #%d: %w", prNumber, commentsErr)
	}
	return &Snapshot{
		TakenAt:      time.Now().UTC(),
		PR:           *pr,
		Comments:     comments,
		FailedRunIDs: failed,
		BaseSHA:      baseSHA,
		StatusRollup: summarizeRollup(pr.StatusCheckRollup),
	}, nil
}

func fetchPRDetails(ctx context.Context, gh wt.GHRunner, dir string, n int) (*PRDetails, error) {
	args := []string{
		"pr", "view", strconv.Itoa(n),
		"--json", "number,url,headRefName,baseRefName,headRefOid,state,isDraft,reviewDecision,mergeable,statusCheckRollup",
	}
	res, err := gh.Run(ctx, args, dir)
	if err != nil {
		return nil, ghError(err, res)
	}
	var pr PRDetails
	if err := json.Unmarshal([]byte(res.Stdout), &pr); err != nil {
		return nil, fmt.Errorf("parse pr view: %w", err)
	}
	return &pr, nil
}

func summarizeRollup(rows []statusCheckRow) string {
	if len(rows) == 0 {
		return ""
	}
	anyPending := false
	anyFailure := false
	for _, c := range rows {
		concl := strings.ToUpper(c.Conclusion)
		if concl == "" {
			concl = strings.ToUpper(c.State)
		}
		switch {
		case concl == "FAILURE", concl == "TIMED_OUT", concl == "CANCELLED", concl == "ERROR":
			anyFailure = true
		case strings.EqualFold(c.Status, "IN_PROGRESS"), strings.EqualFold(c.Status, "QUEUED"), concl == "PENDING":
			anyPending = true
		}
	}
	switch {
	case anyFailure:
		return StatusFailure
	case anyPending:
		return StatusPending
	default:
		return StatusSuccess
	}
}

type ghRunListItem struct {
	DatabaseID int64 `json:"databaseId"`
}

func fetchFailedRunIDs(ctx context.Context, gh wt.GHRunner, dir, branch string) ([]int64, error) {
	if branch == "" {
		return nil, nil
	}
	args := []string{
		"run", "list",
		"--branch", branch,
		"--status", "failure",
		"--json", "databaseId",
		"--limit", "10",
	}
	res, err := gh.Run(ctx, args, dir)
	if err != nil {
		return nil, ghError(err, res)
	}
	var items []ghRunListItem
	if err := json.Unmarshal([]byte(res.Stdout), &items); err != nil {
		return nil, fmt.Errorf("parse run list: %w", err)
	}
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.DatabaseID)
	}
	return out, nil
}

type ghComment struct {
	CreatedAt time.Time   `json:"created_at"`
	User      ghUser      `json:"user"`
	ID        json.Number `json:"id"`
}

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"` // "Bot" for bot accounts
}

func fetchAllComments(ctx context.Context, gh wt.GHRunner, dir, owner, repo string, n int, opts SnapshotOptions) ([]CommentRef, error) {
	inline, err := fetchComments(ctx, gh, dir, fmt.Sprintf("repos/%s/%s/pulls/%d/comments", owner, repo, n), "inline", opts)
	if err != nil {
		return nil, err
	}
	issue, err := fetchComments(ctx, gh, dir, fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, repo, n), "issue", opts)
	if err != nil {
		return nil, err
	}
	return append(inline, issue...), nil
}

func fetchComments(ctx context.Context, gh wt.GHRunner, dir, endpoint, source string, opts SnapshotOptions) ([]CommentRef, error) {
	args := []string{"api", "--paginate", endpoint}
	if !opts.CommentsSince.IsZero() {
		args = append(args, "-f", "since="+opts.CommentsSince.UTC().Format(time.RFC3339))
	}
	res, err := gh.Run(ctx, args, dir)
	if err != nil {
		return nil, ghError(err, res)
	}
	body := strings.TrimSpace(res.Stdout)
	if body == "" {
		return nil, nil
	}
	// `gh api --paginate` emits one JSON array per page concatenated back-to-
	// back (e.g. `[a,b][c,d]`), which is NOT valid as a single JSON value.
	// Decode page arrays in a loop and flatten.
	dec := json.NewDecoder(strings.NewReader(body))
	var raw []ghComment
	for dec.More() {
		var page []ghComment
		if err := dec.Decode(&page); err != nil {
			return nil, fmt.Errorf("parse comments (%s): %w", source, err)
		}
		raw = append(raw, page...)
	}
	out := make([]CommentRef, 0, len(raw))
	for _, c := range raw {
		// Inline (/pulls/{n}/comments) and issue (/issues/{n}/comments) comment
		// IDs come from separate GitHub ID sequences and can collide. Namespace
		// the dedup key by source so two distinct comments with the same
		// numeric ID aren't silently dropped as duplicates.
		out = append(out, CommentRef{
			ID:      source + ":" + string(c.ID),
			Source:  source,
			Author:  c.User.Login,
			IsBot:   c.User.Type == "Bot" || strings.HasSuffix(c.User.Login, "[bot]"),
			IsSelf:  opts.Self != "" && c.User.Login == opts.Self,
			Created: c.CreatedAt,
		})
	}
	return out, nil
}

func fetchBaseSHA(ctx context.Context, gh wt.GHRunner, dir, base string) (string, error) {
	if base == "" {
		return "", nil
	}
	res, err := gh.Run(ctx, []string{
		"api", fmt.Sprintf("repos/{owner}/{repo}/git/refs/heads/%s", base),
		"--jq", ".object.sha",
	}, dir)
	if err != nil {
		return "", ghError(err, res)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// repoSlugFromURL parses an HTML PR URL to (owner, repo).
// Example: https://github.com/sycamore-labs/kernel/pull/2318 → ("sycamore-labs", "kernel").
func repoSlugFromURL(url string) (string, string, error) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(url, prefix) {
		return "", "", fmt.Errorf("not a github.com URL")
	}
	rest := strings.TrimPrefix(url, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("malformed URL")
	}
	return parts[0], parts[1], nil
}

func ghError(err error, res *wt.CmdResult) error {
	if res != nil && res.Stderr != "" {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(res.Stderr))
	}
	return err
}
