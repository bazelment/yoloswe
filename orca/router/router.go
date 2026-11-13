// Package router classifies owner messages and selects an orca destination.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/store"

	"github.com/bazelment/yoloswe/orca/model"
)

// Destination is the owner-chat handoff target.
type Destination string

const (
	Concierge          Destination = "concierge"
	AnswerOpenQuestion Destination = "answer-open-question"
	Planner            Destination = "planner"
	Memory             Destination = "memory"
	Dispatcher         Destination = "dispatcher"
	Controller         Destination = "controller"
)

var destinations = []Destination{Concierge, AnswerOpenQuestion, Planner, Memory, Dispatcher, Controller}

// Decision is the structured routing result. Confidence is in [0,1].
//
//nolint:govet // preserve the routing contract's field grouping.
type Decision struct {
	Confidences         map[string]float64 `json:"confidences,omitempty"`
	Text                string             `json:"text,omitempty"`
	QuestionID          string             `json:"question_id,omitempty"`
	Destination         Destination        `json:"destination"`
	Confidence          float64            `json:"confidence"`
	NeedsClarification  bool               `json:"needs_clarification"`
	IsStandingRule      bool               `json:"is_standing_rule"`
	AnswersOpenQuestion bool               `json:"answers_open_question"`
	ClarifyingQuestion  string             `json:"clarifying_question,omitempty"`
	Echo                string             `json:"echo,omitempty"`
	Parts               []Decision         `json:"parts,omitempty"`
}

// Router classifies one owner message. Implementations must not mutate input.
type Router interface {
	Route(context.Context, string) (Decision, error)
}

// FormatInput appends the current open-question context in the same shape used
// by production chat routing and evaluation.
func FormatInput(text, openQuestions string) string {
	if openQuestions == "" {
		return text
	}
	return text + "\nOpen questions:\n" + openQuestions
}

// Logger records every routing decision, including low-confidence decisions.
type Logger interface {
	LogRouting(context.Context, string, Decision) error
}

// FallbackRouter uses the existing model client with a strict JSON response.
//
//nolint:govet // keep public configuration fields grouped.
type FallbackRouter struct {
	Client              model.ModelClient
	Model               string
	Logger              Logger
	ConfidenceThreshold float64
}

func (r FallbackRouter) Route(ctx context.Context, text string) (Decision, error) {
	return r.routeOne(ctx, text)
}
func (r FallbackRouter) routeOne(ctx context.Context, text string) (Decision, error) {
	if r.Client == nil {
		return Decision{}, errors.New("orca/router: nil model client")
	}
	req := model.Request{Model: r.Model, MaxTokens: 2048, System: routingPrompt, Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentBlock{model.NewTextBlock(text)}}}}
	resp, err := r.Client.CreateMessage(ctx, req)
	if err != nil {
		return Decision{}, err
	}
	var d Decision
	raw := strings.TrimSpace(resp.Text())
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &d); err != nil {
		return Decision{}, fmt.Errorf("orca/router: invalid structured response: %w", err)
	}
	if len(d.Parts) == 0 && !validDestination(d.Destination) {
		return Decision{}, fmt.Errorf("orca/router: invalid destination %q", d.Destination)
	}
	d = normalize(d, text, r.ConfidenceThreshold, hasOpenQuestionContext(text))
	if r.Logger != nil {
		if err := r.Logger.LogRouting(ctx, text, d); err != nil {
			return Decision{}, err
		}
	}
	return d, nil
}
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func normalize(d Decision, text string, threshold float64, openQuestion bool) Decision {
	if threshold == 0 {
		threshold = .75
	}
	// A reply can only answer a question that is present in the supplied
	// thread context. This is enforced after the model response as well as in
	// the prompt so an over-eager model cannot invent an open question.
	if !openQuestion && d.Destination == AnswerOpenQuestion {
		d.Destination = Concierge
		d.AnswersOpenQuestion = false
		d.QuestionID = ""
	}
	if len(d.Parts) > 0 {
		d.Confidence = 1
		for i := range d.Parts {
			if d.Parts[i].Confidence < d.Confidence {
				d.Confidence = d.Parts[i].Confidence
			}
		}
	}
	if math.IsNaN(d.Confidence) || math.IsInf(d.Confidence, 0) || d.Confidence < 0 || d.Confidence > 1 {
		d.Confidence = 0
	}
	if d.NeedsClarification || d.Confidence < threshold {
		d.NeedsClarification = true
		if strings.TrimSpace(d.ClarifyingQuestion) == "" {
			d.ClarifyingQuestion = "Which part should I handle first?"
		}
	}
	if d.Echo == "" && !d.NeedsClarification {
		d.Echo = oneLine(text)
	}
	for i := range d.Parts {
		d.Parts[i] = normalize(d.Parts[i], d.Parts[i].Text, threshold, openQuestion)
		if !validDestination(d.Parts[i].Destination) || d.Parts[i].Text == "" {
			d.Parts[i].NeedsClarification = true
			d.Parts[i].ClarifyingQuestion = "What should I do with this part?"
		}
		if d.Parts[i].NeedsClarification {
			d.NeedsClarification = true
			d.ClarifyingQuestion = d.Parts[i].ClarifyingQuestion
		}
	}
	return d
}

