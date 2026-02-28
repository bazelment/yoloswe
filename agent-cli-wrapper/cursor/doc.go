// Package cursor provides a Go SDK for interacting with the Cursor Agent CLI.
//
// The SDK manages the lifecycle of Cursor Agent CLI processes and provides
// both synchronous and streaming APIs for one-shot prompt execution.
// Unlike the Claude SDK, Cursor operates in one-shot mode only (no
// interactive stdin conversation).
//
// # Quick Start
//
// For simple one-shot queries:
//
//	result, err := cursor.Query(ctx, "What is 2+2?")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Printf("Response: %s\n", result.Text)
//
// # Streaming Usage
//
// For streaming responses with real-time text output:
//
//	events, err := cursor.QueryStream(ctx, "Write a haiku about Go programming")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	for event := range events {
//	    switch e := event.(type) {
//	    case cursor.TextEvent:
//	        fmt.Print(e.Text)
//	    case cursor.ToolStartEvent:
//	        fmt.Printf("\n[Tool: %s started]\n", e.Name)
//	    case cursor.TurnCompleteEvent:
//	        fmt.Printf("\n[Done: success=%v]\n", e.Success)
//	    }
//	}
//
// # Session Usage
//
// For lower-level control:
//
//	session := cursor.NewSession("What is 2+2?",
//	    cursor.WithModel("cursor-default"),
//	    cursor.WithWorkDir("/path/to/project"),
//	)
//	if err := session.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer session.Stop()
//
//	for event := range session.Events() {
//	    switch e := event.(type) {
//	    case cursor.ReadyEvent:
//	        fmt.Printf("Session: %s\n", e.SessionID)
//	    case cursor.TextEvent:
//	        fmt.Print(e.Text)
//	    case cursor.TurnCompleteEvent:
//	        fmt.Printf("\nDuration: %dms\n", e.DurationMs)
//	    }
//	}
package cursor
