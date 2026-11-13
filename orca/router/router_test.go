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

	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestLowMarginUsesConcierge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"handler":{"type":"choice","choice":"planner","confidence":0.5,"probabilities":{"planner":0.5,"dispatcher":0.4,"concierge":0.1}}}}`))
	}))
	defer server.Close()
	d, err := (GeminiRouter{BaseURL: server.URL, APIKey: "dummy"}).Route(t.Context(), "change it")
	if err != nil || d.Handler != Concierge || d.Confidence != .5 || d.Probabilities["planner"] != .5 {
		t.Fatalf("low margin: %+v %v", d, err)
	}
}

type fake struct {
	err  error
	text string
}

func (f fake) CreateMessage(context.Context, model.Request) (*model.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &model.Response{Message: model.NewAssistantMessage(model.NewTextBlock(f.text))}, nil
}
func TestLLMThreeWayParse(t *testing.T) {
	for _, h := range []Handler{Concierge, Planner, Dispatcher} {
		d, err := (LLMRouter{Client: fake{text: `{"handler":"` + string(h) + `","confidence":0.9}`}}).Route(t.Context(), "message")
		if err != nil || d.Handler != h || d.Backend != "llm" {
			t.Fatalf("%s: %+v %v", h, d, err)
		}
	}
	d, err := (LLMRouter{Client: fake{text: `{"handler":"planner","confidence":0.4}`}}).Route(t.Context(), "message")
	if err != nil || d.Handler != Concierge {
		t.Fatalf("low confidence: %+v %v", d, err)
	}
	for _, raw := range []string{`{"handler":"memory","confidence":1}`, `{"handler":"planner","confidence":1.5}`, `invalid`} {
		if _, err := (LLMRouter{Client: fake{text: raw}}).Route(t.Context(), "message"); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestGeminiSingleChoiceAndOpenQuestion(t *testing.T) {
	raw, err := os.ReadFile("testdata/gemini.json")
	if err != nil {
		t.Fatal(err)
	}
	input := FormatInput("go with B", "ask-1: A or B?")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]struct {
				Criteria     map[string]string
				Type         string
				Instructions string
			}
			Input string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1beta/decisions" || r.Header.Get("x-goog-api-key") != "dummy" || body.Input != input || len(body.Questions) != 1 || len(body.Questions["handler"].Criteria) != 3 || body.Questions["handler"].Instructions != routingRubric {
			t.Errorf("unexpected request: %+v", body)
		}
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	d, err := Select(fake{err: errors.New("LLM must not run")}, "model", "dummy", server.URL, nil).Route(t.Context(), input)
	if err != nil || d.Handler != Dispatcher || d.Fallback || d.Backend != "gemini" {
		t.Fatalf("%+v %v", d, err)
	}
}
func TestFallbackAndAudit(t *testing.T) {
	//nolint:govet // Table fields follow the failure scenario.
	for _, tc := range []struct {
		name, body string
		status     int
		llm        fake
		want       Handler
		backend    string
	}{
		{"500", "", 500, fake{text: `{"handler":"planner","confidence":0.9}`}, Planner, "llm"},
		{"invalid choice", `{"answers":{"handler":{"type":"choice","choice":"memory","confidence":0.9,"probabilities":{"concierge":0.9,"planner":0.05,"dispatcher":0.05}}}}`, 200, fake{text: `{"handler":"dispatcher","confidence":0.9}`}, Dispatcher, "llm"},
		{"invalid probabilities", `{"answers":{"handler":{"type":"choice","choice":"planner","confidence":0.9,"probabilities":{"planner":0.9}}}}`, 200, fake{text: `{"handler":"planner","confidence":0.9}`}, Planner, "llm"},
		{"both down", "", 500, fake{err: errors.New("down")}, Concierge, "concierge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			st := store.NewMemoryStore(nil)
			defer st.Close()
			if err := st.Runs().Create(t.Context(), store.Run{ID: "run"}); err != nil {
				t.Fatal(err)
			}
			d, err := Select(tc.llm, "model", "dummy", server.URL, StoreLogger{Store: st, RunID: "run"}).Route(t.Context(), "message")
			if err != nil || d.Handler != tc.want || !d.Fallback || d.Backend != tc.backend {
				t.Fatalf("%+v %v", d, err)
			}
			logs, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: "run"})
			if err != nil || len(logs) != 1 {
				t.Fatalf("%+v %v", logs, err)
			}
			var logged Decision
			if err := json.Unmarshal([]byte(logs[0].Output), &logged); err != nil {
				t.Fatal(err)
			}
			if !logged.Fallback || logged.Handler != tc.want || logged.Backend != tc.backend || logs[0].Input != "message" {
				t.Fatalf("%+v", logs[0])
			}
		})
	}
}
func TestNoKeyUsesLLM(t *testing.T) {
	d, err := Select(fake{text: `{"handler":"dispatcher","confidence":0.9}`}, "model", "", "http://unreachable.invalid", nil).Route(t.Context(), "message")
	if err != nil || d.Handler != Dispatcher || d.Fallback {
		t.Fatalf("%+v %v", d, err)
	}
}

type errorTransport struct{}

func (errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("private transport detail")
}
func TestTransportErrorIsSanitized(t *testing.T) {
	_, err := (GeminiRouter{APIKey: "test-secret", HTTPClient: &http.Client{Transport: errorTransport{}}}).Route(t.Context(), "message")
	if err == nil || strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "private") {
		t.Fatalf("%v", err)
	}
}
