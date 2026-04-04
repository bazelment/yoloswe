package jiradozer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCommentAction(t *testing.T) {
	tests := []struct {
		body string
		want FeedbackAction
	}{
		{"approve", FeedbackApprove},
		{"Approve", FeedbackApprove},
		{"APPROVE", FeedbackApprove},
		{"  approve  ", FeedbackApprove},
		{"lgtm", FeedbackApprove},
		{"LGTM", FeedbackApprove},
		{"ship it", FeedbackApprove},
		{"approved", FeedbackApprove},
		{"redo", FeedbackRedo},
		{"Redo with changes", FeedbackRedo},
		{"retry", FeedbackRedo},
		{"Retry please", FeedbackRedo},
		{"Please fix the tests", FeedbackComment},
		{"", FeedbackComment},
		{"I think the plan is good but could use more detail", FeedbackComment},
	}

	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			got := ParseCommentAction(tt.body)
			assert.Equal(t, tt.want, got, "body=%q", tt.body)
		})
	}
}
