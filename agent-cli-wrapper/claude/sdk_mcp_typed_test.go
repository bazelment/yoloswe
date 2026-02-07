package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test parameter types for various scenarios
type SimpleParams struct {
	Text string `json:"text" jsonschema:"required,description=Simple text field"`
}

type NumberParams struct {
	A float64 `json:"a" jsonschema:"required,description=First number"`
	B float64 `json:"b" jsonschema:"required,description=Second number"`
}

type ComplexParams struct {
	Query      string   `json:"query" jsonschema:"required,description=Search query"`
	Filters    []string `json:"filters,omitempty" jsonschema:"description=Filter criteria"`
	MaxResults int      `json:"max_results,omitempty" jsonschema:"description=Maximum results,default=10"`
	Enabled    bool     `json:"enabled,omitempty" jsonschema:"description=Enable feature"`
}

type EnumParams struct {
	Operation string `json:"operation" jsonschema:"required,description=Operation type,enum=add,enum=subtract,enum=multiply,enum=divide"`
}

func TestTypedToolRegistry_SchemaGeneration(t *testing.T) {
	t.Run("simple string parameter", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "simple", "Simple tool",
			func(ctx context.Context, p SimpleParams) (string, error) {
				return p.Text, nil
			})

		tools := registry.Tools()
		require.Len(t, tools, 1)
		assert.Equal(t, "simple", tools[0].Name)
		assert.Equal(t, "Simple tool", tools[0].Description)

		// Verify schema structure
		var schema map[string]interface{}
		err := json.Unmarshal(tools[0].InputSchema, &schema)
		require.NoError(t, err)

		assert.Equal(t, "object", schema["type"])
		props := schema["properties"].(map[string]interface{})
		assert.Contains(t, props, "text")
	})

	t.Run("number parameters", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "add", "Add numbers",
			func(ctx context.Context, p NumberParams) (string, error) {
				return fmt.Sprintf("%g", p.A+p.B), nil
			})

		tools := registry.Tools()
		require.Len(t, tools, 1)

		var schema map[string]interface{}
		err := json.Unmarshal(tools[0].InputSchema, &schema)
		require.NoError(t, err)

		props := schema["properties"].(map[string]interface{})
		assert.Contains(t, props, "a")
		assert.Contains(t, props, "b")
	})

	t.Run("complex parameters with arrays and optional fields", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "search", "Search with filters",
			func(ctx context.Context, p ComplexParams) (string, error) {
				return fmt.Sprintf("Searching: %s", p.Query), nil
			})

		tools := registry.Tools()
		require.Len(t, tools, 1)

		var schema map[string]interface{}
		err := json.Unmarshal(tools[0].InputSchema, &schema)
		require.NoError(t, err)

		props := schema["properties"].(map[string]interface{})
		assert.Contains(t, props, "query")
		assert.Contains(t, props, "max_results")
		assert.Contains(t, props, "filters")
		assert.Contains(t, props, "enabled")

		// Verify filters is an array
		filters := props["filters"].(map[string]interface{})
		assert.Equal(t, "array", filters["type"])
	})
}

func TestTypedToolRegistry_HandlerInvocation(t *testing.T) {
	t.Run("successful handler call", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "echo", "Echo text",
			func(ctx context.Context, p SimpleParams) (string, error) {
				return fmt.Sprintf("Echo: %s", p.Text), nil
			})

		result, err := registry.HandleToolCall(
			context.Background(),
			"echo",
			json.RawMessage(`{"text": "hello world"}`),
		)

		require.NoError(t, err)
		assert.False(t, result.IsError)
		require.Len(t, result.Content, 1)
		assert.Equal(t, "Echo: hello world", result.Content[0].Text)
	})

	t.Run("handler returns error", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "failing", "Failing tool",
			func(ctx context.Context, p SimpleParams) (string, error) {
				return "", fmt.Errorf("intentional error: %s", p.Text)
			})

		result, err := registry.HandleToolCall(
			context.Background(),
			"failing",
			json.RawMessage(`{"text": "test"}`),
		)

		require.NoError(t, err)
		assert.True(t, result.IsError)
		require.Len(t, result.Content, 1)
		assert.Contains(t, result.Content[0].Text, "intentional error: test")
	})

	t.Run("number calculation", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "add", "Add numbers",
			func(ctx context.Context, p NumberParams) (string, error) {
				sum := p.A + p.B
				return fmt.Sprintf("%g", sum), nil
			})

		result, err := registry.HandleToolCall(
			context.Background(),
			"add",
			json.RawMessage(`{"a": 17, "b": 25}`),
		)

		require.NoError(t, err)
		assert.False(t, result.IsError)
		require.Len(t, result.Content, 1)
		assert.Equal(t, "42", result.Content[0].Text)
	})
}

func TestTypedToolRegistry_InvalidJSON(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "test", "Test tool",
			func(ctx context.Context, p SimpleParams) (string, error) {
				return p.Text, nil
			})

		_, err := registry.HandleToolCall(
			context.Background(),
			"test",
			json.RawMessage(`{invalid json}`),
		)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid arguments")
	})

	t.Run("missing required field", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "test", "Test tool",
			func(ctx context.Context, p SimpleParams) (string, error) {
				return p.Text, nil
			})

		// Missing "text" field - will unmarshal to empty string
		_, err := registry.HandleToolCall(
			context.Background(),
			"test",
			json.RawMessage(`{}`),
		)

		// JSON unmarshaling succeeds even with missing fields (Go default behavior)
		// The handler receives empty values
		require.NoError(t, err)
	})

	t.Run("wrong field type", func(t *testing.T) {
		registry := NewTypedToolRegistry()
		AddTool(registry, "add", "Add numbers",
			func(ctx context.Context, p NumberParams) (string, error) {
				return fmt.Sprintf("%g", p.A+p.B), nil
			})

		// Passing string instead of number
		_, err := registry.HandleToolCall(
			context.Background(),
			"add",
			json.RawMessage(`{"a": "not a number", "b": 5}`),
		)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid arguments")
	})
}

