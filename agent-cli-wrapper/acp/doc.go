// Package acp provides a Go SDK for the Agent Client Protocol (ACP).
//
// ACP is an open standard for communication between code editors and
// AI coding agents, using JSON-RPC 2.0 over stdio. This SDK wraps
// any ACP-compatible agent binary (Gemini CLI, Claude Code, Goose, etc.)
// as a subprocess and provides a high-level Go API.
//
// # Basic Usage
//
// For simple one-shot queries with Gemini CLI:
//
//	client := acp.NewClient(
//	    acp.WithBinaryPath("gemini"),
//	    acp.WithBinaryArgs("--experimental-acp"),
//	    acp.WithClientName("my-app"),
//	)
//
//	if err := client.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Stop()
//
//	session, err := client.NewSession(ctx, acp.WithSessionCWD("/path/to/project"))
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	result, err := session.Prompt(ctx, "What files are in this directory?")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Println(result.FullText)
//
// # Multi-Turn Conversations
//
// ACP sessions maintain conversation context across prompts:
//
//	session, _ := client.NewSession(ctx, acp.WithSessionCWD("/path/to/project"))
//
//	result1, _ := session.Prompt(ctx, "What files are here?")
//	fmt.Println(result1.FullText)
//
//	result2, _ := session.Prompt(ctx, "Summarize the main.go file")
//	fmt.Println(result2.FullText)
//
// # Streaming Events
//
//	go func() {
//	    for event := range client.Events() {
//	        switch e := event.(type) {
//	        case acp.TextDeltaEvent:
//	            fmt.Print(e.Delta)
//	        case acp.ToolCallStartEvent:
//	            fmt.Printf("\n[tool: %s]\n", e.ToolName)
//	        case acp.TurnCompleteEvent:
//	            fmt.Println("\nDone!")
//	        }
//	    }
//	}()
//
// # Agent Compatibility
//
// This SDK works with any ACP-compatible agent binary:
//   - Gemini CLI: WithBinaryPath("gemini"), WithBinaryArgs("--experimental-acp")
//   - Claude Code: WithBinaryPath("claude"), WithBinaryArgs("--acp")
//   - Other agents: Configure BinaryPath and BinaryArgs accordingly
package acp
