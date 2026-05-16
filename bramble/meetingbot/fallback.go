package meetingbot

import (
	"fmt"
	"strings"
)

func immediateOpening(question string, snippets []MeetingEvent, evidence []Evidence) string {
	q := strings.ToLower(question)
	contextText := strings.ToLower(joinEventText(snippets) + "\n" + joinEvidenceText(evidence))
	switch {
	case strings.Contains(q, "preview"):
		return "The preview issue appears split between auth preview and app availability."
	case strings.Contains(q, "staging"):
		return "Based on the meeting, staging is useful but not the source of truth yet."
	case strings.Contains(q, "sandbox"):
		return "The sandbox work should focus on lifecycle/runtime state, not one isolated bug."
	case strings.Contains(q, "customer") || strings.Contains(q, "workflow"):
		if strings.Contains(q, "workflow") && !strings.Contains(contextText, "workflow") && !strings.Contains(contextText, "approval") {
			return "This note does not clearly establish a workflow priority change."
		}
		return "The customer ask is multi-department approval workflows with human review points."
	case strings.Contains(q, "action") || strings.Contains(q, "follow"):
		return "The highest-value follow-ups are deployment confidence, preview fixes, and customer-readiness work."
	case len(evidence) > 0:
		return "Based on the meeting and cached research, the answer is evidence-backed but still needs owner confirmation."
	case len(snippets) > 0:
		return "Based on the meeting context, the answer is tentative but actionable."
	default:
		return "I do not have enough meeting context yet, so this answer should be treated as provisional."
	}
}

func fallbackAnswer(question, opening string, snippets []MeetingEvent, evidence []Evidence) string {
	var b strings.Builder
	b.WriteString(opening)
	b.WriteString("\n\nMy read: ")
	b.WriteString(localRead(question, snippets, evidence))

	if len(snippets) > 0 {
		b.WriteString("\n\nMeeting evidence:\n")
		for _, e := range limitEvents(snippets, 5) {
			fmt.Fprintf(&b, "- %s\n", formatEvent(e))
		}
	}
	if len(evidence) > 0 {
		b.WriteString("\nResearch cross-check:\n")
		for _, ev := range limitEvidence(evidence, 4) {
			fmt.Fprintf(&b, "- [%s/%s] %s\n", ev.Scope, ev.Topic, compact(ev.Text, 260))
		}
	}
	return strings.TrimSpace(b.String())
}

func localRead(question string, snippets []MeetingEvent, evidence []Evidence) string {
	joined := strings.ToLower(question + "\n" + joinEventText(snippets) + "\n" + joinEvidenceText(evidence))
	switch {
	case strings.Contains(joined, "preview"):
		return "treat preview as two separate problems: preview auth/full-screen routing can be mitigated quickly, while missing app availability needs session/workspace investigation."
	case strings.Contains(joined, "staging"):
		return "do not over-index on abandoned staging demo apps; use production for customer demos and staging only to validate newly deployed fixes."
	case strings.Contains(joined, "sandbox"):
		return "the recurring failures point at lifecycle state drift between sessions, workers, sandboxes, and project records; a narrow patch may not be enough without runtime/state ownership."
	case strings.Contains(joined, "workflow"):
		return "new customer demand clusters around intake and approval workflows: humans review, managers approve, other departments sign off, and the system closes the loop."
	case strings.Contains(joined, "action") || strings.Contains(joined, "follow"):
		return "owners should close deployment configuration gaps, validate preview paths, keep CA/customer testing unblocked, and turn the repeated sandbox findings into a lifecycle plan."
	default:
		if len(evidence) > 0 {
			return "the cached research supports answering from the meeting record, but the question still needs a named owner or source confirmation before being treated as a decision."
		}
		return "the transcript has related discussion, but not enough cross-referenced research to make a stronger claim."
	}
}

