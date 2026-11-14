package main

import (
	"testing"

	chatui "github.com/bazelment/yoloswe/orca/chat"
)

// TestParseGHPR reads `gh pr view --json state,isDraft,statusCheckRollup`:
// check runs by status and conclusion, status contexts by state.
func TestParseGHPR(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, json, text string
		want             chatui.PRStatus
	}{
		{name: "merged", json: `{"number":5705,"state":"MERGED","isDraft":false,"mergeCommit":{"oid":"merge-oid-5705"},"statusCheckRollup":[
{"__typename":"CheckRun","name":"Lint","status":"COMPLETED","conclusion":"SUCCESS"},
{"__typename":"CheckRun","name":"Build and Test","status":"COMPLETED","conclusion":"SUCCESS"},
{"__typename":"StatusContext","context":"swarm-review","state":"SUCCESS"}]}`,
			want: chatui.PRStatus{Number: 5705, State: "MERGED", MergeSHA: "merge-oid-5705", Checks: "3 passed"}, text: "merged, checks: 3 passed"},
		{name: "open draft", json: `{"state":"OPEN","isDraft":true,"statusCheckRollup":[
{"__typename":"CheckRun","name":"Lint","status":"COMPLETED","conclusion":"FAILURE"},
{"__typename":"CheckRun","name":"Build and Test","status":"IN_PROGRESS","conclusion":""},
{"__typename":"StatusContext","context":"swarm-review","state":"PENDING"}]}`,
			want: chatui.PRStatus{State: "OPEN", Draft: true, Checks: "0 passed, 1 failed (Lint), 2 pending"}, text: "open draft, checks: 0 passed, 1 failed (Lint), 2 pending"},
		{name: "closed without checks", json: `{"state":"CLOSED","isDraft":false,"statusCheckRollup":[]}`,
			want: chatui.PRStatus{State: "CLOSED", Checks: "none reported"}, text: "closed, checks: none reported"},
	} {
		got, err := parseGHPR([]byte(tc.json))
		if err != nil || got != tc.want || got.String() != tc.text {
			t.Fatalf("%s: got %+v (%q), %v; want %+v (%q)", tc.name, got, got.String(), err, tc.want, tc.text)
		}
	}
}
