package adopt

import (
	"fmt"
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
	writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"goal":"owner secret ghp_FAKE_TOKEN_12345678901234567890","target":"main","merge_gate":"verdict+ci","phases":[{"name":"swe"}]},"tasks":[{"id":"measurement","branch":"swarm-lane/synthetic/measurement","phase":"swe","status":"done","round":1}]}`)
	writeSanitizeTest(t, filepath.Join(input, "OBJECTIVE.md"), "Owner context with ghp_FAKE_TOKEN_12345678901234567890 and #77\n")
	writeSanitizeTest(t, filepath.Join(input, "measurement.swe.md"), "Outcome: done\nHEAD: abcdef1234567\nPR: #77\nBreaks: private details\n")
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tmeasurement\tcompleted\tPR #77 is still OPEN\n")
	writeSanitizeTest(t, filepath.Join(input, "measurement.hold-advance"), "measurement lane: never opens a PR\n")
	writeSanitizeTest(t, filepath.Join(input, "measurement.swe.spawn.json"), `{"session_id":"session-77","terminal_id":"terminal-77","note":"ghp_FAKE_TOKEN_12345678901234567890"}`)
	writeSanitizeTest(t, filepath.Join(input, ".creds"), "ghp_FAKE_TOKEN_12345678901234567890\n")
	writeSanitizeTest(t, filepath.Join(input, "fake.pem"), "-----BEGIN FAKE PRIVATE KEY-----\n")
	writeSanitizeTest(t, filepath.Join(input, "lanes.env"), "TOKEN=ghp_FAKE_TOKEN_12345678901234567890\n")
	writeSanitizeTest(t, filepath.Join(input, "reports", "secrets", "token.txt"), "sk-FAKE_12345678901234567890\n")
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
	writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"phases":[{"name":"review"}]},"tasks":[{"id":"lane-ghp_BADTOKEN_12345678901234567890","phase":"review","status":"done"}]}`)
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tlane-ghp_BADTOKEN_12345678901234567890\tdecision\tcompleted\n")
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

func TestSanitizeVerifyPreservesEventVocabularyAndNestedReports(t *testing.T) {
	input := t.TempDir()
	output := filepath.Join(t.TempDir(), "sanitized")
	writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"phases":[{"name":"swe"}]},"tasks":[{"id":"lane","phase":"swe","status":"done","round":1,"terminal_state":"finished","merge_sha":"abcdef1234567"}]}`)
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tlane\thealth-state\tfinished sensitive prose\n")
	writeSanitizeTest(t, filepath.Join(input, "reports", "lane.swe.r1.md"), "Outcome: done\nVerdict: pass\nBreaks: private prose\n")
	if err := SanitizeDir(input, output); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(output, "EVENTS"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "\thealth-state\tfinished\n") {
		t.Fatalf("structured event vocabulary changed: %q", events)
	}
	if _, err := os.Stat(filepath.Join(output, "reports", "lane.swe.r1.md")); err != nil {
		t.Fatalf("nested report was dropped: %v", err)
	}
	if err := VerifySanitized(t.Context(), input, output); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeVerifyReportsPerLaneDifference(t *testing.T) {
	input := t.TempDir()
	output := filepath.Join(t.TempDir(), "sanitized")
	writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"phases":[{"name":"swe"}]},"tasks":[{"id":"lane","phase":"swe","status":"running","round":1,"session_id":"session","terminal_state":"working"}]}`)
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tlane\thealth-state\tworking\n")
	writeSanitizeTest(t, filepath.Join(input, "reports", "lane.swe.r1.md"), "Outcome: done\n")
	if err := SanitizeDir(input, output); err != nil {
		t.Fatal(err)
	}
	writeSanitizeTest(t, filepath.Join(output, "reports", "lane.swe.r1.md"), "Outcome: needs-input\n")
	data, err := os.ReadFile(filepath.Join(output, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), `"terminal_state": "working"`, `"terminal_state": "finished"`, 1)
	if err := os.WriteFile(filepath.Join(output, "state.json"), []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	err = VerifySanitized(t.Context(), input, output)
	if err == nil || !strings.Contains(err.Error(), "lane lane:") || !strings.Contains(err.Error(), "move") {
		t.Fatalf("verify error = %v, want per-lane move diff", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("verify refusal left output: %v", statErr)
	}
}

func TestSanitizeRefusesSecretInEventsDetail(t *testing.T) {
	input := t.TempDir()
	output := filepath.Join(t.TempDir(), "sanitized")
	writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"phases":[{"name":"swe"}]},"tasks":[{"id":"lane","phase":"swe","status":"done"}]}`)
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tlane\tdecision\tordinary prose ghp_FAKE_TOKEN_12345678901234567890\n")
	err := SanitizeDir(input, output)
	if err == nil || !strings.Contains(err.Error(), "EVENTS column detail") || !strings.Contains(err.Error(), "github-token") || strings.Contains(err.Error(), "FAKE_TOKEN") {
		t.Fatalf("sanitize error = %v, want redacted detail-column refusal", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("refused sanitize left output: %v", statErr)
	}
}

func TestSanitizeSecretPatternsRejectRealisticTokensOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		class string
	}{
		{name: "sk", value: "sk-12345678901234567890", class: "api-token"},
		{name: "sk proj", value: "sk-proj-12345678901234567890", class: "api-token"},
		{name: "sk ant", value: "sk-ant-12345678901234567890", class: "api-token"},
		{name: "google", value: "AIza12345678901234567890123456789012345", class: "google-token"},
		{name: "slack", value: "xoxb-1234567890", class: "slack-token"},
		{name: "github ghp", value: "ghp_12345678901234567890", class: "github-token"},
		{name: "github gho", value: "gho_12345678901234567890", class: "github-token"},
		{name: "github pat", value: "github_pat_12345678901234567890", class: "github-token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !sanitizeSecretRE.MatchString(tc.value) {
				t.Fatalf("sanitizeSecretRE did not match realistic %s token %q", tc.class, tc.value)
			}
			if got := secretPatternClass([]byte(tc.value)); got != tc.class {
				t.Fatalf("secretPatternClass(%q) = %q, want %q", tc.value, got, tc.class)
			}
		})
	}
	for _, benign := range []string{"task-x", "task-1", "disk-full", "warden-risk-xyz", "2026-11-18T01:02:03Z"} {
		if sanitizeSecretRE.MatchString(benign) {
			t.Errorf("benign value %q matched secret pattern", benign)
		}
	}
}

func TestSanitizePreservesIntegrationEvidenceVocabulary(t *testing.T) {
	input := t.TempDir()
	output := filepath.Join(t.TempDir(), "sanitized")
	writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"phases":[{"name":"review"}]},"tasks":[{"id":"lane","phase":"review","status":"done","round":1}]}`)
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tlane\torch-note\tmerged #42 as abc1234def56789\n")
	allow, err := sanitizeAllowlist(input)
	if err != nil {
		t.Fatal(err)
	}
	vocab, err := sanitizeVocabularyFor(input, allow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vocab.tokens["as"]; !ok {
		t.Fatal("integration vocabulary dropped the merge SHA connector")
	}
	if err := SanitizeDir(input, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, "EVENTS"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "merged #42 as abc1234def56789") {
		t.Fatalf("integration evidence changed: %q", got)
	}
	if got := laneMergeNotePRs(filepath.Join(output, "EVENTS"))["lane"]; len(got) != 1 || got[0] != "42" {
		t.Fatalf("sanitized integration evidence PRs = %#v, want [42]", got)
	}
	if err := VerifySanitized(t.Context(), input, output); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeRejectsEscapingPaths(t *testing.T) {
	for _, tc := range []struct{ name, id, phase string }{
		{"lane", "../outside", "review"},
		{"phase", "lane", "../../../outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			input := filepath.Join(root, "input")
			output := filepath.Join(root, "output")
			writeSanitizeTest(t, filepath.Join(root, "outside.hold-advance"), "external owner prose")
			writeSanitizeTest(t, filepath.Join(input, "state.json"), fmt.Sprintf(`{"config":{"phases":[{"name":%q}]},"tasks":[{"id":%q,"phase":%q,"status":"done"}]}`, tc.phase, tc.id, tc.phase))
			if err := SanitizeDir(input, output); err == nil {
				t.Fatal("sanitize accepted an escaping ledger path")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("refusal left output: %v", err)
			}
		})
	}
}

