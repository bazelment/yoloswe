package sessionanalysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyzeUsageStats_IncludeSubagentsByDefault(t *testing.T) {
	projectDir := t.TempDir()

	topPath := filepath.Join(projectDir, "session-top.jsonl")
	subDir := filepath.Join(projectDir, "session-top", "subagents")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	subPath := filepath.Join(subDir, "agent-a.jsonl")

	require.NoError(t, writeJSONLLines(topPath,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T01:00:00Z",
			"message": map[string]interface{}{
				"id":    "msg-top-1",
				"model": "claude-sonnet-4-6",
				"usage": map[string]interface{}{
					"input_tokens":                100,
					"output_tokens":               50,
					"cache_read_input_tokens":     10,
					"cache_creation_input_tokens": 5,
				},
			},
		}),
	))

	require.NoError(t, writeJSONLLines(subPath,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T01:05:00Z",
			"agentId":   "worker-a",
			"message": map[string]interface{}{
				"id":    "msg-sub-1",
				"model": "claude-opus-4-7",
				"usage": map[string]interface{}{
					"input_tokens":                200,
					"output_tokens":               80,
					"cache_read_input_tokens":     20,
					"cache_creation_input_tokens": 10,
				},
			},
		}),
	))

	report, err := AnalyzeUsageStats([]string{projectDir}, DefaultStatsConfig())
	require.NoError(t, err)

	assert.Equal(t, 2, report.FilesScanned, "top-level + subagent")
	assert.EqualValues(t, 300, report.Total.Usage.InputTokens)
	assert.EqualValues(t, 130, report.Total.Usage.OutputTokens)
	assert.EqualValues(t, 30, report.Total.Usage.CacheReadInputTokens)
	assert.EqualValues(t, 15, report.Total.Usage.CacheCreationInputTokens)
	assert.Equal(t, 2, report.Total.Sessions)
	assert.Len(t, report.ByModel, 2)
}

func TestAnalyzeUsageStats_TopLevelOnly(t *testing.T) {
	projectDir := t.TempDir()

	topPath := filepath.Join(projectDir, "session-top.jsonl")
	subDir := filepath.Join(projectDir, "session-top", "subagents")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	subPath := filepath.Join(subDir, "agent-a.jsonl")

	require.NoError(t, writeJSONLLines(topPath,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T01:00:00Z",
			"message": map[string]interface{}{
				"id":    "msg-top-1",
				"model": "claude-sonnet-4-6",
				"usage": map[string]interface{}{
					"input_tokens":                100,
					"output_tokens":               50,
					"cache_read_input_tokens":     0,
					"cache_creation_input_tokens": 0,
				},
			},
		}),
	))
	require.NoError(t, writeJSONLLines(subPath,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T01:05:00Z",
			"agentId":   "worker-a",
			"message": map[string]interface{}{
				"id":    "msg-sub-1",
				"model": "claude-opus-4-7",
				"usage": map[string]interface{}{
					"input_tokens":                200,
					"output_tokens":               80,
					"cache_read_input_tokens":     0,
					"cache_creation_input_tokens": 0,
				},
			},
		}),
	))

	cfg := DefaultStatsConfig()
	cfg.IncludeSubagents = false
	report, err := AnalyzeUsageStats([]string{projectDir}, cfg)
	require.NoError(t, err)

	assert.Equal(t, 1, report.FilesScanned)
	assert.EqualValues(t, 100, report.Total.Usage.InputTokens)
	assert.EqualValues(t, 50, report.Total.Usage.OutputTokens)
	assert.Equal(t, 1, report.Total.Sessions)
}

func TestAnalyzeUsageStats_DedupesAssistantUsageAndToolUse(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "session.jsonl")

	line := eventLine(map[string]interface{}{
		"type":      "assistant",
		"timestamp": "2026-04-23T01:00:00Z",
		"message": map[string]interface{}{
			"id":    "msg-1",
			"model": "claude-sonnet-4-6",
			"usage": map[string]interface{}{
				"input_tokens":                100,
				"output_tokens":               10,
				"cache_read_input_tokens":     5,
				"cache_creation_input_tokens": 0,
			},
			"content": []map[string]interface{}{
				{
					"type": "tool_use",
					"id":   "tool-1",
					"name": "Read",
				},
			},
		},
	})
	// Duplicate same message id/usage/tool_use should be counted once.
	require.NoError(t, writeJSONLLines(path, line, line))

	report, err := AnalyzeUsageStats([]string{projectDir}, DefaultStatsConfig())
	require.NoError(t, err)

	assert.EqualValues(t, 100, report.Total.Usage.InputTokens)
	assert.EqualValues(t, 10, report.Total.Usage.OutputTokens)
	assert.EqualValues(t, 5, report.Total.Usage.CacheReadInputTokens)
	assert.EqualValues(t, 1, report.Total.ToolUses)
	assert.EqualValues(t, 1, report.Total.UsageEvents)
}

