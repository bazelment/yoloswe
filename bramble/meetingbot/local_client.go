package meetingbot

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// LocalAgentClient is a deterministic, no-network client for tests and offline
// evaluations. It exercises the same orchestration path as real providers.
type LocalAgentClient struct{}

func (LocalAgentClient) Run(ctx context.Context, req AgentRequest) (AgentResponse, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return AgentResponse{Latency: time.Since(start), Model: req.Model, Provider: "local"}, err
	}

	var text string
	switch req.Role {
	case RoleFastAnswer:
		text = localFastAnswer(req)
	case RoleSummary:
		text = localSummary(req.Prompt)
	case RoleInternalResearch, RoleCodebaseResearch, RoleWebResearch:
		text = localResearch(req.Role, req.Prompt)
	default:
		text = compact(req.Prompt, 1000)
	}
	return AgentResponse{
		Latency:  time.Since(start),
		Text:     text,
		Model:    firstNonEmpty(req.Model, "local"),
		Provider: "local",
	}, nil
}

func localResearch(role AgentRole, prompt string) string {
	lines := relevantPromptLines(prompt, 8)
	switch role {
	case RoleCodebaseResearch:
		return "Codebase research (offline): no repository scan was performed by the local client. Relevant meeting anchors:\n" + bulletLines(lines)
	case RoleWebResearch:
		return "Public-web research (offline): no public internet lookup was performed by the local client. Use real Codex/Claude mode for URL-cited findings. Relevant meeting anchors:\n" + bulletLines(lines)
	default:
		return "Internal research: the discussion points to these reusable context anchors:\n" + bulletLines(lines)
	}
}

func localFastAnswer(req AgentRequest) string {
	prompt := req.Prompt
	question := firstNonEmpty(req.Question, promptValue(prompt, "Question:"))
	evidenceText := strings.Join(relevantPromptLines(prompt, 12), "\n")
	opening := firstNonEmpty(req.Opening, promptValue(prompt, "The bot already streamed this first sentence to satisfy live latency:"))
	if opening == "" {
		opening = immediateOpening(question, nil, nil)
	}

	focus := classifyAnswerFocus(question, evidenceText+"\n"+prompt)
	read := localReadText(focus, strings.TrimSpace(evidenceText) != "")
	if focus == focusGeneric {
		read = "The answer should stay grounded in the transcript snippets and cached research because the meeting contains unresolved threads."
	}

	return opening + "\n\n" + read + "\n\nSupporting anchors:\n" + bulletLines(relevantPromptLines(prompt, 5))
}

func localSummary(prompt string) string {
	lower := strings.ToLower(prompt)
	var b strings.Builder
	b.WriteString("Executive summary\n")
	b.WriteString("The meeting focused on reliability work around deployments, secrets, preview/sandbox behavior, and customer-readiness. Product direction also surfaced around human-in-the-loop workflows and app/ticket upgrade paths.\n\n")

	b.WriteString("Decisions\n")
	if strings.Contains(lower, "production going forward") || strings.Contains(lower, "staging") {
		b.WriteString("- Use production as the reliable demo surface where possible; abandoned staging apps should not drive urgent response.\n")
	}
	if strings.Contains(lower, "preview") {
		b.WriteString("- Treat preview auth/full-screen behavior separately from missing app availability so the quick mitigation does not hide the deeper bug.\n")
	}
	if strings.Contains(lower, "workflow") {
		b.WriteString("- Customer workflow demand should be framed as multi-step, human-in-the-loop approval flows.\n")
	}

	b.WriteString("\nAction items\n")
	if strings.Contains(lower, "secret") || strings.Contains(lower, "deployment") {
		b.WriteString("- Close configuration and secret propagation gaps before the next staging/prod deployment cycle.\n")
	}
	if strings.Contains(lower, "sandbox") || strings.Contains(lower, "preview") {
		b.WriteString("- Assign preview/sandbox lifecycle investigation by separating UI/auth symptoms from backend app/session state symptoms.\n")
	}
	if strings.Contains(lower, "feedback") || strings.Contains(lower, "extraction") {
		b.WriteString("- Build the feedback channel so pilot testing can report extraction issues directly.\n")
	}
	if strings.Contains(lower, "builder") || strings.Contains(lower, "judge") {
		b.WriteString("- Continue builder smoke tests and judge work, with attention to pre-PR rerun conservatism.\n")
	}

	b.WriteString("\nRisks/blockers\n")
	b.WriteString("- Environment drift across dev, staging, and prod can mislead incident response.\n")
	b.WriteString("- Sandbox state drift can reappear unless lifecycle ownership is made explicit.\n")
	b.WriteString("- Workflow scope could sprawl unless customer status quo and approval paths are pinned down.\n\n")

	b.WriteString("Background/context\n")
	b.WriteString(bulletLines(relevantPromptLines(prompt, 8)))
	return strings.TrimSpace(b.String())
}

func relevantPromptLines(prompt string, limit int) []string {
	keywords := []string{
		"sandbox", "preview", "staging", "production", "prod", "secret", "deployment",
		"workflow", "customer", "feedback", "builder", "ticket", "app", "github",
		"apps", "application", "cloud run", "lite", "sso", "judge",
	}
	lines := strings.Split(prompt, "\n")
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Question:") ||
			strings.HasPrefix(trimmed, "Cached research:") ||
			strings.HasPrefix(trimmed, "Now produce") ||
			strings.HasPrefix(trimmed, "The bot already streamed") {
			continue
		}
		lower := strings.ToLower(trimmed)
		for _, kw := range keywords {
			if lineMatchesKeyword(lower, kw) {
				out = append(out, compact(trimmed, 320))
				break
			}
		}
		if len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		for _, line := range lines {
			trimmed := strings.TrimSpace(strings.TrimPrefix(line, "- "))
			if trimmed == "" || strings.HasSuffix(trimmed, ":") {
				continue
			}
			out = append(out, compact(trimmed, 320))
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func lineMatchesKeyword(line, keyword string) bool {
	if strings.Contains(keyword, " ") {
		return strings.Contains(line, keyword)
	}
	// Tokenize without significantWords' length/stop-word filter: that filter
	// drops words shorter than 4 chars, which would make short keywords like
	// "app" or "sso" silently dead. Whole-token equality still avoids the
	// substring false positives a plain strings.Contains would introduce.
	for _, token := range lineTokens(line) {
		if token == keyword {
			return true
		}
	}
	return false
}

// lineTokens splits text into lowercase word tokens on the same rune classes
// significantWords uses, but without its length or stop-word filtering.
func lineTokens(text string) []string {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return unicode.ToLower(r)
		}
		return ' '
	}, text)
	return strings.Fields(normalized)
}

func bulletLines(lines []string) string {
	if len(lines) == 0 {
		return "- No relevant anchors found.\n"
	}
	var b strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	return b.String()
}

func promptValue(prompt, label string) string {
	idx := strings.Index(prompt, label)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(prompt[idx+len(label):])
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest)
}
