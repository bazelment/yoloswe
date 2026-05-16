package meetingbot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Bot follows a meeting transcript, builds background research, and answers
// live questions with a fast opening plus a deeper synthesized answer.
type Bot struct { //nolint:govet // fieldalignment: lifecycle deps first, mutable state second.
	client AgentClient
	cfg    Config

	mu         sync.RWMutex
	events     []MeetingEvent
	evidence   []Evidence
	researched map[string]struct{}
}

// New creates a meeting bot. If client is nil, ProviderAgentClient is used.
func New(client AgentClient, cfg Config) *Bot {
	cfg = normalizeConfig(cfg)
	if client == nil {
		client = ProviderAgentClient{}
	}
	return &Bot{
		client:     client,
		cfg:        cfg,
		researched: make(map[string]struct{}),
	}
}

// Observe adds one transcript event. When AutoResearch is enabled, it performs
// bounded background research every ResearchChunkEvents transcript turns.
func (b *Bot) Observe(ctx context.Context, event MeetingEvent) error {
	b.mu.Lock()
	event.Index = len(b.events)
	b.events = append(b.events, event)
	shouldResearch := b.cfg.AutoResearch && b.cfg.ResearchChunkEvents > 0 && len(b.events)%b.cfg.ResearchChunkEvents == 0
	b.mu.Unlock()

	if shouldResearch {
		return b.BuildBackground(ctx)
	}
	return nil
}

// IngestTranscript feeds transcript events in order.
func (b *Bot) IngestTranscript(ctx context.Context, events []MeetingEvent) error {
	for _, e := range events {
		if err := b.Observe(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// BuildBackground extracts meeting topics and refreshes internal/code/web
// research caches. Work is intentionally bounded by MaxResearchTopics.
func (b *Bot) BuildBackground(ctx context.Context) error {
	events := b.snapshotEvents()
	if len(events) == 0 {
		return nil
	}
	topics := candidateTopics(events, b.cfg.MaxResearchTopics)
	for _, topic := range topics {
		if err := b.researchTopic(ctx, topic.Name, events); err != nil {
			return err
		}
	}
	return nil
}

// Topics returns the current top candidate topics.
func (b *Bot) Topics() []Topic {
	return candidateTopics(b.snapshotEvents(), b.cfg.MaxResearchTopics)
}

// Evidence returns a snapshot of cached research.
func (b *Bot) Evidence() []Evidence {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Evidence(nil), b.evidence...)
}

func (b *Bot) snapshotEvents() []MeetingEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]MeetingEvent(nil), b.events...)
}

func (b *Bot) researchTopic(ctx context.Context, topic string, events []MeetingEvent) error {
	snippets := selectSnippets(events, topic, b.cfg.MaxSnippetsPerPrompt)
	if len(snippets) == 0 {
		return nil
	}

	for _, scope := range b.cfg.ResearchScopes {
		key := string(scope) + "\x00" + strings.ToLower(topic)
		b.mu.RLock()
		_, done := b.researched[key]
		b.mu.RUnlock()
		if done {
			continue
		}

		req := b.researchRequest(scope, topic, snippets)
		resp, err := b.client.Run(ctx, req)
		if err != nil {
			// Public-web research may be unavailable in some local runs. Cache
			// the miss as evidence so downstream summaries know not to invent it.
			resp.Text = fmt.Sprintf("Research unavailable for %s/%s: %v", scope, topic, err)
		}
		text := strings.TrimSpace(resp.Text)
		if text == "" {
			text = fmt.Sprintf("No %s findings returned for %q.", scope, topic)
		}
		ev := Evidence{
			CreatedAt: b.cfg.Now(),
			Scope:     scope,
			Topic:     topic,
			Text:      text,
			Sources:   evidenceSources(scope, snippets),
		}

		b.mu.Lock()
		b.evidence = append(b.evidence, ev)
		b.researched[key] = struct{}{}
		b.mu.Unlock()
	}
	return nil
}

