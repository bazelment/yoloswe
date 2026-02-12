package app

import "strings"

// renderTextContent renders text content using markdown only when it is
// multi-line. This keeps single-line assistant text stable and avoids
// markdown-induced wrapping artifacts in both TUI and replay views.
func renderTextContent(content string, md *MarkdownRenderer, fallbackPrefix string) string {
	normalized := strings.Trim(content, "\r\n")
	if md != nil && shouldRenderMarkdownText(normalized) {
		rendered, err := md.Render(normalized)
		if err == nil {
			return strings.Trim(rendered, "\n")
		}
	}
	return fallbackPrefix + normalized
}

func shouldRenderMarkdownText(content string) bool {
	return strings.Contains(content, "\n")
}
