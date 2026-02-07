// Package main demonstrates the TypedToolRegistry with diverse tool examples.
//
// This demo showcases:
// - Calculator tool with enum operations
// - Text manipulation tool with optional parameters
// - Search tool with array filters and default values
//
// Run with: bazel run //examples/typed_tools:typed_tools
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// CalculatorParams demonstrates enum constraints and numeric operations
type CalculatorParams struct {
	A         float64 `json:"a" jsonschema:"required,description=First number"`
	B         float64 `json:"b" jsonschema:"required,description=Second number"`
	Operation string  `json:"operation" jsonschema:"required,description=Math operation to perform,enum=add,enum=subtract,enum=multiply,enum=divide"`
}

// TextManipParams demonstrates string operations with optional prefix
type TextManipParams struct {
	Text      string `json:"text" jsonschema:"required,description=Text to manipulate"`
	Operation string `json:"operation" jsonschema:"required,description=Operation to perform,enum=reverse,enum=uppercase,enum=lowercase,enum=title"`
	Prefix    string `json:"prefix,omitempty" jsonschema:"description=Optional prefix to add to result"`
}

// SearchParams demonstrates arrays and default values
type SearchParams struct {
	Query      string   `json:"query" jsonschema:"required,description=Search query string"`
	MaxResults int      `json:"max_results,omitempty" jsonschema:"description=Maximum number of results,default=10,minimum=1,maximum=100"`
	Filters    []string `json:"filters,omitempty" jsonschema:"description=Filter criteria (e.g. language type year)"`
	SortBy     string   `json:"sort_by,omitempty" jsonschema:"description=Sort order,enum=relevance,enum=date,enum=popularity,default=relevance"`
}

func calculatorTool(ctx context.Context, p CalculatorParams) (string, error) {
	var result float64
	var symbol string

	switch p.Operation {
	case "add":
		result = p.A + p.B
		symbol = "+"
	case "subtract":
		result = p.A - p.B
		symbol = "-"
	case "multiply":
		result = p.A * p.B
		symbol = "×"
	case "divide":
		if p.B == 0 {
			return "", fmt.Errorf("division by zero")
		}
		result = p.A / p.B
		symbol = "÷"
	default:
		return "", fmt.Errorf("unknown operation: %s", p.Operation)
	}

	return fmt.Sprintf("%g %s %g = %g", p.A, symbol, p.B, result), nil
}

func textManipTool(ctx context.Context, p TextManipParams) (string, error) {
	var result string

	switch p.Operation {
	case "reverse":
		runes := []rune(p.Text)
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		result = string(runes)
	case "uppercase":
		result = strings.ToUpper(p.Text)
	case "lowercase":
		result = strings.ToLower(p.Text)
	case "title":
		result = strings.Title(p.Text)
	default:
		return "", fmt.Errorf("unknown operation: %s", p.Operation)
	}

	if p.Prefix != "" {
		result = p.Prefix + result
	}

	return result, nil
}

func searchTool(ctx context.Context, p SearchParams) (string, error) {
	maxResults := p.MaxResults
	if maxResults == 0 {
		maxResults = 10
	}

	sortBy := p.SortBy
	if sortBy == "" {
		sortBy = "relevance"
	}

	result := fmt.Sprintf("🔍 Searching for: \"%s\"\n", p.Query)
	result += fmt.Sprintf("📊 Max results: %d\n", maxResults)
	result += fmt.Sprintf("🔀 Sort by: %s\n", sortBy)

	if len(p.Filters) > 0 {
		result += fmt.Sprintf("🏷️  Filters: %s\n", strings.Join(p.Filters, ", "))
	}

	result += "\n✅ Search configured successfully! (This is a demo, no actual search performed)"

	return result, nil
}

