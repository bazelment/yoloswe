package displaytext_test

import (
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/displaytext"
)

func TestTruncate(t *testing.T) {
	if got := displaytext.Truncate("hello", 10); got != "hello" {
		t.Errorf("short string: got %q", got)
	}
	if got := displaytext.Truncate("hello world", 8); got != "hello..." {
		t.Errorf("truncated: got %q", got)
	}
}

func TestTruncatePath(t *testing.T) {
	short := "/tmp/test.go"
	if got := displaytext.TruncatePath(short, 50); got != short {
		t.Errorf("short path: got %q", got)
	}
	long := "/very/long/path/to/some/deeply/nested/file.go"
	got := displaytext.TruncatePath(long, 25)
	if got != ".../nested/file.go" {
		t.Errorf("truncated path should keep last two components: got %q", got)
	}
}