func (b *Bot) researchRequest(scope ResearchScope, topic string, snippets []MeetingEvent) AgentRequest {
	role := RoleInternalResearch
	model := b.cfg.ResearchModel
	permission := "plan"
	switch scope {
	case ScopeCodebase:
		role = RoleCodebaseResearch
		model = b.cfg.CodeResearchModel
	case ScopeWeb:
		role = RoleWebResearch
		model = b.cfg.WebResearchModel
		permission = ""
	}
	return AgentRequest{
		Role:           role,
		Model:          model,
		Effort:         b.cfg.ResearchEffort,
		Timeout:        b.cfg.ResearchTimeout,
		WorkDir:        b.cfg.WorkDir,
		PermissionMode: permission,
		SystemPrompt:   roleSystemPrompt(role),
		Prompt:         buildResearchPrompt(scope, topic, snippets),
	}
}

// AnswerQuestion returns a fast opening immediately, then asks the fast-answer
// agent to synthesize from meeting context and cached research.
func (b *Bot) AnswerQuestion(ctx context.Context, question string) (Answer, error) {
	start := time.Now()
	events := b.snapshotEvents()
	snippets := selectSnippets(events, question, b.cfg.MaxSnippetsPerPrompt)
	evidence := b.matchEvidence(question)

	opening := immediateOpening(question, snippets, evidence)
	first10 := time.Since(start)

	req := AgentRequest{
		Role:           RoleFastAnswer,
		Model:          b.cfg.FastAnswerModel,
		Effort:         b.cfg.FastAnswerEffort,
		Timeout:        b.cfg.FastAnswerTimeout,
		WorkDir:        b.cfg.WorkDir,
		PermissionMode: "plan",
		SystemPrompt:   roleSystemPrompt(RoleFastAnswer),
		Prompt:         buildAnswerPrompt(question, opening, snippets, evidence),
	}
	resp, err := b.client.Run(ctx, req)
	text := strings.TrimSpace(resp.Text)
	if err != nil || text == "" {
		text = fallbackAnswer(question, opening, snippets, evidence)
	}
	if !startsWithNormalized(text, opening) {
		text = opening + "\n\n" + text
	}
	return Answer{
		Question:            question,
		Opening:             opening,
		Text:                text,
		Model:               firstNonEmpty(resp.Model, b.cfg.FastAnswerModel),
		First10WordsLatency: first10,
		Evidence:            evidence,
		ResearchRefs:        evidenceRefList(evidence),
	}, err
}

// SummarizeMeeting produces a post-meeting synthesis cross-referenced with
// cached research.
func (b *Bot) SummarizeMeeting(ctx context.Context) (Summary, error) {
	start := time.Now()
	events := b.snapshotEvents()
	evidence := b.Evidence()
	req := AgentRequest{
		Role:           RoleSummary,
		Model:          b.cfg.SummaryModel,
		Effort:         b.cfg.SummaryEffort,
		Timeout:        b.cfg.SummaryTimeout,
		WorkDir:        b.cfg.WorkDir,
		PermissionMode: "plan",
		SystemPrompt:   roleSystemPrompt(RoleSummary),
		Prompt:         buildSummaryPrompt(events, evidence, b.cfg.MaxSnippetsPerPrompt*3),
	}
	resp, err := b.client.Run(ctx, req)
	text := strings.TrimSpace(resp.Text)
	if err != nil || text == "" {
		text = fallbackSummary(events, evidence)
	}
	return Summary{
		Text:     text,
		Model:    firstNonEmpty(resp.Model, b.cfg.SummaryModel),
		Latency:  time.Since(start),
		Evidence: evidence,
	}, err
}

func (b *Bot) matchEvidence(question string) []Evidence {
	terms := queryTerms(question)
	b.mu.RLock()
	defer b.mu.RUnlock()
	var scored []struct {
		ev    Evidence
		score int
	}
	for _, ev := range b.evidence {
		score := wordOverlapScore(strings.ToLower(ev.Topic+" "+ev.Text), terms)
		if score == 0 && len(terms) == 0 {
			score = 1
		}
		if score > 0 {
			scored = append(scored, struct {
				ev    Evidence
				score int
			}{ev: ev, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].ev.CreatedAt.Before(scored[j].ev.CreatedAt)
	})
	limit := 8
	if len(scored) < limit {
		limit = len(scored)
	}
	out := make([]Evidence, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, scored[i].ev)
	}
	return out
}