func TestAnalyzeUsageStats_CountsToolUseFromLaterDuplicateMessage(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "session.jsonl")

	first := eventLine(map[string]interface{}{
		"type":      "assistant",
		"timestamp": "2026-04-23T01:00:00Z",
		"message": map[string]interface{}{
			"id":    "msg-1",
			"model": "claude-sonnet-4-6",
			"usage": map[string]interface{}{
				"input_tokens":                100,
				"output_tokens":               10,
				"cache_read_input_tokens":     0,
				"cache_creation_input_tokens": 0,
			},
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": "thinking...",
				},
			},
		},
	})

	second := eventLine(map[string]interface{}{
		"type":      "assistant",
		"timestamp": "2026-04-23T01:00:01Z",
		"message": map[string]interface{}{
			"id":    "msg-1",
			"model": "claude-sonnet-4-6",
			"usage": map[string]interface{}{
				"input_tokens":                100,
				"output_tokens":               10,
				"cache_read_input_tokens":     0,
				"cache_creation_input_tokens": 0,
			},
			"content": []map[string]interface{}{
				{
					"type": "tool_use",
					"id":   "tool-1",
					"name": "Read",
				},
			},
		},
	})

	require.NoError(t, writeJSONLLines(path, first, second))

	report, err := AnalyzeUsageStats([]string{projectDir}, DefaultStatsConfig())
	require.NoError(t, err)

	assert.EqualValues(t, 100, report.Total.Usage.InputTokens, "usage must be deduped by message id")
	assert.EqualValues(t, 1, report.Total.ToolUses, "tool_use in later duplicate message must still count")
}

func TestAnalyzeUsageStats_WindowFilter(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "session.jsonl")

	require.NoError(t, writeJSONLLines(path,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-22T23:59:00Z",
			"message": map[string]interface{}{
				"id":    "msg-old",
				"model": "claude-sonnet-4-6",
				"usage": map[string]interface{}{
					"input_tokens":                100,
					"output_tokens":               0,
					"cache_read_input_tokens":     0,
					"cache_creation_input_tokens": 0,
				},
			},
		}),
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T00:01:00Z",
			"message": map[string]interface{}{
				"id":    "msg-new",
				"model": "claude-sonnet-4-6",
				"usage": map[string]interface{}{
					"input_tokens":                200,
					"output_tokens":               0,
					"cache_read_input_tokens":     0,
					"cache_creation_input_tokens": 0,
				},
			},
		}),
	))

	cfg := DefaultStatsConfig()
	cfg.Since = mustTime(t, "2026-04-23T00:00:00Z")
	report, err := AnalyzeUsageStats([]string{projectDir}, cfg)
	require.NoError(t, err)

	assert.EqualValues(t, 200, report.Total.Usage.InputTokens)
}

func TestAnalyzeUsageStats_EstimatedCostWithCustomPricing(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "session.jsonl")

	require.NoError(t, writeJSONLLines(path,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T01:00:00Z",
			"message": map[string]interface{}{
				"id":    "msg-1",
				"model": "my-model",
				"usage": map[string]interface{}{
					"input_tokens":                1000,
					"output_tokens":               2000,
					"cache_read_input_tokens":     3000,
					"cache_creation_input_tokens": 4000,
				},
			},
		}),
	))

	cfg := DefaultStatsConfig()
	cfg.Pricing = PricingTable{
		Version: "test",
		Source:  "test",
		Models: map[string]ModelPricing{
			"my-model": {
				InputPerMTok:         1,
				OutputPerMTok:        2,
				CacheReadPerMTok:     3,
				CacheCreationPerMTok: 4,
			},
		},
	}

	report, err := AnalyzeUsageStats([]string{projectDir}, cfg)
	require.NoError(t, err)

	// (1000*1 + 2000*2 + 3000*3 + 4000*4) / 1e6 = 0.03
	assert.InDelta(t, 0.03, report.Total.EstimatedCostUSD, 1e-9)
	assert.EqualValues(t, 10000, report.Total.PricedTokens)
	assert.EqualValues(t, 0, report.Total.UnpricedTokens)
}

