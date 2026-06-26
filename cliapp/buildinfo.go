package cliapp

import (
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

// staleBuildThreshold is how old a binary's commit can be before startup emits
// a loud WARN. Cron-driven tools are rebuilt nightly, so a build older than
// this almost always means a deploy/symlink went stale. now is a package var
// so tests can pin the clock.
const staleBuildThreshold = 7 * 24 * time.Hour

var now = time.Now

// BuildInfo describes the VCS provenance of the running binary, read from the
// Go build info embedded by the toolchain (debug.ReadBuildInfo). It lets a
// long-lived cron-driven tool self-identify which commit produced the binary
// that actually ran — the single most useful diagnostic when a stale symlink
// silently keeps an old build in service.
type BuildInfo struct {
	// Time is the commit timestamp ("vcs.time"), falling back to the running
	// executable's mtime, zero when neither is available.
	Time time.Time
	// Revision is the VCS commit the binary was built from ("vcs.revision"),
	// or "unknown" when build info is unavailable (e.g. `go run`, some test
	// binaries).
	Revision string
	// Modified reports whether the working tree had uncommitted changes at
	// build time ("vcs.modified").
	Modified bool
}

// readBuildInfo and executablePath are package vars so tests can stub them.
var (
	readBuildInfo  = debug.ReadBuildInfo
	executablePath = os.Executable
)

// stampedRevision and stampedTime are set at link time via Bazel x_defs (see
// jiradozer/cmd/jiradozer/BUILD.bazel + tools/workspace_status.sh) when a build
// runs with --config=stamp. They are the most reliable provenance source for
// Bazel binaries, which build sandboxed away from .git and so carry no
// debug.ReadBuildInfo vcs.* settings. Empty when unstamped (normal dev builds,
// `go run`, non-Bazel builds), in which case ReadBuildInfo falls back to the
// debug build info and then the executable mtime. stampedTime is RFC3339.
var (
	stampedRevision string
	stampedTime     string
)

// ReadBuildInfo extracts the VCS settings from the embedded build info. It
// never fails: when build info or a VCS setting is missing, the corresponding
// field stays at its zero value (Revision defaults to "unknown").
//
// Time falls back to the running executable's modification time when the
// toolchain did not embed vcs.time. Bazel builds in particular run sandboxed
// away from .git, so they carry no VCS stamp — but the binary's mtime is still
// a reliable "when was this built" signal, which is what the staleness guard
// needs.
func ReadBuildInfo() BuildInfo {
	bi := BuildInfo{Revision: "unknown"}
	if info, ok := readBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if s.Value != "" {
					bi.Revision = s.Value
				}
			case "vcs.time":
				if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
					bi.Time = t
				}
			case "vcs.modified":
				bi.Modified = s.Value == "true"
			}
		}
	}
	// Prefer the Bazel stamp when present — it is the only reliable source for
	// Bazel builds, which sandbox away from .git. An unstamped build leaves
	// these empty (or as the literal placeholder when stamping is off but the
	// key was substituted to empty), so guard against both.
	if rev := stampValue(stampedRevision); rev != "" {
		bi.Revision = rev
	}
	if ts := stampValue(stampedTime); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			bi.Time = t
		}
	}
	if bi.Time.IsZero() {
		bi.Time = executableModTime()
	}
	return bi
}

// stampValue normalizes a linker-stamped x_def value: it returns "" for an
// unset stamp, the literal "unknown" sentinel emitted by workspace_status.sh
// when not in a git tree, or an unsubstituted "{STABLE_...}" placeholder (which
// rules_go can leave verbatim on an unstamped build). A real value passes
// through unchanged.
func stampValue(v string) string {
	if v == "" || v == "unknown" {
		return ""
	}
	if strings.HasPrefix(v, "{") && strings.HasSuffix(v, "}") {
		return ""
	}
	return v
}

// executableModTime returns the running binary's modification time, or the zero
// time when it can't be determined.
func executableModTime() time.Time {
	path, err := executablePath()
	if err != nil {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// ShortRevision returns the first 12 characters of the revision (or the whole
// thing if shorter), with a "-dirty" suffix when the build had local changes.
// Suitable for compact log lines and failure notifications.
func (b BuildInfo) ShortRevision() string {
	rev := b.Revision
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if b.Modified {
		rev += "-dirty"
	}
	return rev
}

// buildTimeString renders a build time for structured logs, or "unknown" when
// the timestamp is unavailable.
func buildTimeString(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// warnIfStaleBuild emits a loud WARN when the running binary's commit is older
// than staleBuildThreshold. A no-op when the build time is unavailable (we
// can't judge staleness without it). This is cheap insurance against a cron
// symlink silently pointing at an old build, as happened in production.
func warnIfStaleBuild(logger *slog.Logger, toolName string, b BuildInfo) {
	if b.Time.IsZero() {
		return
	}
	age := now().Sub(b.Time)
	if age <= staleBuildThreshold {
		return
	}
	logger.Warn("running a stale build — rebuild/redeploy may be overdue",
		"tool", toolName,
		"build_revision", b.ShortRevision(),
		"build_time", buildTimeString(b.Time),
		"age_days", int(age.Hours()/24),
		"threshold_days", int(staleBuildThreshold.Hours()/24),
	)
}
