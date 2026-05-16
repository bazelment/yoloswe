package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/wt"
)

// ReviewComment is normalized feedback from GitHub PR reviews and review
// comments.
type ReviewComment struct {
	CreatedAt time.Time
	Path      string
	Body      string
	Author    string
	URL       string
	Line      int
}

// FetchPRReviewComments fetches actionable PR feedback through gh. prRef may
// be a PR number or URL. The gh command runs in dir so {owner}/{repo}
// placeholders resolve from the repository remote.
func FetchPRReviewComments(ctx context.Context, gh wt.GHRunner, dir, prRef string) ([]ReviewComment, error) {
	number, err := parsePRNumber(prRef)
	if err != nil {
		return nil, err
	}

	lineComments, err := fetchPRLineComments(ctx, gh, dir, number)
	if err != nil {
		return nil, err
	}
	reviewComments, err := fetchPRReviewBodies(ctx, gh, dir, number)
	if err != nil {
		return nil, err
	}

	lineComments = append(lineComments, reviewComments...)
	comments := lineComments
	sort.SliceStable(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	return comments, nil
}

func parsePRNumber(prRef string) (int, error) {
	prRef = strings.TrimSpace(prRef)
	if prRef == "" {
		return 0, fmt.Errorf("PR reference is empty")
	}
	if n, err := strconv.Atoi(prRef); err == nil && n > 0 {
		return n, nil
	}
	u, err := url.Parse(prRef)
	if err != nil {
		return 0, fmt.Errorf("invalid PR reference %q: %w", prRef, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] != "pull" {
			continue
		}
		n, err := strconv.Atoi(parts[i+1])
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid PR number in %q", prRef)
		}
		return n, nil
	}
	return 0, fmt.Errorf("invalid PR reference %q: expected number or pull request URL", prRef)
}

func fetchPRLineComments(ctx context.Context, gh wt.GHRunner, dir string, number int) ([]ReviewComment, error) {
	repo, err := fetchCurrentRepo(ctx, gh, dir)
	if err != nil {
		return nil, err
	}

	result, err := gh.Run(ctx, []string{
		"api", "graphql",
		"-f", "query=" + prReviewThreadsQuery,
		"-F", "owner=" + repo.Owner.Login,
		"-F", "name=" + repo.Name,
		"-F", "number=" + strconv.Itoa(number),
	}, dir)
	if err != nil {
		return nil, fmt.Errorf("fetch PR review threads: %w", err)
	}

	var raw ghPRReviewThreadsResponse
	if err := json.Unmarshal([]byte(result.Stdout), &raw); err != nil {
		return nil, fmt.Errorf("parse PR review threads: %w", err)
	}
	return normalizePRReviewThreads(raw.Data.Repository.PullRequest.ReviewThreads.Nodes)
}

func fetchCurrentRepo(ctx context.Context, gh wt.GHRunner, dir string) (*ghRepoView, error) {
	result, err := gh.Run(ctx, []string{"repo", "view", "--json", "owner,name"}, dir)
	if err != nil {
		return nil, fmt.Errorf("resolve current GitHub repository: %w", err)
	}
	var repo ghRepoView
	if err := json.Unmarshal([]byte(result.Stdout), &repo); err != nil {
		return nil, fmt.Errorf("parse current GitHub repository: %w", err)
	}
	if repo.Owner.Login == "" || repo.Name == "" {
		return nil, fmt.Errorf("current GitHub repository is missing owner or name")
	}
	return &repo, nil
}