func buildResearchPrompt(scope ResearchScope, topic string, snippets []MeetingEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Research scope: %s\nTopic: %s\n\nMeeting evidence:\n", scope, topic)
	for _, e := range snippets {
		fmt.Fprintf(&b, "- %s\n", formatEvent(e))
	}
	fmt.Fprintf(&b, "\nReturn findings that help a live meeting assistant answer questions or write the final summary. Include uncertainty and source anchors.")
	return b.String()
}

func buildAnswerPrompt(question, opening string, snippets []MeetingEvent, evidence []Evidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s\n\nThe bot already streamed this first sentence to satisfy live latency: %s\n\n", question, opening)
	fmt.Fprintf(&b, "Relevant meeting context:\n")
	for _, e := range snippets {
		fmt.Fprintf(&b, "- %s\n", formatEvent(e))
	}
	fmt.Fprintf(&b, "\nCached research:\n")
	for _, ev := range evidence {
		fmt.Fprintf(&b, "- [%s/%s] %s\n", ev.Scope, ev.Topic, compact(ev.Text, 900))
	}
	fmt.Fprintf(&b, "\nNow produce the final answer. Keep the streamed opening if it is correct, refine after it, and cite meeting timestamps, file paths, or web sources when present.")
	return b.String()
}

func buildSummaryPrompt(events []MeetingEvent, evidence []Evidence, maxEvents int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Transcript excerpts:\n")
	for _, e := range representativeEvents(events, maxEvents) {
		fmt.Fprintf(&b, "- %s\n", formatEvent(e))
	}
	fmt.Fprintf(&b, "\nCached research:\n")
	for _, ev := range evidence {
		fmt.Fprintf(&b, "- [%s/%s] %s\n", ev.Scope, ev.Topic, compact(ev.Text, 1200))
	}
	fmt.Fprintf(&b, "\nWrite the final summary with sections: Executive summary, Decisions, Action items, Risks/blockers, Background/context. Cross-reference research where it changes interpretation.")
	return b.String()
}

func evidenceSources(scope ResearchScope, snippets []MeetingEvent) []string {
	sources := make([]string, 0, len(snippets))
	for _, e := range snippets {
		sources = append(sources, fmt.Sprintf("%s %s", scope, formatStamp(e.Start)))
	}
	return sources
}

func evidenceRefList(evidence []Evidence) []string {
	refs := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		refs = append(refs, fmt.Sprintf("%s/%s", ev.Scope, ev.Topic))
	}
	return refs
}

func candidateTopics(events []MeetingEvent, max int) []Topic {
	if max <= 0 {
		max = 4
	}
	counts := make(map[string]int)
	phrases := []string{
		"agent os", "app upgrade", "builder lite", "cloud run", "customer prod",
		"deployment", "feedback endpoint", "github app", "lite lm", "microvm",
		"preview", "production workspace", "sandbox", "sso", "staging",
		"tenant service", "tickets", "workflow", "workflows",
	}
	for _, e := range events {
		text := strings.ToLower(e.Text)
		for _, phrase := range phrases {
			if strings.Contains(text, phrase) {
				counts[phrase] += 5
			}
		}
		for _, w := range significantWords(text) {
			counts[w]++
		}
	}
	topics := make([]Topic, 0, len(counts))
	for name, score := range counts {
		topics = append(topics, Topic{Name: name, Score: score})
	}
	sort.Slice(topics, func(i, j int) bool {
		if topics[i].Score != topics[j].Score {
			return topics[i].Score > topics[j].Score
		}
		return topics[i].Name < topics[j].Name
	})
	if len(topics) > max {
		topics = topics[:max]
	}
	return topics
}

