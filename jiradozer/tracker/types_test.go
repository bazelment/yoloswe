package tracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitCSV(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "empty returns nil", input: "", want: nil},
		{name: "single value", input: "Todo", want: []string{"Todo"}},
		{name: "multiple values", input: "Todo,Backlog", want: []string{"Todo", "Backlog"}},
		{name: "trims spaces", input: " Todo , Backlog ", want: []string{"Todo", "Backlog"}},
		{name: "preserves internal spaces", input: "In Progress,Todo", want: []string{"In Progress", "Todo"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SplitCSV(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}
