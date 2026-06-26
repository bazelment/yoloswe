package cliapp

import (
	"bytes"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func withStubbedBuildInfo(t *testing.T, info *debug.BuildInfo, ok bool) {
	t.Helper()
	prev := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, ok }
	t.Cleanup(func() { readBuildInfo = prev })
}

// stubExecutable makes executableModTime fall back to a missing path, so tests
// that exercise the "no vcs.time" path get a zero Time rather than the real
// test-binary mtime.
func stubMissingExecutable(t *testing.T) {
	t.Helper()
	prev := executablePath
	executablePath = func() (string, error) { return "/nonexistent/jiradozer-test-binary", nil }
	t.Cleanup(func() { executablePath = prev })
}

func buildInfoWith(settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{Settings: settings}
}

// withStampedBuild pins the linker-stamped revision/time package vars for a
// test and restores them afterward.
func withStampedBuild(t *testing.T, rev, ts string) {
	t.Helper()
	prevRev, prevTime := stampedRevision, stampedTime
	stampedRevision, stampedTime = rev, ts
	t.Cleanup(func() { stampedRevision, stampedTime = prevRev, prevTime })
}

func TestReadBuildInfo(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		withStubbedBuildInfo(t, buildInfoWith(
			debug.BuildSetting{Key: "vcs.revision", Value: "abcdef0123456789"},
			debug.BuildSetting{Key: "vcs.time", Value: "2026-05-31T20:06:00Z"},
			debug.BuildSetting{Key: "vcs.modified", Value: "true"},
		), true)

		got := ReadBuildInfo()
		if got.Revision != "abcdef0123456789" {
			t.Errorf("Revision = %q", got.Revision)
		}
		if !got.Modified {
			t.Errorf("Modified = false, want true")
		}
		want, _ := time.Parse(time.RFC3339, "2026-05-31T20:06:00Z")
		if !got.Time.Equal(want) {
			t.Errorf("Time = %v, want %v", got.Time, want)
		}
		if got.ShortRevision() != "abcdef012345-dirty" {
			t.Errorf("ShortRevision = %q", got.ShortRevision())
		}
	})

	t.Run("no build info falls back to unknown", func(t *testing.T) {
		withStubbedBuildInfo(t, nil, false)
		stubMissingExecutable(t)
		got := ReadBuildInfo()
		if got.Revision != "unknown" {
			t.Errorf("Revision = %q, want unknown", got.Revision)
		}
		if !got.Time.IsZero() {
			t.Errorf("Time = %v, want zero", got.Time)
		}
		if got.ShortRevision() != "unknown" {
			t.Errorf("ShortRevision = %q, want unknown", got.ShortRevision())
		}
	})

	t.Run("missing vcs.time falls back to executable mtime", func(t *testing.T) {
		withStubbedBuildInfo(t, buildInfoWith(
			debug.BuildSetting{Key: "vcs.revision", Value: "abcdef0123456789"},
		), true)
		// Point at a real file with a known mtime.
		f, err := os.CreateTemp(t.TempDir(), "bin")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		want := time.Date(2026, 5, 31, 20, 6, 0, 0, time.UTC)
		if err := os.Chtimes(f.Name(), want, want); err != nil {
			t.Fatal(err)
		}
		prev := executablePath
		executablePath = func() (string, error) { return f.Name(), nil }
		t.Cleanup(func() { executablePath = prev })

		got := ReadBuildInfo()
		if !got.Time.Equal(want) {
			t.Errorf("Time = %v, want executable mtime %v", got.Time, want)
		}
	})

	t.Run("bazel stamp overrides revision and time", func(t *testing.T) {
		// No debug build info (sandboxed Bazel build), but the linker stamped
		// the revision and time via x_defs.
		withStubbedBuildInfo(t, nil, false)
		stubMissingExecutable(t)
		withStampedBuild(t, "deadbeefcafe0000", "2026-06-20T08:00:00Z")

		got := ReadBuildInfo()
		if got.Revision != "deadbeefcafe0000" {
			t.Errorf("Revision = %q, want stamped revision", got.Revision)
		}
		want, _ := time.Parse(time.RFC3339, "2026-06-20T08:00:00Z")
		if !got.Time.Equal(want) {
			t.Errorf("Time = %v, want stamped time %v", got.Time, want)
		}
		if got.ShortRevision() != "deadbeefcafe" {
			t.Errorf("ShortRevision = %q", got.ShortRevision())
		}
	})

	t.Run("stamp takes precedence over vcs settings", func(t *testing.T) {
		withStubbedBuildInfo(t, buildInfoWith(
			debug.BuildSetting{Key: "vcs.revision", Value: "0000000000000000"},
			debug.BuildSetting{Key: "vcs.time", Value: "2020-01-01T00:00:00Z"},
		), true)
		withStampedBuild(t, "feedface12345678", "2026-06-21T09:30:00Z")

		got := ReadBuildInfo()
		if got.Revision != "feedface12345678" {
			t.Errorf("Revision = %q, want stamped revision", got.Revision)
		}
		want, _ := time.Parse(time.RFC3339, "2026-06-21T09:30:00Z")
		if !got.Time.Equal(want) {
			t.Errorf("Time = %v, want stamped time", got.Time)
		}
	})

	t.Run("empty and placeholder stamps are ignored", func(t *testing.T) {
		withStubbedBuildInfo(t, buildInfoWith(
			debug.BuildSetting{Key: "vcs.revision", Value: "abcdef0123456789"},
		), true)
		stubMissingExecutable(t)
		// Unstamped build: empty, the git-less "unknown" sentinel, or an
		// unsubstituted placeholder must all fall back to vcs.revision.
		for _, sentinel := range []string{"", "unknown", "{STABLE_GIT_REVISION}"} {
			withStampedBuild(t, sentinel, "")
			if got := ReadBuildInfo().Revision; got != "abcdef0123456789" {
				t.Errorf("stamp %q: Revision = %q, want vcs.revision fallback", sentinel, got)
			}
		}
	})

	t.Run("clean build has no dirty suffix", func(t *testing.T) {
		withStubbedBuildInfo(t, buildInfoWith(
			debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef"},
			debug.BuildSetting{Key: "vcs.modified", Value: "false"},
		), true)
		if got := ReadBuildInfo().ShortRevision(); got != "0123456789ab" {
			t.Errorf("ShortRevision = %q", got)
		}
	})
}

func TestWarnIfStaleBuild(t *testing.T) {
	fixedNow := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	prevNow := now
	now = func() time.Time { return fixedNow }
	t.Cleanup(func() { now = prevNow })

	cases := []struct {
		name     string
		build    BuildInfo
		wantWarn bool
	}{
		{
			name:     "fresh build is quiet",
			build:    BuildInfo{Revision: "abc", Time: fixedNow.Add(-24 * time.Hour)},
			wantWarn: false,
		},
		{
			name:     "stale build warns",
			build:    BuildInfo{Revision: "abc", Time: fixedNow.Add(-10 * 24 * time.Hour)},
			wantWarn: true,
		},
		{
			name:     "missing build time is quiet",
			build:    BuildInfo{Revision: "unknown"},
			wantWarn: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			warnIfStaleBuild(logger, "jiradozer", tc.build)
			gotWarn := strings.Contains(buf.String(), "stale build")
			if gotWarn != tc.wantWarn {
				t.Errorf("warn emitted = %v, want %v (log: %q)", gotWarn, tc.wantWarn, buf.String())
			}
		})
	}
}
