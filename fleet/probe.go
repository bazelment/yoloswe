package fleet

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tool describes the binary being dispatched. Everything the probe and the
// dispatcher need to know that differs between prdozer and jiradozer lives
// here, so neither has to restate the mechanism.
type Tool struct {
	// Name is the binary name as it appears on PATH ("prdozer", "jiradozer").
	Name string
	// LeaseDir is where that tool keeps its per-task flock files, e.g.
	// "~/.prdozer/leases". The probe counts HELD locks there.
	LeaseDir string
	// MaxLeasesPerHost caps concurrent runs on one box. 0 means the default.
	MaxLeasesPerHost int
}

// probeScript gathers every signal in ONE ssh round trip. Each command is
// sub-100ms and needs no sudo. Sections are delimited so the parser never has
// to guess which output it is looking at — `df` in particular omits or
// collapses rows depending on the filesystem, so position-based parsing is
// unreliable.
//
// The lease section emits both a COUNT and the held lock NAMES. The names are
// what let a caller answer "which host is running INF-1234", which a count
// cannot; the dispatcher needs the count and the reaper needs the names.
func (t Tool) probeScript() string {
	leaseDir := t.LeaseDir
	if leaseDir == "" {
		leaseDir = "~/." + t.Name + "/leases"
	}
	// GitHub auth is probed because EVERY task needs it: the worker runs
	// `wt new`, which refuses without GitHub auth, so a box with an expired
	// token fails a second after dispatch, every time, having done nothing.
	// Observed 2026-08-05: the token expired fleet-wide, dispatch still scored
	// all three boxes "eligible", and the run died at worktree creation. The
	// probe was checking that the tool EXISTS, not that it can work — the same
	// blind spot that let a ~/.local/bin install read as "not on PATH".
	//
	// It goes through `gh api user`, NOT `gh auth status`. Verified 2026-08-06
	// against a deliberately corrupted token: `gh auth status` PRINTS "The
	// token is invalid" and still EXITS 0, so `gh auth status && echo ok`
	// reports a healthy box no matter what. The first version of this fix did
	// exactly that and passed its unit tests, because those feed the parser
	// synthetic values and never run the command.
	//
	// `gh api user` exercises the capability the worker actually needs —
	// authorization against the API, which is what `wt new` fails on — and its
	// exit code means something. It costs no extra round trip: `gh auth status`
	// was already calling the network and discarding the answer.
	//
	// Held locks are tested with `flock -n`, never counted as files. Release
	// deliberately leaves the file behind — removing it races a process that
	// just opened it and is about to flock — so a file count is a count of
	// tasks the box has EVER run. Observed 2026-07-30: with a cap of 2, two of
	// three boxes reported "already holds 2 leases" with zero live workers,
	// leaving only the most overloaded host eligible.
	return `echo "__NPROC__"; nproc
echo "__LOAD__"; cat /proc/loadavg
echo "__DF__"; df -P "$HOME"; df -P /mnt/nvme 2>/dev/null
echo "__TMUX__"; tmux list-windows -a 2>/dev/null | wc -l
echo "__LEASES__"; { cd ` + leaseDir + ` 2>/dev/null && for f in *.lock; do [ -e "$f" ] || continue; flock -n "$f" true 2>/dev/null || echo "$f"; done; } 2>/dev/null || true
echo "__BIN__"; ` + t.resolveBinExpr() + `
echo "__GH__"; gh api user >/dev/null 2>&1 && echo ok || echo ` + ghAuthMissing + `
echo "__END__"`
}

// binMissing is what resolveBinExpr prints when the tool is nowhere to be
// found. It is a sentinel rather than an empty line so a truncated section and
// an absent binary stay distinguishable.
const binMissing = "MISSING"

// ghAuthMissing is what the probe prints when `gh api user` fails. Like
// binMissing it is a sentinel, so "the section was truncated" and "auth is
// broken" never collapse into the same reading.
const ghAuthMissing = "NOAUTH"

// resolveBinExpr renders the shell expression that prints the tool's absolute
// path on a target box, or binMissing.
//
// EVERY remote command that names the tool must resolve it through this one
// expression. A non-interactive SSH shell's PATH contains neither ~/bin nor
// ~/.local/bin, and both install shapes are in use: a symlink at ~/bin on boxes
// that build the binary, and a copied artifact at ~/.local/bin on boxes
// carrying no worktree. When two call sites resolve differently the fleet goes
// split-brain — the probe reports a box as healthy and eligible for dispatch
// while a gather on the same box reports "command not found", which reads as
// "nothing is running there".
func (t Tool) resolveBinExpr() string {
	n := t.Name
	return `command -v ` + n +
		` || ([ -x "$HOME/bin/` + n + `" ] && echo "$HOME/bin/` + n + `")` +
		` || ([ -x "$HOME/.local/bin/` + n + `" ] && echo "$HOME/.local/bin/` + n + `")` +
		` || echo ` + binMissing
}

