package adopt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeDirDropsSecretsAndPreservesPreview(t *testing.T) {
	input := t.TempDir()
	output := filepath.Join(t.TempDir(), "corpus")
	if err := os.MkdirAll(filepath.Join(input, "reports", "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"goal":"owner secret ghp_FAKE_TOKEN","target":"main","merge_gate":"verdict+ci","phases":[{"name":"swe"}]},"tasks":[{"id":"measurement","branch":"swarm-lane/synthetic/measurement","phase":"swe","status":"done","round":1}]}`)
	writeSanitizeTest(t, filepath.Join(input, "OBJECTIVE.md"), "Owner context with ghp_FAKE_TOKEN and #77\n")
	writeSanitizeTest(t, filepath.Join(input, "measurement.swe.md"), "Outcome: done\nHEAD: abcdef1234567\nPR: #77\nBreaks: private details\n")
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tmeasurement\tcompleted\tPR #77 is still OPEN\n")
	writeSanitizeTest(t, filepath.Join(input, "measurement.hold-advance"), "measurement lane: never opens a PR\n")
	writeSanitizeTest(t, filepath.Join(input, "measurement.swe.spawn.json"), `{"session_id":"session-77","terminal_id":"terminal-77","note":"ghp_FAKE_TOKEN"}`)
	writeSanitizeTest(t, filepath.Join(input, ".creds"), "ghp_FAKE_TOKEN\n")
	writeSanitizeTest(t, filepath.Join(input, "fake.pem"), "-----BEGIN FAKE PRIVATE KEY-----\n")
	writeSanitizeTest(t, filepath.Join(input, "lanes.env"), "TOKEN=ghp_FAKE_TOKEN\n")
	writeSanitizeTest(t, filepath.Join(input, "reports", "secrets", "token.txt"), "sk-FAKE\n")
	writeSanitizeTest(t, filepath.Join(input, "random.tmp"), "drop me\n")
	writeSanitizeTest(t, filepath.Join(input, "measurement.swe.prev-1.md"), "drop history\n")

	want, err := (&Service{}).DryRunDir(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := SanitizeDir(input, output); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(output, "random.tmp"), filepath.Join(output, "measurement.swe.prev-1.md")} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("unknown file survived: %s", path)
		}
	}
	got, err := (&Service{}).DryRunDir(t.Context(), output)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Lanes) != len(got.Lanes) || want.Lanes[0].Move != got.Lanes[0].Move || want.Lanes[0].ReasonClass != got.Lanes[0].ReasonClass {
		t.Fatalf("preview changed: input=%+v output=%+v", want.Lanes, got.Lanes)
	}
	for _, path := range []string{filepath.Join(output, ".creds"), filepath.Join(output, "fake.pem"), filepath.Join(output, "lanes.env"), filepath.Join(output, "reports", "secrets")} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("secret path survived: %s", path)
		}
	}
	if err := filepath.Walk(output, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), "ghp_") || strings.Contains(string(data), "PRIVATE KEY") || strings.Contains(string(data), "sk-") {
			t.Fatalf("secret survived in %s: %q", path, data)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeRefusalPublishesNoOutput(t *testing.T) {
	input := t.TempDir()
	output := filepath.Join(t.TempDir(), "corpus")
	writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"phases":[{"name":"review"}]},"tasks":[{"id":"lane-ghp_BADTOKEN","phase":"review","status":"done"}]}`)
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tlane-ghp_BADTOKEN\tdecision\tcompleted\n")
	if err := SanitizeDir(input, output); err == nil || !strings.Contains(err.Error(), "EVENTS") || !strings.Contains(err.Error(), "github-token") || strings.Contains(err.Error(), "BADTOKEN") {
		t.Fatalf("refusal = %v, want relative file and pattern class without secret content", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("refused sanitize left output: %v", err)
	}
}

func TestPreviewMeasurementHoldAndConfigMergeGate(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	measurement := Lane{ID: "measurement", Phase: "swe", Status: "done", Branch: "lane/measurement", HoldAdvance: true, holdAdvanceText: "measurement lane: never opens a PR", Report: "Outcome: done\n", branchChecked: true, branchHead: "abcdef1234567"}
	if got := previewLaneAt(Run{}, measurement, now); got.Move != "none" || !strings.Contains(got.Reason, "measurement lane; host never merges") {
		t.Fatalf("measurement verdict = %+v, want none with host-never-merges reason", got)
	}
	gate := Lane{ID: "gate", Phase: "review", Status: "running", HoldAdvance: true, holdAdvanceText: "", Report: "Outcome: done\n"}
	got := previewLaneAt(Run{MergeGate: "verdict+ci"}, gate, now)
	if got.Move != "judge" || !strings.Contains(got.Reason, "merge gate: verdict+ci") {
		t.Fatalf("config merge gate verdict = %+v, want judge with config gate", got)
	}
}

func writeSanitizeTest(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
