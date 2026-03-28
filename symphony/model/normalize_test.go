package model

import "testing"

func TestSanitizeIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ABC-123", "ABC-123"},
		{"simple", "simple"},
		{"with spaces", "with_spaces"},
		{"special/chars!@#", "special_chars___"},
		{"dots.and-dashes_ok", "dots.and-dashes_ok"},
		{"unicode—chars™", "unicode_chars_"},
		{"", ""},
		{"UPPER.lower-123_mix", "UPPER.lower-123_mix"},
		{"path/to/thing", "path_to_thing"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SanitizeIdentifier(tt.input)
			if got != tt.want {
				t.Errorf("SanitizeIdentifier(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeState(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Todo", "todo"},
		{"In Progress", "in progress"},
		{"DONE", "done"},
		{"already lower", "already lower"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := NormalizeState(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeState(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestComposeSessionID(t *testing.T) {
	got := ComposeSessionID("thread-abc", "turn-1")
	want := "thread-abc-turn-1"
	if got != want {
		t.Errorf("ComposeSessionID() = %q, want %q", got, want)
	}
}
