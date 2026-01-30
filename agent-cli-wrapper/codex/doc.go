// Package codex provides a Go SDK for interacting with the Codex CLI app-server.
//
// The SDK provides both low-level JSON-RPC types and high-level abstractions
// for common operations like creating threads, sending messages, and handling
// streaming responses.
//
// # Basic Usage
//
// For simple one-shot queries:
//
//	client := codex.NewClient(
//	    codex.WithClientName("my-app"),
//	    codex.WithClientVersion("1.0.0"),
//	)
//
//	if err := client.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Stop()
//
//	response, err := client.Ask(ctx, "What is 2+2?")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Println(response)
//
// # Multi-Turn Conversations
//
// For conversations with multiple turns:
//
//	thread, err := client.CreateThread(ctx,
//	    codex.WithModel("gpt-4o"),
//	    codex.WithWorkDir("/path/to/project"),
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// Wait for MCP startup
//	for event := range client.Events() {
//	    if e, ok := event.(codex.ThreadReadyEvent); ok && e.ThreadID == thread.ID() {
//	        break
//	    }
//	}
//
//	// First turn
//	result1, err := thread.Ask(ctx, "What files are in this directory?")
//	fmt.Println(result1.FullText)
//
//	// Follow-up turn
//	result2, err := thread.Ask(ctx, "Summarize the main.go file")
//	fmt.Println(result2.FullText)
//
// # Streaming Events
//
// For real-time streaming of responses:
//
//	thread, _ := client.CreateThread(ctx)
//	thread.SendMessage(ctx, "Write a haiku about Go")
//
//	for event := range client.Events() {
//	    switch e := event.(type) {
//	    case codex.TextDeltaEvent:
//	        fmt.Print(e.Delta) // Stream text as it arrives
//	    case codex.TurnCompletedEvent:
//	        fmt.Println("\nDone!")
//	        return
//	    }
//	}
//
// # Configuration Options
//
// Client-level options:
//   - WithCodexPath: Custom path to codex binary
//   - WithClientName: Client identifier
//   - WithClientVersion: Client version string
//   - WithEventBufferSize: Event channel buffer size
//   - WithStderrHandler: Handler for app-server stderr
//   - WithApprovalHandler: Handler for tool approval requests
//
// Thread-level options:
//   - WithModel: Model to use (e.g., "gpt-4o")
//   - WithModelProvider: Model provider
//   - WithWorkDir: Working directory
//   - WithApprovalPolicy: Tool approval policy
//   - WithSandbox: Sandbox configuration
//
// Turn-level options:
//   - WithTurnModel: Override model for this turn
//   - WithEffort: Reasoning effort level
//   - WithSummary: Context for the turn
//   - WithOutputSchema: Structured output schema
package codex
