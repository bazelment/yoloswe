package tracker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/symphony/tracker"
	"github.com/bazelment/yoloswe/symphony/tracker/linear"
)

func TestNewLinear(t *testing.T) {
	t.Parallel()

	got, err := tracker.New(tracker.KindLinear, "https://linear.example/graphql", "test-api-key")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got == nil {
		t.Fatal("New() tracker = nil")
	}

	issues, err := got.FetchIssueStatesByIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs(empty) error = %v", err)
	}
	if issues != nil {
		t.Fatalf("FetchIssueStatesByIDs(empty) = %v, want nil", issues)
	}
}

func TestNewErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing api key", func(t *testing.T) {
		t.Parallel()

		_, err := tracker.New(tracker.KindLinear, "", "")
		requireLinearError(t, err, linear.ErrMissingTrackerAPIKey, "api key is required")
	})

	t.Run("unsupported kind", func(t *testing.T) {
		t.Parallel()

		_, err := tracker.New("jira", "", "key")
		requireLinearError(t, err, linear.ErrUnsupportedTrackerKind, "unsupported tracker kind: jira")
	})
}

func requireLinearError(t *testing.T, err error, wantCategory linear.ErrorCategory, wantMessage string) {
	t.Helper()

	var trackerErr *linear.Error
	if !errors.As(err, &trackerErr) {
		t.Fatalf("error = %v, want *linear.Error", err)
	}
	if trackerErr.Category != wantCategory {
		t.Fatalf("error category = %q, want %q", trackerErr.Category, wantCategory)
	}
	if !strings.Contains(trackerErr.Message, wantMessage) {
		t.Fatalf("error message = %q, want substring %q", trackerErr.Message, wantMessage)
	}
}