func hasOpenQuestionContext(text string) bool {
	const marker = "Open questions:"
	i := strings.Index(text, marker)
	return i >= 0 && strings.TrimSpace(text[i+len(marker):]) != ""
}
func validDestination(d Destination) bool {
	for _, x := range destinations {
		if d == x {
			return true
		}
	}
	return false
}

const routingPrompt = `Classify the owner message. Return JSON only with destination (one of concierge, answer-open-question, planner, memory, dispatcher, controller), confidence (0 to 1), needs_clarification, is_standing_rule, answers_open_question, and optional clarifying_question. The input may include a trailing Open questions section containing recent thread context. answer-open-question is allowed only when that section contains an actual open question and the owner names or clearly replies to it; when the section is absent or empty, answer-open-question is not a candidate. Do not infer or invent an open question. Apply this priority rubric: direct questions about run policy or current state are concierge; a change to an existing goal, plan, order, timing, or scope is planner; a standing rule or durable fact for future work is memory; a command to assign, fund, retry, merge, or steer work is dispatcher; only an explicit pause, resume, stop, hold, or release of execution is controller. Do not use controller for ordinary plan edits, and do not use memory for a one-time decision or plan update. Set needs_clarification only when the requested destination or action is genuinely ambiguous, not merely because the message is short. For multiple independent requests return parts, each containing its exact text and all routing fields; do not split code or prose just because it contains punctuation. Include question_id only when answering a specific supplied open question. A trailing Open questions section is context, not a request to split or dispatch.`

// GeminiClient calls Gemini Decisions API (v1beta) and translates its choice response.
//
//nolint:govet // keep public client configuration grouped.
type GeminiClient struct {
	Segmenter              Router
	HTTPClient             *http.Client
	BaseURL, APIKey, Model string
	Logger                 Logger
	ConfidenceThreshold    float64
}

func (c GeminiClient) Route(ctx context.Context, text string) (Decision, error) {
	openQuestion := hasOpenQuestionContext(text)
	var parts []Decision
	questionID := ""
	if c.Segmenter != nil {
		seg, err := c.Segmenter.Route(ctx, text)
		if err != nil {
			return Decision{}, err
		}
		parts = seg.Parts
		questionID = seg.QuestionID
	}
	var out Decision
	if len(parts) > 0 {
		out = Decision{Destination: Concierge, Confidence: 1}
		for i := range parts {
			part := parts[i]
			d, err := c.routeOneWithContext(ctx, part.Text, openQuestion)
			if err != nil {
				return Decision{}, err
			}
			d.Text = part.Text
			if d.Destination == AnswerOpenQuestion {
				d.QuestionID = part.QuestionID
			} else {
				d.QuestionID = ""
			}
			out.Parts = append(out.Parts, d)
		}
		out = normalize(out, text, c.ConfidenceThreshold, hasOpenQuestionContext(text))
	} else {
		d, err := c.routeOne(ctx, text)
		if err != nil {
			return Decision{}, err
		}
		out = d
		out.QuestionID = questionID
	}
	if c.Logger != nil {
		if err := c.Logger.LogRouting(ctx, text, out); err != nil {
			return Decision{}, err
		}
	}
	return out, nil
}

// The wire schema follows google-genai's generated decisions types:
// https://github.com/googleapis/python-genai/tree/main/google/genai/_gaos/types/decisions
func (c GeminiClient) routeOne(ctx context.Context, text string) (Decision, error) {
	return c.routeOneWithContext(ctx, text, hasOpenQuestionContext(text))
}

