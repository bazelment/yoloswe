package sessionmodel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatToolContentReadTruncatesToTrailingFilename(t *testing.T) {
	content := FormatToolContent("Read", map[string]interface{}{
		"file_path": "/really/long/prefix/that/exceeds/sixty/chars/with/many/segments/deeply/nested/file.go",
	})

	assert.Equal(t, "Read .../file.go", content)
}

func TestFormatToolContentTruncatesNonReadBranches(t *testing.T) {
	assertContent := func(t *testing.T, name string, input map[string]interface{}, maxRunes int, want string) {
		t.Helper()
		content := FormatToolContent(name, input)
		assert.LessOrEqual(t, len([]rune(content)), maxRunes)
		assert.Equal(t, want, content)
	}

	t.Run("Bash", func(t *testing.T) {
		assertContent(t, "Bash", map[string]interface{}{
			"command": strings.Repeat("printf unicode-λ ", 5),
		}, 56, "Bash: printf unicode-λ printf unicode-λ printf unicod...")
	})
	t.Run("Grep", func(t *testing.T) {
		assertContent(t, "Grep", map[string]interface{}{
			"pattern": strings.Repeat("needle", 10),
		}, 45, "Grep needleneedleneedleneedleneedleneedlen...")
	})
	t.Run("Task", func(t *testing.T) {
		assertContent(t, "Task", map[string]interface{}{
			"description": strings.Repeat("summarize ", 6),
		}, 46, "Task: summarize summarize summarize summari...")
	})
}