func normalizePRReviewThreads(raw []ghPRReviewThread) ([]ReviewComment, error) {
	var out []ReviewComment
	for _, thread := range raw {
		if thread.IsResolved {
			continue
		}
		for i := range thread.Comments.Nodes {
			c := &thread.Comments.Nodes[i]
			if skipGitHubAuthor(c.Author) || strings.TrimSpace(c.Body) == "" {
				continue
			}
			createdAt, err := parseGitHubTime(c.CreatedAt)
			if err != nil {
				return nil, err
			}
			line := 0
			if c.Line != nil {
				line = *c.Line
			}
			if line == 0 && c.OriginalLine != nil {
				line = *c.OriginalLine
			}
			out = append(out, ReviewComment{
				Path:      c.Path,
				Line:      line,
				Body:      strings.TrimSpace(c.Body),
				Author:    c.Author.Login,
				URL:       c.URL,
				CreatedAt: createdAt,
			})
		}
	}
	return out, nil
}

func fetchPRReviewBodies(ctx context.Context, gh wt.GHRunner, dir string, number int) ([]ReviewComment, error) {
	result, err := gh.Run(ctx, []string{
		"api",
		fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/reviews", number),
	}, dir)
	if err != nil {
		return nil, fmt.Errorf("fetch PR reviews: %w", err)
	}

	var raw []ghPRReview
	if err := json.Unmarshal([]byte(result.Stdout), &raw); err != nil {
		return nil, fmt.Errorf("parse PR reviews: %w", err)
	}
	return normalizePRReviews(raw)
}

func normalizePRReviews(raw []ghPRReview) ([]ReviewComment, error) {
	var out []ReviewComment
	for _, r := range raw {
		body := strings.TrimSpace(r.Body)
		if skipGitHubAuthor(r.User) || body == "" || strings.EqualFold(r.State, "APPROVED") {
			continue
		}
		submittedAt, err := parseGitHubTime(r.SubmittedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, ReviewComment{
			Body:      body,
			Author:    r.User.Login,
			URL:       r.HTMLURL,
			CreatedAt: submittedAt,
		})
	}
	return out, nil
}

// FormatPRReviewFeedback renders comments into the feedback block given to the
// validate agent.
func FormatPRReviewFeedback(comments []ReviewComment) string {
	if len(comments) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range comments {
		location := "PR review"
		if c.Path != "" {
			location = c.Path
			if c.Line > 0 {
				location += ":" + strconv.Itoa(c.Line)
			}
		}
		fmt.Fprintf(&b, "- %s by @%s: %q", location, c.Author, c.Body)
		if c.URL != "" {
			fmt.Fprintf(&b, " (%s)", c.URL)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func skipGitHubAuthor(user ghReviewUser) bool {
	login := strings.ToLower(user.Login)
	return user.Type == "Bot" || user.Typename == "Bot" || strings.HasSuffix(login, "[bot]") || strings.Contains(login, "jiradozer")
}

func parseGitHubTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return t, nil
}

type ghPRReview struct {
	User        ghReviewUser `json:"user"`
	Body        string       `json:"body"`
	State       string       `json:"state"`
	HTMLURL     string       `json:"html_url"`
	SubmittedAt string       `json:"submitted_at"`
}

type ghReviewUser struct {
	Login    string `json:"login"`
	Type     string `json:"type"`
	Typename string `json:"__typename"`
}

type ghRepoView struct {
	Owner ghReviewUser `json:"owner"`
	Name  string       `json:"name"`
}

type ghPRReviewThreadsResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []ghPRReviewThread `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

type ghPRReviewThread struct {
	Comments struct {
		Nodes []ghPRThreadComment `json:"nodes"`
	} `json:"comments"`
	IsResolved bool `json:"isResolved"`
}

//nolint:govet // fieldalignment: mirrors the GitHub GraphQL response shape.
type ghPRThreadComment struct {
	Author       ghReviewUser `json:"author"`
	Body         string       `json:"body"`
	Path         string       `json:"path"`
	URL          string       `json:"url"`
	CreatedAt    string       `json:"createdAt"`
	Line         *int         `json:"line"`
	OriginalLine *int         `json:"originalLine"`
}

const prReviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) {
        nodes {
          isResolved
          comments(first: 100) {
            nodes {
              body
              path
              line
              originalLine
              url
              createdAt
              author {
                login
                __typename
              }
            }
          }
        }
      }
    }
  }
}`
