package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/jiradozer/tracker/local"
)

//nolint:govet // fieldalignment: keep embedded run args and refine-only flags grouped by purpose.
type refineArgs struct {
	runArgs
	feedback     string
	feedbackFile string
	prRef        string
	noPoll       bool
}

func newRefineCommand(args *refineArgs, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refine",
		Short: "Address review feedback on an existing PR",
		Long:  "Re-enter a completed jiradozer workflow at validate, using explicit feedback or GitHub PR review comments, and update the existing branch/PR in place.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			args.dryRunSet = dryRunChanged(cmd)
			args.configPath = *configPath
			app := cliapp.FromContext(cmd.Context())
			return refine(cmd.Context(), app, *args)
		},
	}
	cmd.SilenceUsage = true
	registerRunFlags(cmd, &args.runArgs)
	cmd.Flags().StringVar(&args.feedback, "feedback", "", "Explicit refinement feedback; skips PR comment fetching")
	cmd.Flags().StringVar(&args.feedbackFile, "feedback-file", "", "Read explicit refinement feedback from file (use - for stdin)")
	cmd.Flags().StringVar(&args.prRef, "pr", "", "PR number or URL (overrides PR auto-discovery)")
	cmd.Flags().BoolVar(&args.noPoll, "no-poll", false, "Run one validation pass and exit at the next review gate")
	return cmd
}

func refine(ctx context.Context, app *cliapp.App, args refineArgs) error {
	if args.feedback != "" && args.feedbackFile != "" {
		return fmt.Errorf("--feedback and --feedback-file are mutually exclusive")
	}
	if args.feedbackFile != "" {
		data, err := readFileOrStdin(args.feedbackFile)
		if err != nil {
			return fmt.Errorf("read feedback file: %w", err)
		}
		args.feedback = strings.TrimSpace(string(data))
		if args.feedback == "" {
			return fmt.Errorf("feedback file is empty")
		}
	}
	if args.descriptionFile != "" {
		if args.issueID != "" {
			return fmt.Errorf("--issue and --description-file are mutually exclusive")
		}
		data, err := readFileOrStdin(args.descriptionFile)
		if err != nil {
			return fmt.Errorf("read description file: %w", err)
		}
		args.description = strings.TrimSpace(string(data))
		if args.description == "" {
			return fmt.Errorf("description file is empty")
		}
	}
	if args.issueID == "" && args.description == "" {
		return fmt.Errorf("refine requires --issue or --description-file")
	}

	cfg, err := loadRunConfig(args.runArgs)
	if err != nil {
		return err
	}
	issueTracker, err := createTracker(cfg, args.issueID)
	if err != nil {
		return err
	}

	var issue *tracker.Issue
	if args.description != "" {
		lt, ok := issueTracker.(*local.Tracker)
		if !ok {
			return fmt.Errorf("--description-file requires local tracker (got %T)", issueTracker)
		}
		issue, err = findExistingLocalIssueByDescription(ctx, lt, args.description)
		if err != nil {
			return err
		}
	} else {
		app.Logger.Info("fetching issue", "identifier", args.issueID)
		issue, err = issueTracker.FetchIssue(ctx, args.issueID)
		if err != nil {
			return fmt.Errorf("fetch issue: %w", err)
		}
	}

	if args.feedback == "" {
		if issueFeedback, ok, err := jiradozer.RefineFeedbackFromIssueComment(ctx, issueTracker, issue.ID); err != nil {
			app.Logger.Warn("failed to inspect issue comments for refine feedback", "error", err)
		} else if ok {
			args.feedback = issueFeedback
		}
	}

	if app.Renderer != nil {
		defer app.Renderer.Reset()
	}
	wfOpts := jiradozer.RefineOptions{
		Issue:    issue,
		Tracker:  issueTracker,
		Config:   cfg,
		Logger:   app.Logger,
		Renderer: app.Renderer,
		Feedback: args.feedback,
		PRRef:    args.prRef,
		WorkDir:  args.workDir,
		NoPoll:   args.noPoll,
	}
	return jiradozer.RunRefine(ctx, wfOpts)
}

func findExistingLocalIssueByDescription(ctx context.Context, lt *local.Tracker, description string) (*tracker.Issue, error) {
	issues, err := lt.ListIssues(ctx, tracker.IssueFilter{})
	if err != nil {
		return nil, fmt.Errorf("list local issues: %w", err)
	}
	description = strings.TrimSpace(description)
	for _, issue := range issues {
		if issue.Description == nil {
			continue
		}
		if strings.TrimSpace(*issue.Description) == description {
			return issue, nil
		}
	}
	return nil, fmt.Errorf("no existing local issue found for --description-file; run jiradozer run --description-file first or use --issue LOCAL-N")
}
