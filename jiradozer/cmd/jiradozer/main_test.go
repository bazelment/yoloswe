package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "no sensitive flags",
			args: []string{"--issue", "ENG-123", "--verbose"},
			want: []string{"--issue", "ENG-123", "--verbose"},
		},
		{
			name: "api-key equals form",
			args: []string{"--api-key=sk-secret123", "--verbose"},
			want: []string{"--api-key=***", "--verbose"},
		},
		{
			name: "api-key space form",
			args: []string{"--api-key", "sk-secret123", "--verbose"},
			want: []string{"--api-key", "***", "--verbose"},
		},
		{
			name: "description redacted",
			args: []string{"--description", "my secret plan", "--verbose"},
			want: []string{"--description", "***", "--verbose"},
		},
		{
			name: "description equals form",
			args: []string{"--description=my secret plan"},
			want: []string{"--description=***"},
		},
		{
			name: "multiple sensitive flags",
			args: []string{"--token", "tok123", "--password=hunter2"},
			want: []string{"--token", "***", "--password=***"},
		},
		{
			name: "sensitive flag at end without value",
			args: []string{"--verbose", "--api-key"},
			want: []string{"--verbose", "--api-key"},
		},
		{
			name: "empty args",
			args: []string{},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactArgs(tt.args)
			assert.Equal(t, tt.want, got)
		})
	}
}