func (c GeminiClient) routeOneWithContext(ctx context.Context, text string, openQuestion bool) (Decision, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	modelName := c.Model
	if modelName == "" {
		modelName = "gemini-decision-1"
	}
	if !strings.HasPrefix(modelName, "models/") {
		modelName = "models/" + modelName
	}
	criteria := map[string]string{"concierge": "Answer a question about the run", "answer-open-question": "Answer a question waiting on the owner", "planner": "Change the goal, plan or task descriptions", "memory": "Save a standing rule, correction or fact", "dispatcher": "Add or steer work", "controller": "Pause, resume or stop the run"}
	questions := map[string]any{"destination": map[string]any{"type": "choice", "instructions": "Choose the destination for the owner's request. Open questions are context, not additional requests.", "criteria": criteria}}
	for key, instructions := range map[string]string{"needs_clarification": "Does the owner's intent require clarification before taking action?", "is_standing_rule": "Is the request a standing rule that should apply in future?", "answers_open_question": "Does the input answer an open question?"} {
		questions[key] = map[string]any{"type": "noul", "instructions": instructions}
	}
	body := map[string]any{"model": modelName, "input": text, "questions": questions}
	raw, err := json.Marshal(body)
	if err != nil {
		return Decision{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1beta/decisions", strings.NewReader(string(raw)))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-goog-api-key", c.APIKey)
	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return Decision{}, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return Decision{}, fmt.Errorf("orca/router: gemini status %s", res.Status)
	}
	//nolint:govet // response fields follow the wire format
	var wire struct {
		Answers map[string]struct {
			Type          string
			Choice        string
			Confidence    float64
			Noul          float64
			Probabilities map[string]float64
		}
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&wire); err != nil {
		return Decision{}, err
	}
	choice, ok := wire.Answers["destination"]
	if !ok || choice.Type != "choice" || !validDestination(Destination(choice.Choice)) {
		return Decision{}, fmt.Errorf("orca/router: invalid Gemini destination answer")
	}
	d := Decision{Destination: Destination(choice.Choice), Confidence: choice.Confidence, Confidences: map[string]float64{"destination": choice.Confidence}}
	for _, key := range []string{"needs_clarification", "is_standing_rule", "answers_open_question"} {
		a, ok := wire.Answers[key]
		if !ok || a.Type != "noul" || a.Noul < 0 || a.Noul > 1 || a.Confidence < 0 || a.Confidence > 1 {
			return Decision{}, fmt.Errorf("orca/router: invalid Gemini %s answer", key)
		}
		d.Confidences[key] = a.Confidence
		yes := a.Noul >= .5
		switch key {
		case "needs_clarification":
			d.NeedsClarification = yes
		case "is_standing_rule":
			d.IsStandingRule = yes
		case "answers_open_question":
			d.AnswersOpenQuestion = yes
		}
	}
	return normalize(d, text, c.ConfidenceThreshold, openQuestion), nil
}

// Select uses Decisions when configured and otherwise the concierge model.
func Select(client model.ModelClient, modelName, key, baseURL string, logger Logger) Router {
	// Decisions is selected only when credentials are present. A base URL alone
	// must not redirect owner messages to an unauthenticated third party.
	if key != "" {
		var segmenter Router
		if client != nil {
			segmentLog := logger
			if log, ok := logger.(StoreLogger); ok {
				log.Tool = "routing.segment"
				segmentLog = log
			}
			segmenter = FallbackRouter{Client: client, Model: modelName, Logger: segmentLog}
		}
		if log, ok := logger.(StoreLogger); ok {
			log.Provider = "gemini"
			log.Model = "gemini-decision-1"
			logger = log
		}
		return GeminiClient{APIKey: key, BaseURL: baseURL, Logger: logger, Segmenter: segmenter}
	}
	return FallbackRouter{Client: client, Model: modelName, Logger: logger}
}

// StoreLogger writes routing records through the shared causal decision log.
type StoreLogger struct {
	Tool     string
	Store    store.Store
	RunID    string
	Provider string
	Model    string
}

func (l StoreLogger) LogRouting(ctx context.Context, text string, d Decision) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	ev, err := l.Store.Events().Append(ctx, store.Event{RunID: l.RunID, Kind: store.EventTimer, Class: store.EventClassAuto, Source: "router.audit", Summary: text})
	if err != nil {
		return err
	}
	args, err := json.Marshal(map[string]any{"event_id": ev.ID, "routing": d})
	if err != nil {
		return err
	}
	tool := l.Tool
	if tool == "" {
		tool = "routing"
	}
	_, err = l.Store.Decisions().Append(ctx, store.Decision{RunID: l.RunID, Actor: "router", Category: store.DecisionJudgment, Tool: tool, TraceID: ev.ID, Input: text, Output: string(raw), Args: args, Result: string(d.Destination), Provider: l.Provider, Model: l.Model, PromptVersion: "router-v2"})
	return err
}
