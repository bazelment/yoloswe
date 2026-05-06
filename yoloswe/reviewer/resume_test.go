package reviewer

import "testing"

func TestIsResumeUnavailableMessage(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{msg: "session not found", want: true},
		{msg: "Thread expired", want: true},
		{msg: "chat not found", want: true},
		{msg: "required parameter missing", want: false},
		{msg: "authentication token expired", want: false},
		{msg: "resume failed: auth token expired", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			got := isResumeUnavailableMessage(tt.msg)
			if got != tt.want {
				t.Fatalf("isResumeUnavailableMessage(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}
