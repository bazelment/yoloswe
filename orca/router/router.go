// Package router classifies owner messages and selects an orca destination.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	Destination         Destination `json:"destination"`
	Confidence          float64     `json:"confidence"`
	NeedsClarification  bool        `json:"needs_clarification"`
	IsStandingRule      bool        `json:"is_standing_rule"`
	AnswersOpenQuestion bool        `json:"answers_open_question"`
	ClarifyingQuestion  string      `json:"clarifying_question,omitempty"`
	Echo                string      `json:"echo,omitempty"`
	Parts               []Decision  `json:"parts,omitempty"`
}

// Router classifies one owner message. Implementations must not mutate input.
type Router interface {
	Route(context.Context, string) (Decision, error)
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
	parts := splitParts(text)
	if len(parts) > 1 {
		out := Decision{Destination: Concierge, Confidence: 1, Parts: make([]Decision, 0, len(parts))}
		for _, p := range parts {
			d, err := r.routeOne(ctx, p)
			if err != nil {
				return Decision{}, err
			}
			out.Parts = append(out.Parts, d)
		}
		if r.Logger != nil {
			if err := r.Logger.LogRouting(ctx, text, out); err != nil {
				return Decision{}, err
			}
		}
		return out, nil
	}
	return r.routeOne(ctx, text)
}
func (r FallbackRouter) routeOne(ctx context.Context, text string) (Decision, error) {
	if r.Client == nil {
		return Decision{}, errors.New("orca/router: nil model client")
	}
	req := model.Request{Model: r.Model, MaxTokens: 512, System: routingPrompt, Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentBlock{model.NewTextBlock(text)}}}}
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
	if !validDestination(d.Destination) {
		return Decision{}, fmt.Errorf("orca/router: invalid destination %q", d.Destination)
	}
	d = normalize(d, text, r.ConfidenceThreshold)
	if r.Logger != nil {
		if err := r.Logger.LogRouting(ctx, text, d); err != nil {
			return Decision{}, err
		}
	}
	return d, nil
}
func splitParts(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func normalize(d Decision, text string, threshold float64) Decision {
	if threshold == 0 {
		threshold = .75
	}
	if d.Confidence < threshold {
		d.NeedsClarification = true
		if strings.TrimSpace(d.ClarifyingQuestion) == "" {
			d.ClarifyingQuestion = "Which part should I handle first?"
		}
	}
	if d.Echo == "" && !d.NeedsClarification {
		d.Echo = oneLine(text)
	}
	return d
}
func validDestination(d Destination) bool {
	for _, x := range destinations {
		if d == x {
			return true
		}
	}
	return false
}

const routingPrompt = `Classify the owner message. Return JSON only with destination (one of concierge, answer-open-question, planner, memory, dispatcher, controller), confidence (0 to 1), needs_clarification, is_standing_rule, answers_open_question, and optional clarifying_question. Ask one short question when intent is ambiguous.`

// GeminiClient calls Gemini Decisions API (v1beta) and translates its choice response.
//
//nolint:govet // keep public client configuration grouped.
type GeminiClient struct {
	HTTPClient             *http.Client
	BaseURL, APIKey, Model string
	Logger                 Logger
	ConfidenceThreshold    float64
}

func (c GeminiClient) Route(ctx context.Context, text string) (Decision, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	modelName := c.Model
	if modelName == "" {
		modelName = "gemini-decision-1"
	}
	body := map[string]any{"model": modelName, "input": text, "choices": destinations, "questions": []string{"needs clarification?", "standing rule?", "answers an open question?"}}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1beta/decisions:create", strings.NewReader(string(b)))
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
	var wire struct {
		Decision `json:"decision"`
	}
	if err := json.NewDecoder(res.Body).Decode(&wire); err != nil {
		return Decision{}, err
	}
	if !validDestination(wire.Destination) {
		return Decision{}, fmt.Errorf("orca/router: invalid gemini destination %q", wire.Destination)
	}
	d := normalize(wire.Decision, text, c.ConfidenceThreshold)
	if c.Logger != nil {
		if err := c.Logger.LogRouting(ctx, text, d); err != nil {
			return Decision{}, err
		}
	}
	return d, nil
}
