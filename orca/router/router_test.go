package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orca/store"

	"github.com/bazelment/yoloswe/orca/model"
)

type fake struct{ text string }

func (f fake) CreateMessage(context.Context, model.Request) (*model.Response, error) {
	return &model.Response{Message: model.Message{Content: []model.ContentBlock{model.NewTextBlock(f.text)}}}, nil
}
func TestFallbackRoutesFiveLabels(t *testing.T) {
	for _, x := range []struct {
		in string
		d  Destination
	}{{"show status", Controller}, {"plan this", Planner}, {"remember use tabs", Memory}, {"dispatch item", Dispatcher}, {"answer the open question", AnswerOpenQuestion}} {
		r := FallbackRouter{Client: fake{`{"destination":"` + string(x.d) + `","confidence":0.99}`}}
		got, e := r.Route(context.Background(), x.in)
		if e != nil || got.Destination != x.d {
			t.Fatalf("%q: %+v %v", x.in, got, e)
		}
	}
}
func TestFallbackClarifies(t *testing.T) {
	r := FallbackRouter{Client: fake{`{"destination":"concierge","confidence":0.2}`}}
	d, _ := r.Route(context.Background(), "hmm")
	if !d.NeedsClarification || d.ClarifyingQuestion == "" {
		t.Fatal(d)
	}
}

type errorTransport struct {
	t *testing.T
}

func (tr errorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.RawQuery != "" || strings.Contains(req.URL.String(), "test-secret") {
		tr.t.Errorf("request URL contains a credential: %s", req.URL.Redacted())
	}
	if req.Header.Get("x-goog-api-key") != "test-secret" {
		tr.t.Error("request is missing the API key header")
	}
	return nil, errors.New("transport unavailable")
}

func TestGeminiTransportErrorKeepsAPIKeyPrivate(t *testing.T) {
	client := GeminiClient{
		APIKey:     "test-secret",
		HTTPClient: &http.Client{Transport: errorTransport{t: t}},
	}
	_, err := client.Route(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), client.APIKey) {
		t.Fatalf("transport error exposes API key: %v", err)
	}
}

func TestRecordedFallbackMultipartAndLog(t *testing.T) {
	raw, err := os.ReadFile("testdata/fallback.json")
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore(nil)
	defer st.Close()
	ctx := context.Background()
	if err := st.Runs().Create(ctx, store.Run{ID: "run"}); err != nil {
		t.Fatal(err)
	}
	r := FallbackRouter{Client: fake{string(raw)}, Logger: StoreLogger{Store: st, RunID: "run"}}
	d, err := r.Route(ctx, "change the plan to ship chat first and remember to use Bazel")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Parts) != 2 || d.Parts[0].Destination != Planner || d.Parts[1].Destination != Memory {
		t.Fatalf("parts: %+v", d)
	}
	logs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs: %+v", logs)
	}
	var logged Decision
	if err := json.Unmarshal([]byte(logs[0].Output), &logged); err != nil {
		t.Fatal(err)
	}
	if logged.Parts[0].Confidence != .97 || logs[0].TraceID == "" {
		t.Fatal(logs[0])
	}
}
func TestRecordedGeminiAndSelection(t *testing.T) {
	raw, err := os.ReadFile("testdata/gemini.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/decisions" {
			t.Error(r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "dummy" {
			t.Error("missing header")
		}
		//nolint:govet // request fields follow the wire format
		var body struct {
			Model     string
			Questions map[string]struct {
				Type         string
				Criteria     map[string]string
				Instructions string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "models/gemini-decision-1" || len(body.Questions) != 4 || len(body.Questions["destination"].Criteria) != 6 || body.Questions["needs_clarification"].Type != "noul" {
			t.Errorf("request %+v", body)
		}
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	r := Select(nil, "concierge", "dummy", server.URL, nil)
	if _, ok := r.(GeminiClient); !ok {
		t.Fatalf("selected %T", r)
	}
	d, err := r.Route(context.Background(), "change the plan to ship chat first")
	if err != nil {
		t.Fatal(err)
	}
	if d.Destination != Planner || d.Confidence != .96 || d.NeedsClarification {
		t.Fatal(d)
	}
	if _, ok := Select(fake{}, "concierge", "", "", nil).(FallbackRouter); !ok {
		t.Fatal("fallback not selected")
	}
}
func TestRecordedFallbackClarificationAndValidation(t *testing.T) {
	for _, raw := range []string{`{"destination":"planner","confidence":1.7}`, `{"destination":"planner","confidence":0.99,"needs_clarification":true}`} {
		d, err := (FallbackRouter{Client: fake{raw}}).Route(context.Background(), "change it")
		if err != nil {
			t.Fatal(err)
		}
		if !d.NeedsClarification || d.ClarifyingQuestion == "" {
			t.Fatal(d)
		}
	}
}

func TestRecordedGeminiMultipartAndConfidences(t *testing.T) {
	fallback, err := os.ReadFile("testdata/fallback.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/gemini.json")
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	inputs := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Input string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		inputs <- request.Input
		if request.Input == "remember to use Bazel" {
			answers := wire["answers"].(map[string]any)
			answers["destination"].(map[string]any)["choice"] = "memory"
			answers["is_standing_rule"].(map[string]any)["noul"] = .99
		}
		if err := json.NewEncoder(w).Encode(wire); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(context.Background(), store.Run{ID: "run"}); err != nil {
		t.Fatal(err)
	}
	r := Select(fake{string(fallback)}, "concierge", "dummy", server.URL, StoreLogger{Store: st, RunID: "run", Provider: "openai", Model: "concierge"})
	d, err := r.Route(context.Background(), "change the plan to ship chat first and remember to use Bazel")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Parts) != 2 || d.Parts[0].Destination != Planner || d.Parts[1].Destination != Memory || !d.Parts[1].IsStandingRule || d.Parts[1].Confidences["is_standing_rule"] != .99 {
		t.Fatalf("decision: %+v", d)
	}
	logs, err := st.Decisions().List(context.Background(), store.DecisionFilter{RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[0].Tool != "routing.segment" || logs[0].Provider != "openai" || logs[1].Provider != "gemini" || logs[1].Tool != "routing" {
		t.Fatalf("routing logs: %+v", logs)
	}
	if got := <-inputs; got != "change the plan to ship chat first" {
		t.Fatal(got)
	}
	if got := <-inputs; got != "remember to use Bazel" {
		t.Fatal(got)
	}
}

func TestGeminiPreservesSelectedOpenQuestion(t *testing.T) {
	raw, err := os.ReadFile("testdata/gemini.json")
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"choice":"planner"`, `"choice":"answer-open-question"`, 1))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) }))
	defer server.Close()
	r := Select(fake{`{"destination":"answer-open-question","confidence":0.99,"question_id":"ask-2"}`}, "concierge", "dummy", server.URL, nil)
	d, err := r.Route(context.Background(), "yes to ask-2")
	if err != nil {
		t.Fatal(err)
	}
	if d.Destination != AnswerOpenQuestion || d.QuestionID != "ask-2" {
		t.Fatal(d)
	}
}