func TestTypedToolRegistry_MultipleTools(t *testing.T) {
	registry := NewTypedToolRegistry()

	// Add multiple tools
	AddTool(registry, "echo", "Echo text",
		func(ctx context.Context, p SimpleParams) (string, error) {
			return fmt.Sprintf("Echo: %s", p.Text), nil
		})

	AddTool(registry, "add", "Add numbers",
		func(ctx context.Context, p NumberParams) (string, error) {
			return fmt.Sprintf("%g", p.A+p.B), nil
		})

	AddTool(registry, "search", "Search",
		func(ctx context.Context, p ComplexParams) (string, error) {
			return fmt.Sprintf("Search: %s", p.Query), nil
		})

	t.Run("all tools listed", func(t *testing.T) {
		tools := registry.Tools()
		require.Len(t, tools, 3)

		names := make([]string, len(tools))
		for i, tool := range tools {
			names[i] = tool.Name
		}

		assert.Contains(t, names, "echo")
		assert.Contains(t, names, "add")
		assert.Contains(t, names, "search")
	})

	t.Run("each tool works independently", func(t *testing.T) {
		// Test echo
		r1, err := registry.HandleToolCall(
			context.Background(),
			"echo",
			json.RawMessage(`{"text": "hello"}`),
		)
		require.NoError(t, err)
		assert.Equal(t, "Echo: hello", r1.Content[0].Text)

		// Test add
		r2, err := registry.HandleToolCall(
			context.Background(),
			"add",
			json.RawMessage(`{"a": 10, "b": 32}`),
		)
		require.NoError(t, err)
		assert.Equal(t, "42", r2.Content[0].Text)

		// Test search
		r3, err := registry.HandleToolCall(
			context.Background(),
			"search",
			json.RawMessage(`{"query": "golang generics"}`),
		)
		require.NoError(t, err)
		assert.Equal(t, "Search: golang generics", r3.Content[0].Text)
	})
}

func TestTypedToolRegistry_UnknownTool(t *testing.T) {
	registry := NewTypedToolRegistry()
	AddTool(registry, "echo", "Echo text",
		func(ctx context.Context, p SimpleParams) (string, error) {
			return p.Text, nil
		})

	result, err := registry.HandleToolCall(
		context.Background(),
		"nonexistent",
		json.RawMessage(`{}`),
	)

	require.NoError(t, err)
	assert.True(t, result.IsError)
	require.Len(t, result.Content, 1)
	assert.Contains(t, result.Content[0].Text, "Unknown tool: nonexistent")
}

func TestTypedToolRegistry_RequiredFields(t *testing.T) {
	type RequiredFieldParams struct {
		Name  string `json:"name" jsonschema:"required,description=User name"`
		Email string `json:"email,omitempty" jsonschema:"description=User email"`
	}

	registry := NewTypedToolRegistry()
	AddTool(registry, "create_user", "Create user",
		func(ctx context.Context, p RequiredFieldParams) (string, error) {
			if p.Name == "" {
				return "", fmt.Errorf("name is required")
			}
			return fmt.Sprintf("Created user: %s (%s)", p.Name, p.Email), nil
		})

	tools := registry.Tools()
	require.Len(t, tools, 1)

	var schema map[string]interface{}
	err := json.Unmarshal(tools[0].InputSchema, &schema)
	require.NoError(t, err)

	// Verify schema has both fields
	props := schema["properties"].(map[string]interface{})
	assert.Contains(t, props, "name")
	assert.Contains(t, props, "email")
}

func TestTypedToolRegistry_EnumValidation(t *testing.T) {
	registry := NewTypedToolRegistry()
	AddTool(registry, "calculate", "Calculate",
		func(ctx context.Context, p EnumParams) (string, error) {
			return fmt.Sprintf("Operation: %s", p.Operation), nil
		})

	tools := registry.Tools()
	require.Len(t, tools, 1)

	var schema map[string]interface{}
	err := json.Unmarshal(tools[0].InputSchema, &schema)
	require.NoError(t, err)

	props := schema["properties"].(map[string]interface{})
	assert.Contains(t, props, "operation")
}

func TestTypedToolRegistry_ContextCancellation(t *testing.T) {
	registry := NewTypedToolRegistry()

	called := false
	AddTool(registry, "slow", "Slow tool",
		func(ctx context.Context, p SimpleParams) (string, error) {
			called = true
			// Check if context is cancelled
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
				return "completed", nil
			}
		})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	result, err := registry.HandleToolCall(
		ctx,
		"slow",
		json.RawMessage(`{"text": "test"}`),
	)

	require.NoError(t, err)
	assert.True(t, called)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "context canceled")
}

func TestTypedToolRegistry_EmptyRegistry(t *testing.T) {
	registry := NewTypedToolRegistry()

	tools := registry.Tools()
	assert.Empty(t, tools)

	result, err := registry.HandleToolCall(
		context.Background(),
		"any",
		json.RawMessage(`{}`),
	)

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "Unknown tool")
}
