package toolbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

//nolint:govet // GitHub JSON fields are intentionally grouped by endpoint.
type prView struct {
	HeadRefOid  string `json:"headRefOid"`
	BaseRefOid  string `json:"baseRefOid"`
	BaseRefName string `json:"baseRefName"`
	State       string `json:"state"`
	IsDraft     bool   `json:"isDraft"`
	Title       string `json:"title"`
}

//nolint:govet // readiness output is a compact JSON contract.
type readiness struct {
	PR      int      `json:"pr"`
	Head    string   `json:"head"`
	Base    string   `json:"base"`
	Ready   bool     `json:"ready"`
	Reasons []string `json:"reasons"`
}

func ghRead(ctx context.Context, dir string, dest any, args ...string) error {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		return refuse("GitHub read failed (%s); readiness is unknown", strings.Join(args[:min(2, len(args))], " "))
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return refuse("malformed GitHub read; readiness is unknown")
	}
	return nil
}

// requiredChecks reads check runs from the exact commit endpoint, rather than
// trusting a status string imported from a prior controller tick.
func requiredChecks(ctx context.Context, dir, repo, sha string) error {
	//nolint:govet // nested GitHub check JSON mirrors the API response.
	var pages []struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Head       string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := ghRead(ctx, dir, &pages, "api", "--paginate", "--slurp", "repos/"+repo+"/commits/"+sha+"/check-runs?per_page=100"); err != nil {
		return err
	}
	for _, name := range []string{"Lint", "Build and Test"} {
		var latest int64
		pass := false
		for _, page := range pages {
			for _, check := range page.CheckRuns {
				if check.Name == name && check.ID > latest {
					latest = check.ID
					pass = check.Head == sha && check.Status == "completed" && check.Conclusion == "success"
				}
			}
		}
		if !pass {
			return refuse("%s is missing, pending or failing on %s", name, sha)
		}
	}
	return nil
}

func readyPR(ctx context.Context, o Options, s state, t task) readiness {
	v := readiness{PR: t.PR, Head: t.PRHead, Reasons: []string{}}
	fail := func(err error) readiness { v.Reasons = append(v.Reasons, err.Error()); return v }
	repo, _ := s.Config["repo"].(string)
	if len(strings.Split(repo, "/")) != 2 || strings.ContainsAny(repo, " ?#\\\n\r") {
		return fail(refuse("run repo must be owner/name"))
	}
	var pr prView
	if err := ghRead(ctx, o.Dir, &pr, "pr", "view", strconv.Itoa(t.PR), "--repo", repo, "--json", "headRefOid,baseRefOid,baseRefName,state,isDraft,title"); err != nil {
		return fail(err)
	}
	v.Head, v.Base = pr.HeadRefOid, pr.BaseRefOid
	if pr.State != "OPEN" || pr.IsDraft {
		return fail(refuse("PR is closed, merged or draft"))
	}
	if pr.HeadRefOid == "" || t.PRHead != pr.HeadRefOid || t.ApprovalSHA != pr.HeadRefOid {
		return fail(refuse("approval and ledger head must match the current PR head"))
	}
	if err := requiredChecks(ctx, o.Dir, repo, pr.HeadRefOid); err != nil {
		return fail(err)
	}
	var base struct {
		SHA string `json:"sha"`
	}
	if err := ghRead(ctx, o.Dir, &base, "api", "repos/"+repo+"/commits/"+pr.BaseRefName); err != nil {
		return fail(err)
	}
	if base.SHA == "" || base.SHA != pr.BaseRefOid {
		return fail(refuse("base moved during readiness read"))
	}
	if err := requiredChecks(ctx, o.Dir, repo, base.SHA); err != nil {
		return fail(refuse("main/base is red or unknown: %v", err))
	}
	var compare struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := ghRead(ctx, o.Dir, &compare, "api", "repos/"+repo+"/compare/"+base.SHA+"..."+pr.HeadRefOid); err != nil {
		return fail(err)
	}
	if compare.MergeBase.SHA != base.SHA {
		return fail(refuse("PR base is stale; refresh against the current base"))
	}
	// Reuse the skill's read-only claim checker on local run reports and the
	// changed-file list. It does not fetch or alter a lane checkout.
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	claims := filepath.Join(home, ".claude", "skills", "subagent-swarm-nebula", "scripts", "merge_claims.py")
	if _, err := os.Stat(claims); err != nil {
		return fail(refuse("skill merge-claim checker unavailable"))
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "diff", strconv.Itoa(t.PR), "--repo", repo, "--name-only")
	cmd.Dir = o.Dir
	paths, err := cmd.Output()
	if err != nil {
		return fail(refuse("cannot read PR changed paths for merge claims"))
	}
	args := []string{claims, "check", "--head", pr.HeadRefOid, "--title", t.Title, "--title", pr.Title}
	for _, path := range strings.Fields(string(paths)) {
		args = append(args, "--changed-file", path)
	}
	for _, phase := range []string{"swe", "clean", "review"} {
		for _, report := range []string{filepath.Join(o.Dir, t.ID+"."+phase+".md"), filepath.Join(o.Dir, "reports", t.ID+"."+phase+".r"+strconv.Itoa(t.Round)+".md")} {
			if info, err := os.Stat(report); err == nil && info.Mode().IsRegular() {
				args = append(args, "--report", report)
			}
		}
	}
	cmd = exec.CommandContext(ctx, "python3", args...)
	cmd.Dir = o.Dir
	if _, err := cmd.Output(); err != nil {
		return fail(refuse("skill merge claims failed or unavailable"))
	}
	v.Ready = true
	return v
}

func readinessText(v readiness) string {
	return fmt.Sprintf("PR #%d ready=%t head=%s base=%s %s", v.PR, v.Ready, v.Head, v.Base, strings.Join(v.Reasons, "; "))
}