func TestAnalyzeUsageStats_ByFamilyRollup(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "session.jsonl")

	require.NoError(t, writeJSONLLines(path,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T01:00:00Z",
			"message": map[string]interface{}{
				"id":    "msg-opus",
				"model": "claude-opus-4-7",
				"usage": map[string]interface{}{
					"input_tokens":                100,
					"output_tokens":               10,
					"cache_read_input_tokens":     0,
					"cache_creation_input_tokens": 0,
				},
			},
		}),
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T01:00:01Z",
			"message": map[string]interface{}{
				"id":    "msg-opus-v46",
				"model": "claude-opus-4-6",
				"usage": map[string]interface{}{
					"input_tokens":                200,
					"output_tokens":               20,
					"cache_read_input_tokens":     0,
					"cache_creation_input_tokens": 0,
				},
			},
		}),
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-23T01:00:02Z",
			"message": map[string]interface{}{
				"id":    "msg-sonnet",
				"model": "claude-sonnet-4-6",
				"usage": map[string]interface{}{
					"input_tokens":                300,
					"output_tokens":               30,
					"cache_read_input_tokens":     0,
					"cache_creation_input_tokens": 0,
				},
			},
		}),
	))

	report, err := AnalyzeUsageStats([]string{projectDir}, DefaultStatsConfig())
	require.NoError(t, err)

	byFamily := make(map[string]FamilyBucket)
	for i := range report.ByFamily {
		byFamily[report.ByFamily[i].Family] = report.ByFamily[i]
	}

	opus, ok := byFamily["opus"]
	require.True(t, ok)
	assert.EqualValues(t, 300, opus.Usage.InputTokens)
	assert.EqualValues(t, 30, opus.Usage.OutputTokens)

	sonnet, ok := byFamily["sonnet"]
	require.True(t, ok)
	assert.EqualValues(t, 300, sonnet.Usage.InputTokens)
	assert.EqualValues(t, 30, sonnet.Usage.OutputTokens)
}

func TestAnalyzeUsageStats_SessionModelAttributedWhenInitBeforeWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// system/init is before the --since window; result event is inside the window.
	// The result event has no modelUsage so it must fall back to sessionModel.
	// Without the fix, sessionModel stays empty and the result goes to "(unknown)".
	require.NoError(t, writeJSONLLines(path,
		eventLine(map[string]interface{}{
			"type":      "system",
			"subtype":   "init",
			"timestamp": "2026-03-31T00:00:00Z", // before Since
			"model":     "claude-sonnet-4-6",
		}),
		eventLine(map[string]interface{}{
			"type":           "result",
			"timestamp":      "2026-04-02T00:00:00Z", // inside Since
			"total_cost_usd": 0.01,
		}),
	))

	cfg := DefaultStatsConfig()
	cfg.Since = mustTime(t, "2026-04-01T00:00:00Z")
	report, err := AnalyzeUsageStats([]string{dir}, cfg)
	require.NoError(t, err)

	// The result cost must be attributed to the correct model, not "(unknown)".
	require.Len(t, report.ByModel, 1)
	assert.Equal(t, "claude-sonnet-4-6", report.ByModel[0].Model)
	assert.InDelta(t, 0.01, report.ByModel[0].ObservedCostUSD, 1e-9)
}

func TestAnalyzeUsageStats_MalformedTimestampCounted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Event with non-empty but non-RFC3339Nano timestamp should increment parseErrors
	// and still be included in the window (treated as timestamp-less).
	require.NoError(t, writeJSONLLines(path,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "not-a-timestamp",
			"message": map[string]interface{}{
				"id":    "msg-1",
				"model": "claude-sonnet-4-6",
				"usage": map[string]interface{}{"input_tokens": 100, "output_tokens": 10},
			},
		}),
	))

	cfg := DefaultStatsConfig()
	cfg.Since = mustTime(t, "2026-01-01T00:00:00Z")
	report, err := AnalyzeUsageStats([]string{dir}, cfg)
	require.NoError(t, err)

	assert.EqualValues(t, 1, report.ParseErrors, "malformed timestamp must increment ParseErrors")
	// Event is still included (treated as timestamp-less when window is active).
	assert.EqualValues(t, 100, report.Total.Usage.InputTokens)
}

