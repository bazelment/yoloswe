package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/decide"
)

var (
	nudgeStanding bool
	nudgeLane     string
)

var nudgeCmd = &cobra.Command{
	Use:   "nudge <run-dir> <text>",
	Short: "Queue an operator instruction for the next tick",
	Long: `nudge records a correction or standing rule on disk.

A rule that lives only in a chat message is lost at the next compaction, and the
run's own recurring prompt then keeps relaying the superseded version. A queued
nudge is re-read every tick, so it survives both.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		n := decide.Nudge{Text: args[1], Lane: nudgeLane, Standing: nudgeStanding}
		if err := decide.AppendNudge(args[0], n); err != nil {
			return err
		}
		kind := "one-shot"
		if nudgeStanding {
			kind = "standing"
		}
		fmt.Printf("queued %s nudge: %s\n", kind, n.Text)
		return nil
	},
}

var escalationsCmd = &cobra.Command{
	Use:   "escalations <run-dir>",
	Short: "List questions waiting for a person",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		open, err := decide.OpenEscalations(args[0])
		if err != nil {
			return err
		}
		for _, e := range open {
			fmt.Printf("%s\n  lane: %s\n  raised: %s\n", e.Question, e.Lane, e.Raised)
			for _, ev := range e.Evidence {
				fmt.Println("  evidence:", ev)
			}
			fmt.Printf("  answer with: swarm-queen answer %s %q <your answer>\n\n", args[0], e.ID)
		}
		fmt.Printf("%d open escalation(s)\n", len(open))
		return nil
	},
}

var answerCmd = &cobra.Command{
	Use:   "answer <run-dir> <id> <answer>",
	Short: "Resolve an escalation",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := decide.AnswerEscalation(args[0], args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("answered %s\n", args[1])
		return nil
	},
}

func init() {
	nudgeCmd.Flags().BoolVar(&nudgeStanding, "standing", false,
		"apply on every future tick, not just the next one")
	nudgeCmd.Flags().StringVar(&nudgeLane, "lane", "", "scope the nudge to one lane")
	rootCmd.AddCommand(nudgeCmd, escalationsCmd, answerCmd)
}
