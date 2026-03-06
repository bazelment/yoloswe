package sessionanalysis

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// SummarizeSession uses Claude Haiku to generate a concise session summary.
// It sends the session's turn-by-turn conversation to Haiku and asks for a
// 2-3 sentence summary of what was accomplished.
func SummarizeSession(ctx context.Context, sess *Session) (string, error) {
	prompt := buildSummaryPrompt(sess)

	result, err := claude.Query(ctx, prompt,
		claude.WithModel("haiku"),
		claude.WithDisablePlugins(),
	)
	if err != nil {
		return "", fmt.Errorf("haiku query: %w", err)
	}

	return cleanSummary(result.Text), nil
}

// cleanSummary strips unwanted headers/prefixes that Haiku sometimes adds.
func cleanSummary(s string) string {
	s = strings.TrimSpace(s)
	// Strip markdown headers like "## Summary\n\n"
	for strings.HasPrefix(s, "#") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = strings.TrimSpace(s[idx+1:])
		} else {
			break
		}
	}
	// Strip bold prefixes like "**Session Summary:**\n\n"
	for _, prefix := range []string{
		"**Session Summary:**",
		"**Summary:**",
	} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
	}
	return s
}

// SummarizeSessions summarizes multiple sessions, skipping failures.
func SummarizeSessions(ctx context.Context, sessions []*Session) {
	for _, sess := range sessions {
		if len(sess.Turns) == 0 {
			continue
		}
		summary, err := SummarizeSession(ctx, sess)
		if err != nil {
			continue
		}
		sess.Summary = summary
	}
}

// SummarizeTurns uses Haiku to summarize long agent responses within sessions.
// Turns with responses exceeding wordLimit words get their ResponseSummary populated.
func SummarizeTurns(ctx context.Context, sessions []*Session, wordLimit int) {
	if wordLimit <= 0 {
		return
	}
	for _, sess := range sessions {
		for i := range sess.Turns {
			t := &sess.Turns[i]
			if t.ResponseWordCount() <= wordLimit {
				continue
			}
			summary, err := summarizeText(ctx, t.Response)
			if err != nil {
				continue
			}
			t.ResponseSummary = summary
		}
	}
}

// summarizeText uses Haiku to produce a concise summary of a long agent response.
func summarizeText(ctx context.Context, text string) (string, error) {
	// Truncate input to ~6K chars to stay within Haiku context.
	if len(text) > 6000 {
		text = text[:3000] + "\n[...]\n" + text[len(text)-3000:]
	}

	prompt := "Summarize this Claude Code agent response concisely, preserving key actions, decisions, and outcomes. Keep it under 100 words.\n\nIMPORTANT: Return ONLY the plain summary text. No headers or formatting prefixes.\n\n" + text

	result, err := claude.Query(ctx, prompt,
		claude.WithModel("haiku"),
		claude.WithDisablePlugins(),
	)
	if err != nil {
		return "", fmt.Errorf("haiku query: %w", err)
	}
	return cleanSummary(result.Text), nil
}

// buildSummaryPrompt constructs the prompt sent to Haiku for summarization.
func buildSummaryPrompt(sess *Session) string {
	var b strings.Builder

	b.WriteString("Summarize this Claude Code session in 2-3 sentences. Focus on: what was the goal, what was done, and the outcome. Be specific about file names, features, or bugs when mentioned.\n\nIMPORTANT: Return ONLY the plain summary text. Do NOT include any headers, prefixes, or formatting like '## Summary', '**Session Summary:**', or similar. Just write the sentences directly.\n\n")

	b.WriteString(fmt.Sprintf("Branch: %s\n", sess.GitBranch))
	b.WriteString(fmt.Sprintf("Duration: %s\n", sess.Duration().Round(time.Second)))
	b.WriteString(fmt.Sprintf("Turns: %d\n", len(sess.Turns)))
	b.WriteString("\n--- Conversation ---\n\n")

	for i := range sess.Turns {
		t := &sess.Turns[i]
		b.WriteString(fmt.Sprintf("Turn %d:\n", t.Number))

		// User input (truncated)
		input := t.UserInput
		if len(input) > 500 {
			input = input[:500] + "..."
		}
		b.WriteString(fmt.Sprintf("USER: %s\n", input))

		// Agent response (truncated for Haiku context)
		response := t.Response
		if len(response) > 800 {
			response = response[:400] + "\n[...]\n" + response[len(response)-400:]
		}
		if response == "" && len(t.ToolCalls) > 0 {
			b.WriteString(fmt.Sprintf("AGENT: [tool-only turn, %d tool calls]\n", len(t.ToolCalls)))
		} else {
			b.WriteString(fmt.Sprintf("AGENT: %s\n", response))
		}
		if len(t.Errors) > 0 {
			b.WriteString(fmt.Sprintf("Errors: %s\n", strings.Join(t.Errors, "; ")))
		}
		b.WriteString("\n")

		// Cap total prompt size to ~8K chars to stay within Haiku context.
		if b.Len() > 8000 {
			b.WriteString("[... remaining turns omitted ...]\n")
			break
		}
	}

	return b.String()
}