// HostHealth is one probed box.
//
//nolint:govet // fieldalignment: grouped by concern for readability.
type HostHealth struct {
	Err       error
	Host      string
	SSHUser   string
	PublicDNS string
	// BinaryPath is the absolute path the probe resolved for the tool. The
	// dispatcher must use it verbatim: a non-interactive SSH shell does not
	// include ~/bin, so a bare name produces a tmux session that dies instantly
	// with "command not found" — indistinguishable from a silent no-op.
	BinaryPath string
	// HeldLeases names the lock files actually held on this host. The count is
	// what gates eligibility; the names are what map a task back to its box.
	HeldLeases  []string
	DiskFreeGB  int
	Load1       float64
	NVMeFreeGB  int
	TmuxWindows int
	Cores       int
	HasBinary   bool
	// HasGitHubAuth reports that `gh api user` succeeded. A box without it
	// can reach the network and hold the binary and still complete no task.
	HasGitHubAuth bool
	Reachable     bool
	IsSelf        bool
}

// Leases counts the leases actually HELD on this host.
func (h HostHealth) Leases() int { return len(h.HeldLeases) }

// HoldsLeaseFor reports whether this host holds a live lease for target.
func (h HostHealth) HoldsLeaseFor(target string) bool {
	want := SanitizeSlug(target) + ".lock"
	for _, l := range h.HeldLeases {
		if l == want {
			return true
		}
	}
	return false
}

// Target returns the ssh destination for this host.
func (h HostHealth) Target() string {
	if h.SSHUser == "" {
		return h.PublicDNS
	}
	return h.SSHUser + "@" + h.PublicDNS
}

// LoadPerCore is the primary ranking signal.
func (h HostHealth) LoadPerCore() float64 {
	if h.Cores <= 0 {
		return h.Load1
	}
	return h.Load1 / float64(h.Cores)
}

// UsableDiskGB is the free space a run can actually use: the NVMe volume when
// present, otherwise the home filesystem. The Azure box's root is small and
// nearly full while its /mnt/nvme has hundreds of gigabytes, so judging it by
// root alone would wrongly exclude the most capable box in the fleet.
func (h HostHealth) UsableDiskGB() int {
	if h.NVMeFreeGB > h.DiskFreeGB {
		return h.NVMeFreeGB
	}
	return h.DiskFreeGB
}

// ProbeOptions tunes a fleet probe.
type ProbeOptions struct {
	SelfDNS          string
	SelfHostname     string
	SSHTimeout       time.Duration
	MinDiskGB        int
	MaxLeasesPerHost int
}

func (o *ProbeOptions) applyDefaults() {
	if o.SSHTimeout <= 0 {
		o.SSHTimeout = 25 * time.Second
	}
	if o.MinDiskGB <= 0 {
		o.MinDiskGB = 40
	}
	if o.MaxLeasesPerHost <= 0 {
		o.MaxLeasesPerHost = 2
	}
}

// SSHRunner executes a command on a remote host. Tests substitute a fake.
type SSHRunner interface {
	Run(ctx context.Context, target string, script string) (string, error)
}

// DefaultSSHRunner shells out to ssh with the same options the fleet scripts use.
type DefaultSSHRunner struct {
	Timeout time.Duration
}

func (r DefaultSSHRunner) Run(ctx context.Context, target, script string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		target, script,
	)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok && len(exitErr.Stderr) > 0 {
			return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return string(out), err
	}
	return string(out), nil
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// Probe probes every host concurrently and returns their health, ordered
// best-first by ScoreHosts.
func Probe(ctx context.Context, ssh SSHRunner, tool Tool, hosts []Host, opts ProbeOptions) []HostHealth {
	opts.applyDefaults()
	out := make([]HostHealth, len(hosts))
	var wg sync.WaitGroup
	for i := range hosts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = probeOne(ctx, ssh, tool, hosts[i], opts)
		}(i)
	}
	wg.Wait()
	ScoreHosts(out)
	return out
}

func probeOne(ctx context.Context, ssh SSHRunner, tool Tool, h Host, opts ProbeOptions) HostHealth {
	hh := HostHealth{
		Host:      h.Hostname,
		SSHUser:   h.SSHUser,
		PublicDNS: h.PublicDNS,
		IsSelf:    h.IsSelf(opts.SelfDNS, opts.SelfHostname),
	}
	raw, err := ssh.Run(ctx, h.Target(), tool.probeScript())
	if err != nil {
		hh.Err = err
		return hh
	}
	if perr := parseProbe(raw, &hh); perr != nil {
		hh.Err = perr
		return hh
	}
	hh.Reachable = true
	return hh
}