func TestSanitizeRejectsSymlinkedKeptFiles(t *testing.T) {
	for _, rel := range []string{"OBJECTIVE.md", "lane.hold-advance", "reports/lane.review.r1.md"} {
		t.Run(rel, func(t *testing.T) {
			input := t.TempDir()
			output := filepath.Join(t.TempDir(), "output")
			outside := filepath.Join(t.TempDir(), "external.md")
			writeSanitizeTest(t, outside, "owner data")
			writeSanitizeTest(t, filepath.Join(input, "state.json"), `{"config":{"orchestrator_session_id":"fixture","phases":[{"name":"review"}]},"tasks":[{"id":"lane","phase":"review","status":"done"}]}`)
			path := filepath.Join(input, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			if err := SanitizeDir(input, output); err == nil {
				t.Fatal("sanitize followed a kept symlink")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("refusal left output: %v", err)
			}
		})
	}
}

func TestVerifySanitizedNeverRemovesInput(t *testing.T) {
	for _, mode := range []string{"relative alias", "ancestor", "symlink ancestor"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			input := filepath.Join(root, "input")
			marker := filepath.Join(input, "keep.txt")
			writeSanitizeTest(t, marker, "owner input must survive")
			// Missing state.json makes verification fail before any lane comparison.
			output := root
			if mode == "relative alias" {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				output, err = filepath.Rel(cwd, input)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink ancestor" {
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				output = alias
				input = filepath.Join(alias, "input")
			}
			if err := VerifySanitized(t.Context(), input, output); err == nil {
				t.Fatal("verify unexpectedly succeeded")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "owner input must survive" {
				t.Fatalf("verify removed its input: %v", err)
			}
		})
	}
}

func TestSanitizeVerifyRefusesLostMergeEvidence(t *testing.T) {
	input, _, _ := integrationFixture(t, "squash", "main", 0, false)
	writeSanitizeTest(t, filepath.Join(input, "EVENTS"), "2026-11-17T00:00:00Z\tlane-1\torch-note\tmerged #42 as abc1234def56789\n")
	before, err := (&Service{}).DryRunDir(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got := before.Lanes[0]; got.Move != "none" || got.ReasonClass != "integrated" {
		t.Fatalf("original note-only evidence = %+v", got)
	}
	output := filepath.Join(t.TempDir(), "sanitized")
	if err := SanitizeDir(input, output); err != nil {
		t.Fatal(err)
	}
	err = VerifySanitized(t.Context(), input, output)
	if err == nil || !strings.Contains(err.Error(), "lane lane-1: move none -> judge; reason_class integrated -> judge") {
		t.Fatalf("expected lost mirror evidence to refuse, got %v", err)
	}
	t.Logf("Observed note-only integration drift: %v", err)
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("verify left drifting output: %v", err)
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
