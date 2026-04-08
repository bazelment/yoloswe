package claude

import "testing"

func TestExtractBackgroundTaskID(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "standard format",
			content: "Command running in background with ID: b2g7s7xob. Output is being written to: /tmp/claude-1000/tasks/b2g7s7xob.output",
			want:    "b2g7s7xob",
		},
		{
			name:    "id without trailing period",
			content: "Command running in background with ID: abc123",
			want:    "abc123",
		},
		{
			name:    "agent task id format",
			content: "Command running in background with ID: a405317c84ec82604. Output is being written to: /tmp/tasks/a405317c84ec82604.output",
			want:    "a405317c84ec82604",
		},
		{
			name:    "no match",
			content: "Some other tool output",
			want:    "",
		},
		{
			name:    "empty string",
			content: "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractBackgroundTaskID(tt.content)
			if got != tt.want {
				t.Errorf("extractBackgroundTaskID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractTaskNotificationID(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "bash task notification",
			content: `<task-notification>
<task-id>b0sp8wxsl</task-id>
<tool-use-id>toolu_01D5HisjsLjwnzyRjrpf2VSM</tool-use-id>
<output-file>/tmp/claude-1000/tasks/b0sp8wxsl.output</output-file>
<status>completed</status>
<summary>Background command "Codex structured review" completed (exit code 0)</summary>
</task-notification>`,
			want: "b0sp8wxsl",
		},
		{
			name: "agent task notification",
			content: `<task-notification>
<task-id>a405317c84ec82604</task-id>
<tool-use-id>toolu_01EcSBZ7cjAEygUt2ZPB6ewm</tool-use-id>
<output-file>/tmp/claude-1000/tasks/a405317c84ec82604.output</output-file>
<status>completed</status>
<summary>Agent "Claude adversarial review" completed</summary>
<result>Analysis results here...</result>
</task-notification>`,
			want: "a405317c84ec82604",
		},
		{
			name:    "not a task notification",
			content: "Just some regular text from the user",
			want:    "",
		},
		{
			name:    "empty string",
			content: "",
			want:    "",
		},
		{
			name:    "task-notification tag but no task-id",
			content: "<task-notification><status>completed</status></task-notification>",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTaskNotificationID(tt.content)
			if got != tt.want {
				t.Errorf("extractTaskNotificationID() = %q, want %q", got, tt.want)
			}
		})
	}
}