func main() {
	fmt.Println("=== TypedToolRegistry Demo ===")
	fmt.Println()
	fmt.Println("This demo showcases type-safe tool registration using Go generics.")
	fmt.Println("Three tools are registered with diverse parameter types:")
	fmt.Println()
	fmt.Println("1. Calculator: add, subtract, multiply, divide (enum operations)")
	fmt.Println("2. Text Manipulation: reverse, uppercase, lowercase, title (with optional prefix)")
	fmt.Println("3. Search: query with max_results, filters array, and sort_by options")
	fmt.Println()

	// Create the TypedToolRegistry
	registry := claude.NewTypedToolRegistry()

	// Register calculator tool
	claude.AddTool(registry, "calculator",
		"Perform arithmetic operations on two numbers. Supports add, subtract, multiply, and divide.",
		calculatorTool)

	// Register text manipulation tool
	claude.AddTool(registry, "text_manip",
		"Manipulate text with various operations. Can reverse, uppercase, lowercase, or title-case text, with optional prefix.",
		textManipTool)

	// Register search tool
	claude.AddTool(registry, "search",
		"Search with customizable parameters. Supports filters, result limits, and sorting options.",
		searchTool)

	fmt.Println("✅ Registered 3 typed tools successfully!")
	fmt.Println()

	// Display the tool definitions
	tools := registry.Tools()
	fmt.Printf("📋 Registered Tools (%d total):\n\n", len(tools))
	for i, tool := range tools {
		fmt.Printf("%d. %s\n", i+1, tool.Name)
		fmt.Printf("   Description: %s\n", tool.Description)
		fmt.Printf("   Schema: %s\n", string(tool.InputSchema))
		fmt.Println()
	}

	// Test the tools directly
	fmt.Println("=== Testing Tools Directly ===")
	fmt.Println()

	ctx := context.Background()

	// Test calculator
	fmt.Println("1. Testing calculator tool:")
	calcResult, err := calculatorTool(ctx, CalculatorParams{A: 17, B: 25, Operation: "add"})
	if err != nil {
		fmt.Printf("   ❌ Error: %v\n", err)
	} else {
		fmt.Printf("   ✅ Result: %s\n", calcResult)
	}
	fmt.Println()

	// Test text manipulation
	fmt.Println("2. Testing text_manip tool:")
	textResult, err := textManipTool(ctx, TextManipParams{
		Text:      "hello world",
		Operation: "reverse",
		Prefix:    "Reversed: ",
	})
	if err != nil {
		fmt.Printf("   ❌ Error: %v\n", err)
	} else {
		fmt.Printf("   ✅ Result: %s\n", textResult)
	}
	fmt.Println()

	// Test search
	fmt.Println("3. Testing search tool:")
	searchResult, err := searchTool(ctx, SearchParams{
		Query:      "golang generics",
		MaxResults: 5,
		Filters:    []string{"language:go", "year:2022"},
		SortBy:     "popularity",
	})
	if err != nil {
		fmt.Printf("   ❌ Error: %v\n", err)
	} else {
		fmt.Printf("   ✅ Result:\n")
		for _, line := range strings.Split(searchResult, "\n") {
			fmt.Printf("      %s\n", line)
		}
	}
	fmt.Println()

	// Start an interactive session
	fmt.Println("=== Starting Interactive Claude Session ===")
	fmt.Println()

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithSDKTools("demo-tools", registry),
	)

	if err := session.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start session: %v\n", err)
		os.Exit(1)
	}
	defer session.Stop()

	fmt.Println("💬 Session started! Sending a test prompt...")
	fmt.Println()

	prompt := `Please demonstrate all three tools:
1. Use calculator to add 42 and 58
2. Use text_manip to reverse "TypeScript" with prefix "Result: "
3. Use search to find "Go generics tutorial" with max 3 results and filters ["language:go", "beginner"]

Just show me the results of each tool.`

	_, err = session.SendMessage(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to send message: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("📨 Prompt sent, collecting response...")
	fmt.Println()

	// Collect and display response
	for event := range session.Events() {
		switch e := event.(type) {
		case claude.TextEvent:
			fmt.Print(e.Text)
		case claude.TurnCompleteEvent:
			fmt.Println()
			fmt.Println()
			if e.Success {
				fmt.Printf("✅ Turn completed successfully! Cost: $%.6f\n", e.Usage.CostUSD)
			} else {
				fmt.Printf("❌ Turn failed: %v\n", e.Error)
			}
			return
		case claude.ErrorEvent:
			fmt.Fprintf(os.Stderr, "Error: %v\n", e.Error)
			os.Exit(1)
		}
	}
}
