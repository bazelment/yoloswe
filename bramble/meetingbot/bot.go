package meetingbot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/displaytext"
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
	highWater  map[string]int
}

// New creates a meeting bot. If client is nil, ProviderAgentClient is used.
func New(client AgentClient, cfg Config) *Bot {
	cfg = normalizeConfig(cfg)
	return newWithConfig(client, cfg)
}

// NewValidated creates a bot after enforcing profile-specific safety rules.
func NewValidated(client AgentClient, cfg Config) (*Bot, error) {
	cfg = normalizeConfig(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newWithConfig(client, cfg), nil
}

func newWithConfig(client AgentClient, cfg Config) *Bot {
	if client == nil {
		client = ProviderAgentClient{}
	}
	return &Bot{
		client:     client,
		cfg:        cfg,
		researched: make(map[string]struct{}),
		highWater:  make(map[string]int),
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
		if b.cfg.ResearchScheduler == nil {
			return errAutoResearchScheduler
		}
		return b.enqueueResearch(ctx)
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
	snapshot := b.researchSnapshot()
	if len(snapshot.Events) == 0 {
		return nil
	}
	topics := candidateTopics(snapshot.Events, b.cfg.MaxResearchTopics)
	jobs := b.researchJobs(snapshot.Events, topics)
	rows, err := b.runResearchJobs(ctx, jobs, snapshot)
	b.publishEvidence(rows)
	if err != nil {
		return err
	}
	return nil
}

// RunResearch implements ResearchExecutor for in-process replay and queue workers.
func (b *Bot) RunResearch(ctx context.Context, work ResearchWork) ([]Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	job := work.Job
	events := append([]MeetingEvent(nil), work.Snapshot.Events...)
	if len(events) == 0 {
		events = b.snapshotEvents()
	}
	scopes := append([]ResearchScope(nil), job.Scopes...)
	if len(scopes) == 0 {
		scopes = append([]ResearchScope(nil), b.cfg.ResearchScopes...)
	}
	return b.researchTopicRows(ctx, job.Topic, scopes, events, job.TranscriptStart, job.TranscriptEnd)
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

func (b *Bot) researchSnapshot() ResearchSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return ResearchSnapshot{
		Events: append([]MeetingEvent(nil), b.events...),
	}
}

func (b *Bot) enqueueResearch(ctx context.Context) error {
	snapshot := b.researchSnapshot()
	if len(snapshot.Events) == 0 {
		return nil
	}
	topics := candidateTopics(snapshot.Events, b.cfg.MaxResearchTopics)
	for _, job := range b.researchJobs(snapshot.Events, topics) {
		if err := b.cfg.ResearchScheduler.Enqueue(ctx, ResearchWork{Job: job, Snapshot: snapshot}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) researchJobs(events []MeetingEvent, topics []Topic) []ResearchJob {
	endIndex := lastEventIndex(events)
	if endIndex < 0 {
		return nil
	}
	jobs := make([]ResearchJob, 0, len(topics)*len(b.cfg.ResearchScopes))
	for _, topic := range topics {
		for _, scope := range b.cfg.ResearchScopes {
			startIndex, ok := b.claimResearchRange(scope, topic.Name, endIndex)
			if !ok {
				continue
			}
			jobs = append(jobs, ResearchJob{
				Topic:           topic.Name,
				Scopes:          []ResearchScope{scope},
				TranscriptStart: startIndex,
				TranscriptEnd:   endIndex,
			})
		}
	}
	return jobs
}

func (b *Bot) claimResearchRange(scope ResearchScope, topic string, endIndex int) (int, bool) {
	key := researchRangeKey(scope, topic)
	b.mu.Lock()
	defer b.mu.Unlock()
	previous, ok := b.highWater[key]
	if ok && previous >= endIndex {
		return 0, false
	}
	startIndex := 0
	if ok {
		startIndex = previous + 1
	}
	b.highWater[key] = endIndex
	return startIndex, true
}

func (b *Bot) runResearchJobs(ctx context.Context, jobs []ResearchJob, snapshot ResearchSnapshot) ([]Evidence, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	concurrency := boundedConcurrency(b.cfg.ResearchConcurrency, len(jobs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var rows []Evidence
	var firstErr error
	stopped := false
	for _, job := range jobs {
		if stopped {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			stopped = true
		}
		if stopped {
			break
		}
		wg.Add(1)
		go func(job ResearchJob) {
			defer wg.Done()
			defer func() { <-sem }()
			jobRows, err := b.RunResearch(ctx, ResearchWork{Job: job, Snapshot: snapshot})
			mu.Lock()
			defer mu.Unlock()
			rows = append(rows, jobRows...)
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}(job)
	}
	wg.Wait()
	if firstErr != nil {
		return rows, firstErr
	}
	if err := ctx.Err(); err != nil {
		return rows, err
	}
	return rows, nil
}

func (b *Bot) researchTopicRows(ctx context.Context, topic string, scopes []ResearchScope, events []MeetingEvent, jobStartIndex, jobEndIndex int) ([]Evidence, error) {
	events = eventsInRange(events, jobStartIndex, jobEndIndex)
	snippets := selectSnippets(events, topic, b.cfg.MaxSnippetsPerPrompt)
	if len(snippets) == 0 {
		return nil, nil
	}
	startIndex, endIndex := eventIndexRange(snippets)
	if jobStartIndex >= 0 && (startIndex < 0 || jobStartIndex < startIndex) {
		startIndex = jobStartIndex
	}
	if jobEndIndex >= 0 && jobEndIndex > endIndex {
		endIndex = jobEndIndex
	}

	rows := make([]Evidence, 0, len(scopes))
	for _, scope := range scopes {
		if err := ctx.Err(); err != nil {
			return rows, err
		}
		key := evidenceKey(scope, topic, startIndex, endIndex)
		if b.isResearched(key) {
			continue
		}
		req := b.researchRequest(scope, topic, snippets)
		resp, err := b.client.Run(ctx, req)
		status := EvidenceStatusSuccess
		errorText := ""
		if err != nil {
			// Public-web research may be unavailable in some local runs. Cache
			// the miss as evidence so downstream summaries know not to invent it.
			resp.Text = fmt.Sprintf("Research unavailable for %s/%s: %v", scope, topic, err)
			status = EvidenceStatusFailed
			errorText = err.Error()
		}
		text := strings.TrimSpace(resp.Text)
		if text == "" {
			text = fmt.Sprintf("No %s findings returned for %q.", scope, topic)
			status = EvidenceStatusEmpty
		}
		rows = append(rows, Evidence{
			CreatedAt:  b.cfg.Now(),
			Scope:      scope,
			Topic:      topic,
			Text:       text,
			Sources:    evidenceSources(scope, snippets),
			Status:     status,
			Error:      errorText,
			StartIndex: startIndex,
			EndIndex:   endIndex,
		})
	}
	return rows, nil
}

func (b *Bot) publishEvidence(rows []Evidence) {
	if len(rows) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range rows {
		ev := rows[i]
		key := evidenceKey(ev.Scope, ev.Topic, ev.StartIndex, ev.EndIndex)
		if _, done := b.researched[key]; done {
			continue
		}
		b.evidence = append(b.evidence, ev)
		b.researched[key] = struct{}{}
	}
}

func (b *Bot) isResearched(key string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, done := b.researched[key]
	return done
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
		permission = "plan"
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
	return b.answerQuestion(ctx, question, nil)
}

// AnswerQuestionStream reports the live opening before waiting for refinement.
func (b *Bot) AnswerQuestionStream(ctx context.Context, question string, stream AnswerStream) (Answer, error) {
	return b.answerQuestion(ctx, question, stream)
}

func (b *Bot) answerQuestion(ctx context.Context, question string, stream AnswerStream) (Answer, error) {
	start := time.Now()
	events := b.snapshotEvents()
	snippets := selectSnippets(events, question, b.cfg.MaxSnippetsPerPrompt)
	evidence := b.matchEvidence(question)

	opening := immediateOpening(question, snippets, evidence)
	openingLatency := time.Since(start)
	if stream != nil {
		if err := stream.OnOpening(ctx, opening, b.cfg.Now()); err != nil {
			return Answer{}, err
		}
	}

	req := AgentRequest{
		Role:           RoleFastAnswer,
		Model:          b.cfg.FastAnswerModel,
		Question:       question,
		Opening:        opening,
		Effort:         b.cfg.FastAnswerEffort,
		Timeout:        b.cfg.FastAnswerTimeout,
		WorkDir:        b.cfg.WorkDir,
		PermissionMode: "plan",
		SystemPrompt:   roleSystemPrompt(RoleFastAnswer),
		Prompt:         buildAnswerPrompt(question, opening, snippets, evidence),
	}
	resp, err := b.client.Run(ctx, req)
	text := strings.TrimSpace(resp.Text)
	errorText := ""
	if err != nil || text == "" {
		if err != nil {
			errorText = err.Error()
		}
		text = fallbackAnswer(question, opening, snippets, evidence)
	}
	if !startsWithNormalized(text, opening) {
		text = opening + "\n\n" + text
	}
	validation := validateAnswer(opening, text, snippets, evidence)
	status := validation.Status
	if errorText != "" {
		status = OutputStatusDegraded
		validation = ValidationResult{
			Status: OutputStatusDegraded,
			Reason: "provider failure; returned grounded fallback",
		}
	}
	if validation.Status == OutputStatusInvalid {
		fallback := fallbackAnswer(question, opening, snippets, evidence)
		text = degradedOutput(validation.Reason, fallback, validation.MissingInputs)
		status = OutputStatusDegraded
		validation.Status = OutputStatusDegraded
	}
	answer := Answer{
		Question:                   question,
		Opening:                    opening,
		Text:                       text,
		Model:                      firstNonEmpty(resp.Model, b.cfg.FastAnswerModel),
		Error:                      errorText,
		OpeningReadinessLatency:    openingLatency,
		TimeToFinalValidatedAnswer: time.Since(start),
		Status:                     status,
		Validation:                 validation,
		Evidence:                   evidence,
		ResearchRefs:               evidenceRefList(evidence),
	}
	if stream != nil {
		if err := stream.OnFinal(ctx, answer, b.cfg.Now()); err != nil {
			return Answer{}, err
		}
	}
	return answer, nil
}

// SummarizeMeeting produces a post-meeting synthesis cross-referenced with
// cached research.
func (b *Bot) SummarizeMeeting(ctx context.Context) (Summary, error) {
	start := time.Now()
	events := b.snapshotEvents()
	evidence := b.Evidence()
	coverage := summaryCoverage(events, candidateTopics(events, b.cfg.MaxResearchTopics), b.cfg.ResearchScopes, evidence)
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
	errorText := ""
	if err != nil || text == "" {
		if err != nil {
			errorText = err.Error()
		}
		text = fallbackSummary(events, evidence)
	}
	validation := validateSummary(text, coverage)
	status := validation.Status
	if errorText != "" {
		status = OutputStatusDegraded
		validation = ValidationResult{
			Status: OutputStatusDegraded,
			Reason: "provider failure; returned grounded fallback summary",
		}
	}
	if validation.Status == OutputStatusInvalid {
		fallback := fallbackSummary(events, evidence)
		text = degradedOutput(validation.Reason, fallback, validation.MissingInputs)
		status = OutputStatusDegraded
		validation.Status = OutputStatusDegraded
	}
	return Summary{
		Text:       text,
		Model:      firstNonEmpty(resp.Model, b.cfg.SummaryModel),
		Latency:    time.Since(start),
		Error:      errorText,
		Status:     status,
		Validation: validation,
		Evidence:   evidence,
		Coverage:   coverage,
	}, nil
}

func (b *Bot) matchEvidence(question string) []Evidence {
	terms := queryTerms(question)
	b.mu.RLock()
	defer b.mu.RUnlock()
	var scored []struct {
		ev    Evidence
		score int
	}
	for i := range b.evidence {
		ev := b.evidence[i]
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
	for i := range evidence {
		ev := evidence[i]
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
	for i := range evidence {
		ev := evidence[i]
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
	for i := range evidence {
		ev := evidence[i]
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
		"agent runtime", "app upgrade", "builder smoke", "cloud run", "customer demo",
		"deployment", "feedback channel", "repository app", "model gateway", "microvm",
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
		extra = append(extra, "workflow", "workflows", "approval", "approvals", "customer", "customers")
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
	return dedupe(terms)
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
	return displaytext.Truncate(s, max)
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

func evidenceKey(scope ResearchScope, topic string, startIndex, endIndex int) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d", scope, strings.ToLower(strings.TrimSpace(topic)), startIndex, endIndex)
}

func researchRangeKey(scope ResearchScope, topic string) string {
	return fmt.Sprintf("%s\x00%s", scope, strings.ToLower(strings.TrimSpace(topic)))
}

func eventsInRange(events []MeetingEvent, startIndex, endIndex int) []MeetingEvent {
	if startIndex < 0 && endIndex < 0 {
		return append([]MeetingEvent(nil), events...)
	}
	out := make([]MeetingEvent, 0, len(events))
	for _, event := range events {
		if startIndex >= 0 && event.Index < startIndex {
			continue
		}
		if endIndex >= 0 && event.Index > endIndex {
			continue
		}
		out = append(out, event)
	}
	return out
}

func boundedConcurrency(configured, total int) int {
	if total <= 0 {
		return 1
	}
	if configured <= 0 || configured > total {
		return total
	}
	return configured
}

func eventIndexRange(events []MeetingEvent) (int, int) {
	if len(events) == 0 {
		return -1, -1
	}
	startIndex := events[0].Index
	endIndex := events[0].Index
	for _, e := range events[1:] {
		if e.Index < startIndex {
			startIndex = e.Index
		}
		if e.Index > endIndex {
			endIndex = e.Index
		}
	}
	return startIndex, endIndex
}

func lastEventIndex(events []MeetingEvent) int {
	if len(events) == 0 {
		return -1
	}
	return events[len(events)-1].Index
}

func containsScope(scopes []ResearchScope, want ResearchScope) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func validateAnswer(opening, text string, snippets []MeetingEvent, evidence []Evidence) ValidationResult {
	var missing []string
	if strings.TrimSpace(text) == "" {
		missing = append(missing, "answer text")
	}
	if strings.TrimSpace(opening) == "" {
		missing = append(missing, "opening")
	}
	if !openingHasGrounding(opening) {
		missing = append(missing, "grounded opening anchor")
	}
	if len(missing) > 0 {
		return ValidationResult{
			Status:        OutputStatusInvalid,
			Reason:        "answer failed deterministic grounding checks",
			MissingInputs: missing,
		}
	}
	if len(snippets) == 0 && len(evidence) == 0 {
		return ValidationResult{
			Status: OutputStatusDegraded,
			Reason: "no transcript snippets or research evidence matched the question",
		}
	}
	return ValidationResult{Status: OutputStatusNormal}
}

func validateSummary(text string, coverage []SummaryCoverage) ValidationResult {
	var missing []string
	lower := strings.ToLower(text)
	for _, section := range requiredSummarySections {
		if !strings.Contains(lower, section) {
			missing = append(missing, section)
		}
	}
	if strings.TrimSpace(text) == "" {
		missing = append(missing, "summary text")
	}
	if len(missing) > 0 {
		return ValidationResult{
			Status:        OutputStatusInvalid,
			Reason:        "summary is missing required sections",
			MissingInputs: missing,
		}
	}
	for _, item := range coverage {
		if item.State == CoverageFailed || item.State == CoverageStale || item.State == CoverageNotSearched {
			return ValidationResult{
				Status: OutputStatusDegraded,
				Reason: "summary has incomplete research coverage",
			}
		}
	}
	return ValidationResult{Status: OutputStatusNormal}
}

var requiredSummarySections = []string{"executive summary", "decisions", "action items", "risks/blockers", "background/context"}

func hasRequiredSummarySections(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, section := range requiredSummarySections {
		if !strings.Contains(lower, section) {
			return false
		}
	}
	return true
}

func openingHasGrounding(opening string) bool {
	lower := strings.ToLower(opening)
	return strings.Contains(opening, "[") ||
		strings.Contains(lower, "cached research") ||
		strings.Contains(lower, "no supporting meeting evidence") ||
		strings.Contains(lower, "no supporting evidence")
}

func degradedOutput(reason, fallback string, missing []string) string {
	var b strings.Builder
	b.WriteString("Status: degraded\n")
	if strings.TrimSpace(reason) == "" {
		reason = "validation did not pass"
	}
	fmt.Fprintf(&b, "Reason: %s\n", reason)
	b.WriteString("Grounded fallback:\n")
	b.WriteString(strings.TrimSpace(fallback))
	if len(missing) > 0 {
		b.WriteString("\n\nMissing evidence:\n")
		for _, item := range missing {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	return strings.TrimSpace(b.String())
}

func summaryCoverage(events []MeetingEvent, topics []Topic, scopes []ResearchScope, evidence []Evidence) []SummaryCoverage {
	if len(topics) == 0 || len(scopes) == 0 {
		return nil
	}
	out := make([]SummaryCoverage, 0, len(topics)*len(scopes))
	for _, topic := range topics {
		latest := latestTopicEventIndex(events, topic.Name)
		for _, scope := range scopes {
			item := SummaryCoverage{
				Topic:  topic.Name,
				Scope:  scope,
				State:  CoverageNotSearched,
				Reason: "no evidence row for selected topic/scope",
			}
			if ev, ok := bestEvidenceForTopicScope(evidence, topic.Name, scope); ok {
				item.State = CoverageFresh
				item.Reason = "evidence covers the latest matched topic range"
				if ev.Status == EvidenceStatusEmpty {
					item.State = CoverageEmpty
					item.Reason = "scope searched and returned no findings"
				}
				if ev.Status == EvidenceStatusFailed {
					item.State = CoverageFailed
					item.Reason = firstNonEmpty(ev.Error, "scope failed or was blocked")
				}
				if latest >= 0 && ev.EndIndex >= 0 && ev.EndIndex < latest && item.State == CoverageFresh {
					item.State = CoverageStale
					item.Reason = "evidence predates later transcript turns for the topic"
				}
			}
			out = append(out, item)
		}
	}
	return out
}

func bestEvidenceForTopicScope(evidence []Evidence, topic string, scope ResearchScope) (Evidence, bool) {
	topic = strings.ToLower(strings.TrimSpace(topic))
	var best Evidence
	found := false
	for i := range evidence {
		ev := evidence[i]
		if ev.Scope != scope || strings.ToLower(strings.TrimSpace(ev.Topic)) != topic {
			continue
		}
		if !found || ev.EndIndex > best.EndIndex || ev.CreatedAt.After(best.CreatedAt) {
			best = ev
			found = true
		}
	}
	return best, found
}

func latestTopicEventIndex(events []MeetingEvent, topic string) int {
	terms := queryTerms(topic)
	latest := -1
	for _, e := range events {
		if wordOverlapScore(strings.ToLower(e.Text+" "+e.Speaker), terms) > 0 && e.Index > latest {
			latest = e.Index
		}
	}
	return latest
}
