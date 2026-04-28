package cliapp

import (
	"reflect"
	"testing"
)

func TestRedactArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		sensitive []string
		want      []string
	}{
		{
			name:      "flag=value form",
			args:      []string{"--api-key=abc", "--port", "8080"},
			sensitive: []string{"--api-key"},
			want:      []string{"--api-key=***", "--port", "8080"},
		},
		{
			name:      "flag value form",
			args:      []string{"--api-key", "abc", "--port", "8080"},
			sensitive: []string{"--api-key"},
			want:      []string{"--api-key", "***", "--port", "8080"},
		},
		{
			name:      "flag value form at end",
			args:      []string{"--port", "8080", "--token"},
			sensitive: []string{"--token"},
			// trailing --token with no value: nothing to redact, leave as is
			want: []string{"--port", "8080", "--token"},
		},
		{
			name:      "non-sensitive prefix that shares characters",
			args:      []string{"--api-keystore", "value"},
			sensitive: []string{"--api-key"},
			// must not match --api-keystore as --api-key
			want: []string{"--api-keystore", "value"},
		},
		{
			name:      "multiple sensitive flags",
			args:      []string{"--token", "T", "--secret=S", "--port", "9"},
			sensitive: []string{"--token", "--secret"},
			want:      []string{"--token", "***", "--secret=***", "--port", "9"},
		},
		{
			name:      "empty sensitive list is a noop",
			args:      []string{"--token", "T"},
			sensitive: nil,
			want:      []string{"--token", "T"},
		},
		{
			name:      "does not mutate input",
			args:      []string{"--token", "T"},
			sensitive: []string{"--token"},
			want:      []string{"--token", "***"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := append([]string(nil), tt.args...)
			got := RedactArgs(input, tt.sensitive)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RedactArgs(%v, %v) = %v, want %v", tt.args, tt.sensitive, got, tt.want)
			}
			if !reflect.DeepEqual(input, tt.args) {
				t.Errorf("RedactArgs mutated input: got %v, want %v", input, tt.args)
			}
		})
	}
}
