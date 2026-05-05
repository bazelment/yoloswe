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
	if got := displaytext.Truncate("hello 世界!", 8); got != "hello..." {
		t.Errorf("unicode truncation: got %q", got)
	}
	if got := displaytext.Truncate("世界hello", 3); got != "世界h" {
		t.Errorf("small max should not add suffix: got %q", got)
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

func TestTruncatePathComponents(t *testing.T) {
	long := "/very/long/path/to/some/deeply/nested/file.go"
	got := displaytext.TruncatePathComponents(long, 25, 1)
	if got != ".../file.go" {
		t.Errorf("truncated path should keep filename: got %q", got)
	}
	unicode := "/really/long/path/with/unicode/世界/file.go"
	got = displaytext.TruncatePathComponents(unicode, 13, 2)
	if got != ".../file.go" {
		t.Errorf("unicode path should use rune-aware fallback: got %q", got)
	}
	nonASCIIFilename := "/really/long/path/世界.go"
	got = displaytext.TruncatePathComponents(nonASCIIFilename, 10, 1)
	if got != ".../世界.go" {
		t.Errorf("non-ASCII filename should be preserved when it fits: got %q", got)
	}
	tooLongFilename := "/very/long/path/supercalifragilistic.go"
	got = displaytext.TruncatePathComponents(tooLongFilename, 12, 1)
	if got != "/very/lon..." {
		t.Errorf("too-long filename should fall back to generic truncation: got %q", got)
	}
}
