package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/multiagent/checkpoint"
)

var statusCmd = &cobra.Command{
	Use:   "status <session-id>",
	Short: "Show session status",
	Long: `Display the current status of a swarm session.

Shows checkpoint information including phase, cost, files modified,
and whether the session can be resumed.

Example:
  swarm status swarm-1234567890`,
	Args: cobra.ExactArgs(1),
	RunE: runStatusCmd,
}

var statusJSON bool

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output status as JSON")

	rootCmd.AddCommand(statusCmd)
}

func runStatusCmd(cmd *cobra.Command, args []string) error {
	sessionID := args[0]
	sessDir := resolveSessionDir()
	sessionPath := filepath.Join(sessDir, sessionID)

	// Check if session directory exists
	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		return fmt.Errorf("session directory not found: %s", sessionPath)
	}

	// Load checkpoint
	cp, err := checkpoint.Load(sessDir, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load checkpoint: %w", err)
	}

	if statusJSON {
		return printStatusJSON(sessionID, sessionPath, cp)
	}

	return printStatusText(sessionID, sessionPath, cp)
}

func printStatusJSON(sessionID, sessionPath string, cp *checkpoint.Checkpoint) error {
	status := map[string]interface{}{
		"session_id":   sessionID,
		"session_path": sessionPath,
	}

	if cp != nil {
		status["checkpoint"] = cp
		status["can_resume"] = cp.CanResume()
		if cp.CanResume() {
			status["resume_phase"] = cp.ResumePhase()
		}
	} else {
		status["checkpoint"] = nil
	}

	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func printStatusText(sessionID, sessionPath string, cp *checkpoint.Checkpoint) error {
	fmt.Printf("Session: %s\n", sessionID)
	fmt.Printf("Path: %s\n", sessionPath)
	fmt.Println()

	if cp == nil {
		fmt.Println("No checkpoint found.")
		return nil
	}

	fmt.Println("=== Checkpoint ===")
	fmt.Printf("Phase: %s\n", cp.Phase)
	fmt.Printf("Mission: %s\n", truncate(cp.Mission, 80))
	fmt.Printf("Last Updated: %s\n", cp.LastUpdated.Format("2006-01-02 15:04:05"))
	fmt.Printf("Total Cost: $%.4f\n", cp.TotalCost)
	fmt.Printf("Iterations: %d\n", cp.IterationCount)

	if cp.LastError != "" {
		fmt.Printf("Last Error: %s\n", cp.LastError)
	}

	fmt.Println()

	// Resume status
	if cp.CanResume() {
		fmt.Printf("Can Resume: Yes (from %s phase)\n", cp.ResumePhase())
	} else {
		fmt.Println("Can Resume: No")
	}

	// Files
	if len(cp.FilesCreated) > 0 {
		fmt.Println("\nFiles Created:")
		for _, f := range cp.FilesCreated {
			fmt.Printf("  - %s\n", f)
		}
	}

	if len(cp.FilesModified) > 0 {
		fmt.Println("\nFiles Modified:")
		for _, f := range cp.FilesModified {
			fmt.Printf("  - %s\n", f)
		}
	}

	// Agent sessions
	fmt.Println("\n=== Agent Sessions ===")
	agents := []string{"orchestrator", "planner", "designer", "builder", "reviewer"}
	for _, agent := range agents {
		agentPath := filepath.Join(sessionPath, agent)
		if info, err := os.Stat(agentPath); err == nil && info.IsDir() {
			// Count task directories for ephemeral agents
			entries, _ := os.ReadDir(agentPath)
			taskCount := 0
			for _, e := range entries {
				if e.IsDir() && len(e.Name()) > 4 && e.Name()[:4] == "task" {
					taskCount++
				}
			}
			if taskCount > 0 {
				fmt.Printf("  %s: %d tasks\n", agent, taskCount)
			} else {
				fmt.Printf("  %s: active\n", agent)
			}
		}
	}

	return nil
}
