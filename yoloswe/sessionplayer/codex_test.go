package sessionplayer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectFormat(t *testing.T) {
	t.Parallel()

	t.Run("claude directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), nil, 0o600); err != nil {
			t.Fatalf("write messages.jsonl: %v", err)
		}

		requireFormat(t, dir, FormatClaude)
	})

	t.Run("codex header", func(t *testing.T) {
		t.Parallel()

		path := writeTempFile(t, `{"format":"codex"}`+"\n")
		requireFormat(t, path, FormatCodex)
	})

	t.Run("legacy codex file", func(t *testing.T) {
		t.Parallel()

		path := writeTempFile(t, `{"direction":"received","message":{}}`+"\n")
		requireFormat(t, path, FormatCodex)
	})

	t.Run("empty file defaults to codex", func(t *testing.T) {
		t.Parallel()

		path := writeTempFile(t, "")
		requireFormat(t, path, FormatCodex)
	})
}

func TestDetectFormatErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing path", func(t *testing.T) {
		t.Parallel()

		_, err := DetectFormat(filepath.Join(t.TempDir(), "missing.jsonl"))
		requireErrorContains(t, err, "stat session path")
	})

	t.Run("directory missing claude messages", func(t *testing.T) {
		t.Parallel()

		_, err := DetectFormat(t.TempDir())
		requireErrorContains(t, err, "missing messages.jsonl")
	})

	t.Run("oversized header", func(t *testing.T) {
		t.Parallel()

		path := writeTempFile(t, strings.Repeat("x", bufioMaxScanTokenSize()+1))
		_, err := DetectFormat(path)
		requireErrorContains(t, err, "scan session log header")
	})
}

func requireFormat(t *testing.T, path string, want SessionFormat) {
	t.Helper()

	got, err := DetectFormat(path)
	if err != nil {
		t.Fatalf("DetectFormat(%q) returned error: %v", path, err)
	}
	if got != want {
		t.Fatalf("DetectFormat(%q) = %q, want %q", path, got, want)
	}
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()

	if err == nil {
		t.Fatalf("DetectFormat() error = nil, want substring %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("DetectFormat() error = %q, want substring %q", err.Error(), want)
	}
}

func writeTempFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp session log: %v", err)
	}
	return path
}

func bufioMaxScanTokenSize() int {
	return 64 * 1024
}