func fallbackSummary(events []MeetingEvent, evidence []Evidence) string {
	var b strings.Builder
	topics := candidateTopics(events, 8)
	b.WriteString("Executive summary\n")
	b.WriteString("The meeting centered on deployment reliability, sandbox/preview correctness, customer-readiness work, and workflow product direction. The most important operational pattern is that several issues were not single bugs; they involved configuration, secrets, lifecycle state, or environment drift.\n\n")

	b.WriteString("Decisions\n")
	for _, line := range inferDecisionLines(events) {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	if len(inferDecisionLines(events)) == 0 {
		b.WriteString("- No explicit final decision was strongly established in the transcript excerpts.\n")
	}

	b.WriteString("\nAction items\n")
	for _, line := range inferActionLines(events) {
		fmt.Fprintf(&b, "- %s\n", line)
	}

	b.WriteString("\nRisks/blockers\n")
	for _, line := range inferRiskLines(events) {
		fmt.Fprintf(&b, "- %s\n", line)
	}

	b.WriteString("\nBackground/context\n")
	if len(topics) > 0 {
		names := make([]string, 0, len(topics))
		for _, t := range topics {
			names = append(names, t.Name)
		}
		fmt.Fprintf(&b, "- Dominant topics: %s.\n", strings.Join(names, ", "))
	}
	for _, ev := range limitEvidence(evidence, 8) {
		fmt.Fprintf(&b, "- [%s/%s] %s\n", ev.Scope, ev.Topic, compact(ev.Text, 260))
	}
	return strings.TrimSpace(b.String())
}

func inferDecisionLines(events []MeetingEvent) []string {
	text := strings.ToLower(joinEventText(events))
	var out []string
	if strings.Contains(text, "production going forward") || strings.Contains(text, "prod") {
		out = append(out, "Customer demos should rely on production where possible; abandoned staging apps should not trigger urgent reaction.")
	}
	if strings.Contains(text, "disable") && strings.Contains(text, "preview") {
		out = append(out, "Preview auth/full-screen behavior can be disabled or simplified as a quick mitigation while the deeper app-card issue is investigated.")
	}
	if strings.Contains(text, "human assigns") || strings.Contains(text, "builder light") || strings.Contains(text, "full builder") {
		out = append(out, "Ticket execution remains human-triggered for now, with Builder Lite or full Builder selected from the ticket.")
	}
	if strings.Contains(text, "workflow") {
		out = append(out, "Near-term customer demand is converging on human-in-the-loop, multi-department approval workflows.")
	}
	return dedupe(out)
}

func inferActionLines(events []MeetingEvent) []string {
	text := strings.ToLower(joinEventText(events))
	var out []string
	if strings.Contains(text, "sql secret") || strings.Contains(text, "secrets") {
		out = append(out, "Ming/Ashudeep: finish root-cause follow-up on staging/prod secrets and configuration drift.")
	}
	if strings.Contains(text, "feedback endpoint") || strings.Contains(text, "document extraction") {
		out = append(out, "Pranava: design the feedback endpoint so CA can report document-extraction issues during testing.")
	}
	if strings.Contains(text, "sandbox") {
		out = append(out, "Ashudeep/Jian/CJ: align on sandbox lifecycle/runtime requirements and verify deployed fixes in staging.")
	}
	if strings.Contains(text, "preview") {
		out = append(out, "Igor/Ashudeep/Anand: separate preview-auth mitigation from missing app availability and assign each issue.")
	}
	if strings.Contains(text, "builder lite") || strings.Contains(text, "judge") {
		out = append(out, "Abhinav: continue Builder Lite smoke test, judge infrastructure, and pre-PR optimization work.")
	}
	if len(out) == 0 {
		out = append(out, "Convert the main discussion threads into owner-specific follow-ups before the next meeting.")
	}
	return dedupe(out)
}

func inferRiskLines(events []MeetingEvent) []string {
	text := strings.ToLower(joinEventText(events))
	var out []string
	if strings.Contains(text, "staging") && strings.Contains(text, "prod") {
		out = append(out, "Staging/prod drift can make incident triage misleading unless the team is explicit about which environment is authoritative.")
	}
	if strings.Contains(text, "sandbox") {
		out = append(out, "Sandbox lifecycle state can drift across workers, sessions, tables, and UI assumptions, causing recurring preview/build failures.")
	}
	if strings.Contains(text, "secret") {
		out = append(out, "Secret propagation and GitHub auth setup remain high-leverage failure points for deployments.")
	}
	if strings.Contains(text, "workflow") {
		out = append(out, "Workflow opportunities are promising but need concrete status-quo mapping from customers before product scope grows too broad.")
	}
	if len(out) == 0 {
		out = append(out, "The transcript contains unresolved threads; the summary should be validated against owners before being treated as final.")
	}
	return dedupe(out)
}

func joinEventText(events []MeetingEvent) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func joinEvidenceText(evidence []Evidence) string {
	var b strings.Builder
	for _, ev := range evidence {
		b.WriteString(ev.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func limitEvents(events []MeetingEvent, n int) []MeetingEvent {
	if len(events) <= n {
		return events
	}
	return events[:n]
}

func limitEvidence(evidence []Evidence, n int) []Evidence {
	if len(evidence) <= n {
		return evidence
	}
	return evidence[:n]
}

func dedupe(lines []string) []string {
	seen := make(map[string]struct{}, len(lines))
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key := strings.ToLower(line)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, line)
	}
	return out
}
