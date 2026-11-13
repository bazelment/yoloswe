package router

import (
	"context"
	"encoding/json"
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
