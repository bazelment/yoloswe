// Package checkpoint provides session state persistence for error recovery.
//
// The checkpoint package enables the multi-agent system to save progress during
// mission execution and resume from that point after a failure or interruption.
//
// # Checkpoint Structure
//
// A checkpoint captures:
//   - Current phase (designing, building, reviewing)
//   - Iteration count
//   - Design/Build/Review responses
//   - Files created and modified
//   - Accumulated cost
//   - Error information if failed
//
// # Usage
//
// Create a checkpoint manager:
//
//	mgr := checkpoint.NewManager(sessionDir, sessionID)
//	mgr.SetMission("Build a hello world CLI")
//
// Record phase transitions:
//
//	mgr.StartDesign()
//	mgr.CompleteDesign(response, cost)
//	mgr.StartBuild()
//	mgr.CompleteBuild(response, cost)
//
// Check if a session can be resumed:
//
//	cp, _ := checkpoint.Load(sessionDir, sessionID)
//	if cp != nil && cp.CanResume() {
//	    resumePhase := cp.ResumePhase()
//	    // Resume from the appropriate phase
//	}
//
// # File Location
//
// Checkpoints are stored at:
//
//	{sessionDir}/{sessionID}/checkpoint.json
package checkpoint
