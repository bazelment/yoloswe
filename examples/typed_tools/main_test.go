package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCalculatorTool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   CalculatorParams
		want string
	}{
		{
			name: "adds",
			in:   CalculatorParams{A: 2, B: 3, Operation: "add"},
			want: "2 + 3 = 5",
		},
		{
			name: "subtracts",
			in:   CalculatorParams{A: 7, B: 4, Operation: "subtract"},
			want: "7 - 4 = 3",
		},
		{
			name: "multiplies",
			in:   CalculatorParams{A: 6, B: 5, Operation: "multiply"},
			want: "6 × 5 = 30",
		},
		{
			name: "divides",
			in:   CalculatorParams{A: 9, B: 3, Operation: "divide"},
			want: "9 ÷ 3 = 3",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := calculatorTool(context.Background(), tt.in)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestCalculatorToolRejectsInvalidOperations(t *testing.T) {
	t.Parallel()

	_, err := calculatorTool(context.Background(), CalculatorParams{A: 9, B: 0, Operation: "divide"})
	require.ErrorContains(t, err, "division by zero")

	_, err = calculatorTool(context.Background(), CalculatorParams{A: 1, B: 1, Operation: "modulo"})
	require.ErrorContains(t, err, "unknown operation: modulo")
}

func TestTextManipTool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   TextManipParams
		want string
	}{
		{
			name: "reverses text by rune",
			in:   TextManipParams{Text: "go語", Operation: "reverse"},
			want: "語og",
		},
		{
			name: "uppercases text",
			in:   TextManipParams{Text: "hello", Operation: "uppercase"},
			want: "HELLO",
		},
		{
			name: "lowercases text",
			in:   TextManipParams{Text: "HELLO", Operation: "lowercase"},
			want: "hello",
		},
		{
			name: "title cases text",
			in:   TextManipParams{Text: "hello world", Operation: "title"},
			want: "Hello World",
		},
		{
			name: "adds prefix",
			in:   TextManipParams{Text: "hello", Operation: "uppercase", Prefix: "Result: "},
			want: "Result: HELLO",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := textManipTool(context.Background(), tt.in)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestTextManipToolRejectsUnknownOperation(t *testing.T) {
	t.Parallel()

	_, err := textManipTool(context.Background(), TextManipParams{Text: "hello", Operation: "swap"})

	require.ErrorContains(t, err, "unknown operation: swap")
}

func TestSearchToolUsesDefaultsAndOptions(t *testing.T) {
	t.Parallel()

	defaults, err := searchTool(context.Background(), SearchParams{Query: "golang generics"})
	require.NoError(t, err)
	require.Contains(t, defaults, "Searching for: \"golang generics\"")
	require.Contains(t, defaults, "Max results: 10")
	require.Contains(t, defaults, "Sort by: relevance")
	require.NotContains(t, defaults, "Filters:")

	withOptions, err := searchTool(context.Background(), SearchParams{
		Query:      "go testing",
		MaxResults: 3,
		Filters:    []string{"language:go", "kind:test"},
		SortBy:     "date",
	})
	require.NoError(t, err)
	require.Contains(t, withOptions, "Max results: 3")
	require.Contains(t, withOptions, "Sort by: date")
	require.Contains(t, withOptions, "Filters: language:go, kind:test")
}