// parseProbe reads the delimited probe output into hh.
func parseProbe(raw string, hh *HostHealth) error {
	sections := splitSections(raw)
	if _, ok := sections["END"]; !ok {
		return fmt.Errorf("probe output truncated (no __END__ marker)")
	}

	if v := firstLine(sections["NPROC"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse nproc %q: %w", v, err)
		}
		hh.Cores = n
	}
	if v := firstLine(sections["LOAD"]); v != "" {
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return fmt.Errorf("parse loadavg %q", v)
		}
		f, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return fmt.Errorf("parse load1 %q: %w", fields[0], err)
		}
		hh.Load1 = f
	}

	// Parse df BY MOUNT POINT, never by row position: df collapses or omits
	// rows, and the optional /mnt/nvme block means the row count varies
	// between hosts.
	for mount, freeKB := range parseDF(sections["DF"]) {
		if mount == "/mnt/nvme" {
			hh.NVMeFreeGB = int(freeKB / 1024 / 1024)
			continue
		}
		// The home filesystem is whatever df reported for $HOME; on these boxes
		// that is "/". Take the largest non-nvme mount so an unusual layout
		// still yields a sane figure.
		if gb := int(freeKB / 1024 / 1024); gb > hh.DiskFreeGB {
			hh.DiskFreeGB = gb
		}
	}

	if v := firstLine(sections["TMUX"]); v != "" {
		// tmux list-windows counts WINDOWS, not sessions: one session with 30
		// windows is a busy box, and list-sessions would report it as "1".
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			hh.TmuxWindows = n
		}
	}
	for _, line := range strings.Split(sections["LEASES"], "\n") {
		if t := strings.TrimSpace(line); t != "" {
			hh.HeldLeases = append(hh.HeldLeases, t)
		}
	}
	if v := firstLine(sections["GH"]); v == "ok" {
		hh.HasGitHubAuth = true
	}
	if v := firstLine(sections["BIN"]); v != "" && v != binMissing {
		hh.HasBinary = true
		hh.BinaryPath = v
	}
	return nil
}

// splitSections turns the __MARKER__-delimited blob into a map.
func splitSections(raw string) map[string]string {
	out := make(map[string]string)
	current := ""
	var buf []string
	flush := func() {
		if current != "" {
			out[current] = strings.Join(buf, "\n")
		}
		buf = nil
	}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "__") && strings.HasSuffix(trimmed, "__") && len(trimmed) > 4 {
			flush()
			current = strings.Trim(trimmed, "_")
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return out
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// parseDF maps mount point -> available KB, skipping headers. `df -P`
// guarantees one record per line (no wrapping), with Available in field 3 and
// the mount point last.
func parseDF(s string) map[string]int64 {
	out := make(map[string]int64)
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[0] == "Filesystem" {
			continue
		}
		avail, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		out[fields[len(fields)-1]] = avail
	}
	return out
}

// Eligible reports whether this host can accept a new run.
func (h HostHealth) Eligible(tool Tool, opts ProbeOptions) (bool, string) {
	opts.applyDefaults()
	maxLeases := opts.MaxLeasesPerHost
	if tool.MaxLeasesPerHost > 0 {
		maxLeases = tool.MaxLeasesPerHost
	}
	switch {
	case !h.Reachable:
		if h.Err != nil {
			return false, "unreachable: " + h.Err.Error()
		}
		return false, "unreachable"
	case !h.HasBinary:
		return false, tool.Name + " not on PATH"
	case !h.HasGitHubAuth:
		return false, "GitHub auth invalid (run `gh auth login` there)"
	case h.UsableDiskGB() < opts.MinDiskGB:
		return false, fmt.Sprintf("only %dGB free (need %dGB)", h.UsableDiskGB(), opts.MinDiskGB)
	case h.Leases() >= maxLeases:
		return false, fmt.Sprintf("already holds %d %s leases", h.Leases(), tool.Name)
	}
	return true, ""
}

// ScoreHosts orders hosts best-first: lowest load per core, then fewest tmux
// windows, then most free disk. Ties break on hostname so two concurrent
// dispatches make the SAME deterministic choice rather than both racing for a
// nondeterministic "best".
func ScoreHosts(hosts []HostHealth) {
	sort.SliceStable(hosts, func(i, j int) bool {
		a, b := hosts[i], hosts[j]
		if a.Reachable != b.Reachable {
			return a.Reachable
		}
		if la, lb := a.LoadPerCore(), b.LoadPerCore(); la != lb {
			return la < lb
		}
		if a.TmuxWindows != b.TmuxWindows {
			return a.TmuxWindows < b.TmuxWindows
		}
		if a.UsableDiskGB() != b.UsableDiskGB() {
			return a.UsableDiskGB() > b.UsableDiskGB()
		}
		return a.Host < b.Host
	})
}

// PickHost returns the best eligible host, or an error explaining why every
// candidate was rejected — a dispatch that silently finds nothing is impossible
// to debug.
func PickHost(hosts []HostHealth, tool Tool, opts ProbeOptions) (HostHealth, error) {
	ScoreHosts(hosts)
	var reasons []string
	for i := range hosts {
		ok, why := hosts[i].Eligible(tool, opts)
		if ok {
			return hosts[i], nil
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", hosts[i].Host, why))
	}
	return HostHealth{}, fmt.Errorf("no eligible host (%s)", strings.Join(reasons, "; "))
}

// FindLeaseHolder returns the host holding a live lease for target, if any.
// This is what maps a task back to the box actually running it — impossible
// with only a lease count.
func FindLeaseHolder(hosts []HostHealth, target string) (HostHealth, bool) {
	for i := range hosts {
		if hosts[i].HoldsLeaseFor(target) {
			return hosts[i], true
		}
	}
	return HostHealth{}, false
}
