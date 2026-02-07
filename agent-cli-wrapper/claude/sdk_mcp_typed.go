package claude

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
	"github.com/invopop/jsonschema"
)

// TypedToolRegistry implements SDKToolHandler using Go generics for type-safe tool registration.
// It automatically generates JSON schemas from struct tags and eliminates manual unmarshaling.
type TypedToolRegistry struct {
	tools []toolRegistration
}

// toolRegistration stores a single tool's metadata and type-erased handler.
type toolRegistration struct {
	name        string
	description string
	schema      json.RawMessage
	invoke      func(context.Context, json.RawMessage) (*protocol.MCPToolCallResult, error)
}

// NewTypedToolRegistry creates a new empty TypedToolRegistry.
func NewTypedToolRegistry() *TypedToolRegistry {
	return &TypedToolRegistry{
		tools: make([]toolRegistration, 0),
	}
}

// AddTool is a helper function that registers a type-safe tool handler using generics.
// It returns the modified registry for method chaining.
//
// The generic type parameter T should be a struct with json and jsonschema struct tags.
//
// Example:
//
//	type EchoParams struct {
//	    Text string `json:"text" jsonschema:"required,description=Text to echo back"`
//	}
//
//	registry := NewTypedToolRegistry()
//	AddTool(registry, "echo", "Echo back the input text",
//	    func(ctx context.Context, params EchoParams) (string, error) {
//	        return fmt.Sprintf("Echo: %s", params.Text), nil
//	    })
func AddTool[T any](
	registry *TypedToolRegistry,
	name, description string,
	handler func(context.Context, T) (string, error),
) *TypedToolRegistry {
	schema := generateSchema[T]()

	// Type-erase the handler by wrapping it in a closure
	invoke := func(ctx context.Context, args json.RawMessage) (*protocol.MCPToolCallResult, error) {
		var params T
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, fmt.Errorf("invalid arguments for tool %s: %w", name, err)
		}

		result, err := handler(ctx, params)
		if err != nil {
			return &protocol.MCPToolCallResult{
				Content: []protocol.MCPContentItem{
					{Type: "text", Text: err.Error()},
				},
				IsError: true,
			}, nil
		}

		return &protocol.MCPToolCallResult{
			Content: []protocol.MCPContentItem{
				{Type: "text", Text: result},
			},
		}, nil
	}

	registry.tools = append(registry.tools, toolRegistration{
		name:        name,
		description: description,
		schema:      schema,
		invoke:      invoke,
	})

	return registry
}

// Tools implements SDKToolHandler interface.
func (r *TypedToolRegistry) Tools() []protocol.MCPToolDefinition {
	result := make([]protocol.MCPToolDefinition, len(r.tools))
	for i, tool := range r.tools {
		result[i] = protocol.MCPToolDefinition{
			Name:        tool.name,
			Description: tool.description,
			InputSchema: tool.schema,
		}
	}
	return result
}

// HandleToolCall implements SDKToolHandler interface.
func (r *TypedToolRegistry) HandleToolCall(
	ctx context.Context,
	name string,
	args json.RawMessage,
) (*protocol.MCPToolCallResult, error) {
	for _, tool := range r.tools {
		if tool.name == name {
			return tool.invoke(ctx, args)
		}
	}

	return &protocol.MCPToolCallResult{
		Content: []protocol.MCPContentItem{
			{Type: "text", Text: fmt.Sprintf("Unknown tool: %s", name)},
		},
		IsError: true,
	}, nil
}

// generateSchema uses reflection to create a JSON schema from a Go struct type.
// It uses the invopop/jsonschema library to parse jsonschema struct tags.
func generateSchema[T any]() json.RawMessage {
	reflector := &jsonschema.Reflector{
		DoNotReference: true, // Inline all definitions instead of using $ref
		ExpandedStruct: true, // Don't use $ref for struct types
	}

	var zero T
	schema := reflector.Reflect(zero)

	bytes, err := json.Marshal(schema)
	if err != nil {
		// This should never happen with valid types
		panic(fmt.Sprintf("failed to generate schema for type %T: %v", zero, err))
	}

	return json.RawMessage(bytes)
}

// Verify TypedToolRegistry implements SDKToolHandler
var _ SDKToolHandler = (*TypedToolRegistry)(nil)
