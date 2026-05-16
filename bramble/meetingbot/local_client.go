package meetingbot

import (
	"context"
	"fmt"
	"strings"
	"time"
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
		text = localFastAnswer(req.Prompt)
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

func localFastAnswer(prompt string) string {
	lower := strings.ToLower(prompt)
	question := strings.ToLower(promptValue(prompt, "Question:"))
	evidenceText := strings.ToLower(strings.Join(relevantPromptLines(prompt, 12), "\n"))
	opening := promptValue(prompt, "The bot already streamed this first sentence to satisfy live latency:")
	if opening == "" {
		opening = immediateOpening(promptValue(prompt, "Question:"), nil, nil)
	}

	var read string
	switch {
	case strings.Contains(question, "workflow") || strings.Contains(question, "customer"):
		if strings.Contains(question, "workflow") && !strings.Contains(evidenceText, "workflow") && !strings.Contains(evidenceText, "approval") {
			read = "This meeting note does not contain enough evidence that workflow priorities changed. It does contain customer-readiness work around feedback, CA testing, and custom app stability, so the next step is to verify workflow demand against a later note or customer source."
		} else {
			read = "New customer interest is converging on workflow automation with human approval steps. The clearest version is intake, manager approval, department review, and a final closure action."
		}
	case strings.Contains(question, "staging"):
		read = "Staging should be used to validate fixes, but the meeting repeatedly warned not to treat old abandoned staging demo apps as urgent customer-facing regressions."
	case strings.Contains(question, "sandbox") || strings.Contains(question, "preview"):
		read = "The team identified preview as a layered issue: quick mitigation is to disable or simplify preview auth/full-screen behavior, while the deeper missing app-card/app-availability path needs session-level debugging."
	case strings.Contains(question, "action") || strings.Contains(question, "follow"):
		read = "Owners should close deployment configuration gaps, validate preview paths, keep CA/customer testing unblocked, and turn the repeated sandbox findings into a lifecycle plan."
	case strings.Contains(lower, "preview"):
		read = "The team identified preview as a layered issue: quick mitigation is to disable or simplify preview auth/full-screen behavior, while the deeper missing app-card/app-availability path needs session-level debugging."
	case strings.Contains(lower, "staging"):
		read = "Staging should be used to validate fixes, but the meeting repeatedly warned not to treat old abandoned staging demo apps as urgent customer-facing regressions."
	case strings.Contains(lower, "sandbox"):
		read = "The most useful framing is lifecycle and runtime consistency. The transcript mentions state drift across worker, sandbox, project, and session tables, so a local one-off patch is risky."
	case strings.Contains(lower, "workflow") || strings.Contains(lower, "customer"):
		read = "New customer interest is converging on workflow automation with human approval steps. The clearest version is intake, manager approval, department review, and a final closure action."
	default:
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
	if strings.Contains(lower, "feedback endpoint") || strings.Contains(lower, "document extraction") {
		b.WriteString("- Build the CA feedback endpoint so customer testing can report document extraction issues directly.\n")
	}
	if strings.Contains(lower, "builder lite") || strings.Contains(lower, "judge") {
		b.WriteString("- Continue Builder Lite smoke tests and judge work, with attention to pre-PR rerun conservatism.\n")
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
	for _, token := range significantWords(line) {
		if token == keyword {
			return true
		}
	}
	return false
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
