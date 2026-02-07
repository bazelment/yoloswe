// Package claude provides a Go SDK for interacting with the Claude CLI.
//
// The SDK manages the lifecycle of Claude CLI processes and provides
// both synchronous and event-driven APIs for sending messages and
// receiving streaming responses.
//
// # Quick Start
//
// For simple one-shot queries:
//
//	result, err := claude.Query(ctx, "What is 2+2?")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Printf("Response: %s\nCost: $%.6f\n", result.Text, result.Usage.CostUSD)
//
// For simple synchronous usage:
//
//	session := claude.NewSession(
//	    claude.WithModel("haiku"),
//	    claude.WithPermissionMode(claude.PermissionModeBypass),
//	)
//
//	if err := session.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer session.Stop()
//
//	// Send message and collect events until turn completes
//	session.SendMessage(ctx, "What is 2+2?")
//	for event := range session.Events() {
//	    switch e := event.(type) {
//	    case claude.TurnCompleteEvent:
//	        fmt.Printf("Success: %v, Cost: $%.6f\n", e.Success, e.Usage.CostUSD)
//	        return
//	    }
//	}
//
// # Event-Driven Usage
//
// For streaming responses with real-time text output:
//
//	session := claude.NewSession(
//	    claude.WithModel("haiku"),
//	    claude.WithPermissionMode(claude.PermissionModeBypass),
//	)
//
//	if err := session.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer session.Stop()
//
//	// Send message
//	session.SendMessage(ctx, "Write a haiku about Go programming")
//
//	// Collect events until turn completes
//	for event := range session.Events() {
//	    switch e := event.(type) {
//	    case claude.ReadyEvent:
//	        fmt.Printf("Session ready: %s\n", e.Info.SessionID)
//	    case claude.TextEvent:
//	        fmt.Print(e.Text) // Stream text as it arrives
//	    case claude.ToolStartEvent:
//	        fmt.Printf("\n[Tool: %s started]\n", e.Name)
//	    case claude.TurnCompleteEvent:
//	        fmt.Printf("\n[Cost: $%.6f]\n", e.Usage.CostUSD)
//	        return // Exit loop when turn completes
//	    }
//	}
//
// # Permission Handling
//
// When using PermissionModeDefault, you can handle permission requests:
//
//	handler := claude.PermissionHandlerFunc(func(
//	    ctx context.Context,
//	    req *claude.PermissionRequest,
//	) (*claude.PermissionResponse, error) {
//	    // Auto-allow Read tool
//	    if req.ToolName == "Read" {
//	        return &claude.PermissionResponse{
//	            Behavior: claude.PermissionAllow,
//	        }, nil
//	    }
//	    // Deny others
//	    return &claude.PermissionResponse{
//	        Behavior: claude.PermissionDeny,
//	        Message:  "Not allowed",
//	    }, nil
//	})
//
//	session := claude.NewSession(
//	    claude.WithPermissionMode(claude.PermissionModeDefault),
//	    claude.WithPermissionHandler(handler),
//	)
//
// # Session Recording
//
// Enable recording to save session messages for debugging or replay:
//
//	session := claude.NewSession(
//	    claude.WithRecording("./recordings"),
//	)
//
//	// ... run session ...
//
//	session.Stop()
//
//	// Get recording info
//	recording := session.Recording()
//	fmt.Printf("Total cost: $%.6f\n", recording.TotalCost())
//
//	// Load a previous recording
//	loaded, _ := claude.LoadRecording("./recordings/session-abc123")
package claude