func TestAnalyzeUsageStats_UntilFiltering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(t, writeJSONLLines(path,
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-01T00:00:00Z",
			"message": map[string]interface{}{
				"id":    "msg-before",
				"model": "claude-sonnet-4-6",
				"usage": map[string]interface{}{"input_tokens": 100, "output_tokens": 50},
			},
		}),
		eventLine(map[string]interface{}{
			"type":      "assistant",
			"timestamp": "2026-04-10T00:00:00Z",
			"message": map[string]interface{}{
				"id":    "msg-after",
				"model": "claude-sonnet-4-6",
				"usage": map[string]interface{}{"input_tokens": 200, "output_tokens": 80},
			},
		}),
	))

	cfg := DefaultStatsConfig()
	cfg.Until = mustTime(t, "2026-04-05T00:00:00Z")
	report, err := AnalyzeUsageStats([]string{dir}, cfg)
	require.NoError(t, err)

	// Only the event before the Until cutoff should be counted.
	assert.EqualValues(t, 100, report.Total.Usage.InputTokens)
	assert.EqualValues(t, 50, report.Total.Usage.OutputTokens)
}

func TestAnalyzeUsageStats_ResultEventObservedCost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(t, writeJSONLLines(path,
		// result with UUID — should be counted once even if duplicated
		eventLine(map[string]interface{}{
			"type":      "result",
			"timestamp": "2026-04-01T00:00:00Z",
			"uuid":      "result-uuid-1",
			"modelUsage": map[string]interface{}{
				"claude-sonnet-4-6": map[string]interface{}{"costUSD": 0.005},
			},
		}),
		// duplicate of the same UUID — should be deduplicated
		eventLine(map[string]interface{}{
			"type":      "result",
			"timestamp": "2026-04-01T00:01:00Z",
			"uuid":      "result-uuid-1",
			"modelUsage": map[string]interface{}{
				"claude-sonnet-4-6": map[string]interface{}{"costUSD": 0.005},
			},
		}),
		// result without UUID — should be counted once
		eventLine(map[string]interface{}{
			"type":           "result",
			"timestamp":      "2026-04-01T00:02:00Z",
			"total_cost_usd": 0.010,
		}),
		// second uuid-less result — should be deduplicated (only first counts)
		eventLine(map[string]interface{}{
			"type":           "result",
			"timestamp":      "2026-04-01T00:03:00Z",
			"total_cost_usd": 0.010,
		}),
	))

	report, err := AnalyzeUsageStats([]string{dir}, DefaultStatsConfig())
	require.NoError(t, err)

	// UUID-based result: 0.005 (deduplicated from 2 identical events)
	// UUID-less result: 0.010 (first of 2 identical events, second deduplicated)
	assert.InDelta(t, 0.015, report.Total.ObservedCostUSD, 1e-9)
}

func TestLoadPricingTable_NormalizesKeyCasing(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "pricing*.json")
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(f).Encode(map[string]interface{}{
		"version": "test",
		"models": map[string]interface{}{
			// Mixed-case keys — should be normalized to lowercase so lookupPricing finds them.
			"Claude-Sonnet-4-6": map[string]interface{}{
				"input_per_mtok":  3.0,
				"output_per_mtok": 15.0,
			},
		},
	}))
	require.NoError(t, f.Close())

	tbl, err := LoadPricingTable(f.Name())
	require.NoError(t, err)

	// The key must exist in lowercase for lookupPricing to match.
	_, ok := tbl.Models["claude-sonnet-4-6"]
	assert.True(t, ok, "expected lowercase key after normalization")
	// Original mixed-case key must not survive.
	_, ok = tbl.Models["Claude-Sonnet-4-6"]
	assert.False(t, ok, "mixed-case key should have been removed")
}

func writeJSONLLines(path string, lines ...string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func eventLine(v map[string]interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	out, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return out
}
