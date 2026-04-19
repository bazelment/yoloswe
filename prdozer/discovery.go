package prdozer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/wt"
)

// DiscoveredPR is the minimal info needed to route a PR to a watcher.
type DiscoveredPR struct {
	HeadRefName string   `json:"headRefName"`
	BaseRefName string   `json:"baseRefName"`
	URL         string   `json:"url"`
	Labels      []string `json:"-"`
	Number      int      `json:"number"`
	IsDraft     bool     `json:"isDraft"`
}

type discoverPRRaw struct {
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	URL         string `json:"url"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Number  int  `json:"number"`
	IsDraft bool `json:"isDraft"`
}

// DiscoverPRs queries gh for open PRs matching the source filter.
// For Mode=single/list, it just returns the listed PR numbers (after fetching
// their details so we have head/base/url).
func DiscoverPRs(ctx context.Context, gh wt.GHRunner, dir string, src SourceConfig) ([]DiscoveredPR, error) {
	switch src.Mode {
	case SourceModeSingle, SourceModeList:
		return fetchByNumbers(ctx, gh, dir, src.PRs)
	case SourceModeAll, "":
		return discoverAll(ctx, gh, dir, src.Filter)
	default:
		return nil, fmt.Errorf("unsupported source.mode %q", src.Mode)
	}
}

func fetchByNumbers(ctx context.Context, gh wt.GHRunner, dir string, numbers []int) ([]DiscoveredPR, error) {
	out := make([]DiscoveredPR, len(numbers))
	errs := make([]error, len(numbers))
	var wg sync.WaitGroup
	for i, n := range numbers {
		wg.Add(1)
		go func(i, n int) {
			defer wg.Done()
			args := []string{
				"pr", "view", fmt.Sprintf("%d", n),
				"--json", "number,headRefName,baseRefName,url,isDraft,labels",
			}
			res, err := gh.Run(ctx, args, dir)
			if err != nil {
				errs[i] = ghError(err, res)
				return
			}
			var raw discoverPRRaw
			if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
				errs[i] = fmt.Errorf("parse pr view #%d: %w", n, err)
				return
			}
			out[i] = toDiscovered(raw)
		}(i, n)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func discoverAll(ctx context.Context, gh wt.GHRunner, dir string, f SourceFilter) ([]DiscoveredPR, error) {
	args := []string{
		"pr", "list",
		"--state", "open",
		"--json", "number,headRefName,baseRefName,url,isDraft,labels",
		"--limit", "200",
	}
	if author := strings.TrimSpace(f.Author); author != "" {
		args = append(args, "--author", author)
	}
	for _, label := range f.Labels {
		args = append(args, "--label", label)
	}
	res, err := gh.Run(ctx, args, dir)
	if err != nil {
		return nil, ghError(err, res)
	}
	var raws []discoverPRRaw
	if err := json.Unmarshal([]byte(res.Stdout), &raws); err != nil {
		return nil, fmt.Errorf("parse pr list: %w", err)
	}
	exclude := make(map[string]bool, len(f.ExcludeLabels))
	for _, l := range f.ExcludeLabels {
		exclude[l] = true
	}
	out := make([]DiscoveredPR, 0, len(raws))
raw:
	for _, r := range raws {
		for _, l := range r.Labels {
			if exclude[l.Name] {
				continue raw
			}
		}
		out = append(out, toDiscovered(r))
	}
	return out, nil
}

func toDiscovered(r discoverPRRaw) DiscoveredPR {
	labels := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	return DiscoveredPR{
		Number:      r.Number,
		HeadRefName: r.HeadRefName,
		BaseRefName: r.BaseRefName,
		URL:         r.URL,
		IsDraft:     r.IsDraft,
		Labels:      labels,
	}
}
