// Package router selects the model that handles an owner message.
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

	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

type Handler string

const (
	Concierge  Handler = "concierge"
	Planner    Handler = "planner"
	Dispatcher Handler = "dispatcher"
)

var handlers = [...]Handler{Concierge, Planner, Dispatcher}

// Decision records the selected handler and backend evidence.
//
//nolint:govet
type Decision struct {
	Handler       Handler            `json:"handler"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Backend       string             `json:"backend"`
	Fallback      bool               `json:"fallback"`
}
type Router interface {
	Route(context.Context, string) (Decision, error)
}
type Logger interface {
	LogRouting(context.Context, string, Decision) error
}

func FormatInput(text, questions string) string {
	if questions == "" {
		return text
	}
	return text + "\nOpen questions:\n" + questions
}

const conciergeCriterion = "Questions about current run state or policy; standing rules, durable preferences or corrections; pause, resume or stop execution. Also unclear messages or mixed independent requests needing front-desk handling."
const plannerCriterion = "Changes to an existing goal, plan, scope, task order, timing or acceptance criteria, including dropping work or changing duration. A one-time plan update is planner, not a standing rule."
const dispatcherCriterion = "Do work now: assign, fund, retry, merge, review, or steer a lane or PR. Also an owner reply answering a listed open question; the Open questions block is context, not an additional request."

// routingRubric is shared by both model backends.
const routingRubric = "Choose which model handles the owner message. concierge: " + conciergeCriterion + " planner: " + plannerCriterion + " dispatcher: " + dispatcherCriterion
const marginCut = .15

func validHandler(h Handler) bool {
	for _, candidate := range handlers {
		if h == candidate {
			return true
		}
	}
	return false
}
func validProbability(p float64) bool { return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1 }
func applyMargin(d Decision) Decision {
	first, second := 0.0, 0.0
	for _, p := range d.Probabilities {
		if p > first {
			second, first = first, p
		} else if p > second {
			second = p
		}
	}
	if first-second < marginCut {
		d.Handler = Concierge
	}
	return d
}

// LLMRouter asks only for a handler and confidence.
type LLMRouter struct {
	Client model.ModelClient
	Model  string
}

func (r LLMRouter) Route(ctx context.Context, text string) (Decision, error) {
	if r.Client == nil {
		return Decision{}, errors.New("orca/router: nil model client")
	}
	resp, err := r.Client.CreateMessage(ctx, model.Request{Model: r.Model, MaxTokens: 2048, System: routingRubric + ` Return JSON only: {"handler":"concierge|planner|dispatcher","confidence":0.0}.`, Messages: []model.Message{{Role: model.RoleUser, Content: []model.ContentBlock{model.NewTextBlock(text)}}}})
	if err != nil {
		return Decision{}, err
	}
	raw := strings.TrimSpace(resp.Text())
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	var answer struct {
		Confidence *float64 `json:"confidence"`
		Handler    Handler  `json:"handler"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &answer); err != nil {
		return Decision{}, errors.New("orca/router: invalid LLM response")
	}
	if !validHandler(answer.Handler) || answer.Confidence == nil || !validProbability(*answer.Confidence) {
		return Decision{}, errors.New("orca/router: invalid LLM handler or confidence")
	}
	d := Decision{Handler: answer.Handler, Confidence: *answer.Confidence, Backend: "llm"}
	if d.Confidence < .5 {
		d.Handler = Concierge
	}
	return d, nil
}

// GeminiRouter makes one Decisions choice request.
//
//nolint:govet
type GeminiRouter struct {
	HTTPClient      *http.Client
	BaseURL, APIKey string
}

func (c GeminiRouter) Route(ctx context.Context, text string) (Decision, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	body := map[string]any{"model": "models/gemini-decision-1", "input": text, "questions": map[string]any{"handler": map[string]any{"type": "choice", "instructions": routingRubric, "criteria": map[string]string{"concierge": conciergeCriterion, "planner": plannerCriterion, "dispatcher": dispatcherCriterion}}}}
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
		return Decision{}, errors.New("orca/router: Gemini transport failed")
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return Decision{}, fmt.Errorf("orca/router: Gemini status %d", res.StatusCode)
	}
	var wire struct {
		Answers map[string]struct {
			Probabilities map[string]float64
			Confidence    *float64
			Type          string
			Choice        Handler
		}
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&wire); err != nil {
		return Decision{}, errors.New("orca/router: invalid Gemini response")
	}
	a, ok := wire.Answers["handler"]
	if !ok || a.Type != "choice" || !validHandler(a.Choice) || a.Confidence == nil || !validProbability(*a.Confidence) || len(a.Probabilities) != 3 {
		return Decision{}, errors.New("orca/router: invalid Gemini answer")
	}
	sum := 0.0
	for _, h := range handlers {
		p, ok := a.Probabilities[string(h)]
		if !ok || !validProbability(p) {
			return Decision{}, errors.New("orca/router: invalid Gemini probabilities")
		}
		sum += p
	}
	if math.Abs(sum-1) > .01 {
		return Decision{}, errors.New("orca/router: invalid Gemini probability sum")
	}
	return applyMargin(Decision{Handler: a.Choice, Confidence: *a.Confidence, Probabilities: a.Probabilities, Backend: "gemini"}), nil
}

type selectedRouter struct {
	primary Router
	llm     Router
	logger  Logger
}

func (r selectedRouter) Route(ctx context.Context, text string) (Decision, error) {
	var d Decision
	var err error
	if r.primary != nil {
		d, err = r.primary.Route(ctx, text)
	}
	fallback := r.primary != nil && err != nil
	if r.primary == nil || err != nil {
		d, err = r.llm.Route(ctx, text)
	}
	if err != nil {
		d = Decision{Handler: Concierge, Backend: "concierge"}
		fallback = true
	}
	d.Fallback = fallback
	if r.logger != nil {
		if err := r.logger.LogRouting(ctx, text, d); err != nil {
			return d, err
		}
	}
	return d, nil
}
func Select(client model.ModelClient, name, key, baseURL string, logger Logger) Router {
	r := selectedRouter{llm: LLMRouter{Client: client, Model: name}, logger: logger}
	if key != "" {
		r.primary = GeminiRouter{APIKey: key, BaseURL: baseURL}
	}
	return r
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
	if d.Backend == "gemini" {
		l.Provider = "gemini"
		l.Model = "gemini-decision-1"
	}
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
	_, err = l.Store.Decisions().Append(ctx, store.Decision{RunID: l.RunID, Actor: "router", Category: store.DecisionJudgment, Tool: tool, TraceID: ev.ID, Input: text, Output: string(raw), Args: args, Result: string(d.Handler), Provider: l.Provider, Model: l.Model, PromptVersion: "router-v3"})
	return err
}
