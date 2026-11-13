package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

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

var _ = json.Valid

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
