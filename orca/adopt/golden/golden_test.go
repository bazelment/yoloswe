package golden

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/bazelment/yoloswe/orca/adopt"
)

var update = flag.Bool("update", false, "rewrite adoption golden files")

type goldenLane struct {
	ID          string `json:"id"`
	Move        string `json:"move"`
	ReasonClass string `json:"reason_class"`
}

// TestCorpus loads every synthetic run directory under testdata. New
// sanitized real runs can be dropped beside these fixtures without changing
// the test harness; only their golden.json needs to be reviewed.
func TestCorpus(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			dir := filepath.Join("testdata", entry.Name())
			gotRun, err := (&adopt.Service{}).DryRunDir(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]goldenLane, 0, len(gotRun.Lanes))
			for _, lane := range gotRun.Lanes {
				got = append(got, goldenLane{ID: lane.ID, Move: lane.Move, ReasonClass: lane.ReasonClass})
			}
			sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
			path := filepath.Join(dir, "golden.json")
			if *update {
				data, marshalErr := json.MarshalIndent(got, "", "  ")
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if writeErr := os.WriteFile(path, append(data, '\n'), 0o644); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			var want []goldenLane
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &want); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("golden mismatch\n got: %v\nwant: %v\nrun=%+v", got, want, gotRun)
			}
		})
	}
}