func selectSnippets(events []MeetingEvent, query string, max int) []MeetingEvent {
	if max <= 0 {
		max = 12
	}
	terms := queryTerms(query)
	var scored []struct {
		event MeetingEvent
		score int
	}
	for _, e := range events {
		score := wordOverlapScore(strings.ToLower(e.Text+" "+e.Speaker), terms)
		if score > 0 {
			scored = append(scored, struct {
				event MeetingEvent
				score int
			}{event: e, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].event.Index < scored[j].event.Index
	})
	if len(scored) == 0 {
		start := len(events) - max
		if start < 0 {
			start = 0
		}
		return append([]MeetingEvent(nil), events[start:]...)
	}
	if len(scored) > max {
		scored = scored[:max]
	}
	out := make([]MeetingEvent, 0, len(scored))
	for _, s := range scored {
		out = append(out, s.event)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func representativeEvents(events []MeetingEvent, max int) []MeetingEvent {
	if max <= 0 || len(events) <= max {
		return append([]MeetingEvent(nil), events...)
	}
	topics := candidateTopics(events, 8)
	seen := make(map[int]struct{})
	var out []MeetingEvent
	for _, topic := range topics {
		for _, e := range selectSnippets(events, topic.Name, 5) {
			if _, ok := seen[e.Index]; ok {
				continue
			}
			seen[e.Index] = struct{}{}
			out = append(out, e)
			if len(out) >= max {
				sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
				return out
			}
		}
	}
	for _, e := range events {
		if _, ok := seen[e.Index]; ok {
			continue
		}
		out = append(out, e)
		if len(out) >= max {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

var stopWords = map[string]struct{}{
	"about": {}, "after": {}, "again": {}, "also": {}, "because": {}, "being": {},
	"could": {}, "doing": {}, "done": {}, "from": {}, "going": {}, "have": {},
	"like": {}, "maybe": {}, "more": {}, "need": {}, "right": {}, "some": {},
	"that": {}, "their": {}, "them": {}, "then": {}, "there": {}, "these": {},
	"they": {}, "thing": {}, "think": {}, "this": {}, "those": {}, "today": {},
	"trying": {}, "were": {}, "what": {}, "when": {}, "with": {}, "would": {},
	"yeah": {}, "yesterday": {}, "your": {}, "mm-hmm": {}, "okay": {},
	"laughs": {}, "chuckles": {}, "just": {}, "will": {}, "said": {},
	"over": {}, "last": {}, "which": {},
}

func significantWords(text string) []string {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return unicode.ToLower(r)
		}
		return ' '
	}, text)
	raw := strings.Fields(normalized)
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{})
	for _, w := range raw {
		if len(w) < 4 {
			continue
		}
		if _, stop := stopWords[w]; stop {
			continue
		}
		if _, ok := seen[w]; ok {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}

func queryTerms(query string) []string {
	terms := significantWords(query)
	lower := strings.ToLower(query)
	var extra []string
	if strings.Contains(lower, "action") || strings.Contains(lower, "follow") || strings.Contains(lower, "risk") {
		extra = append(extra, "deployment", "preview", "sandbox", "customer", "feedback", "builder", "secret", "secrets", "staging")
	}
	if strings.Contains(lower, "workflow") || strings.Contains(lower, "customer") {
		extra = append(extra, "workflow", "workflows", "approval", "approvals", "customer", "customers", "coca-cola", "verizon")
	}
	if strings.Contains(lower, "preview") || strings.Contains(lower, "sandbox") {
		extra = append(extra, "preview", "sandbox", "sandboxes", "auth", "deployment", "session", "worker", "project")
	}
	if strings.Contains(lower, "staging") || strings.Contains(lower, "production") || strings.Contains(lower, "demo") {
		extra = append(extra, "staging", "production", "prod", "demo", "demos", "deployment", "apps")
	}
	for _, w := range extra {
		if len(w) < 4 {
			continue
		}
		terms = append(terms, w)
	}
	return dedupeStrings(terms)
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func wordOverlapScore(text string, terms []string) int {
	tokens := make(map[string]struct{})
	for _, token := range significantWords(text) {
		tokens[token] = struct{}{}
	}
	score := 0
	for _, term := range terms {
		if _, ok := tokens[term]; ok {
			score++
		}
	}
	return score
}

func compact(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	if max < 4 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func startsWithNormalized(text, prefix string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.Join(strings.Fields(s), " "))
	}
	return strings.HasPrefix(norm(text), norm(prefix))
}
