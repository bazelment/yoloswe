package sessionmodel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatToolContentTruncatesLongPathWithParent(t *testing.T) {
	content := FormatToolContent("Read", map[string]interface{}{
		"file_path": "/really/long/prefix/that/exceeds/sixty/chars/with/many/segments/deeply/nested/file.go",
	})

	assert.Equal(t, "Read .../file.go", content)
}
