package prdozer

import "context"

// The probe fixtures and fake SSH live in //fleet now, alongside the parser
// they exercise. prdozer's dispatch-planning tests still need a stand-in host
// that looks healthy, so they get a minimal one here rather than reaching
// across a module boundary for test-only symbols.

// awsProbeBlob is a healthy box with prdozer installed and nothing held.
//
// The __GH__ section reports whether `gh api user` succeeds. Eligibility gates
// on it because every task shells out to gh, and a box with an expired token
// fails a second after dispatch. A fixture without it leaves the host
// ineligible — fail closed by design.
//
// The __LEASES__ section lists HELD lock names, so an idle box emits nothing
// there. It is not a count — a count cannot answer "which host is running
// this task".
const awsProbeBlob = `__NPROC__
8
__LOAD__
0.50 0.40 0.30 1/100 1
__DF__
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/root        506771172 345397612 161357176      69% /
__TMUX__
3
__LEASES__
__GH__
ok
__BIN__
/home/ubuntu/bin/prdozer
__END__
`

// fakeSSH serves canned probe output per target.
type fakeSSH struct {
	out  map[string]string
	errs map[string]error
	seen []string
}

func (f *fakeSSH) Run(_ context.Context, target, _ string) (string, error) {
	f.seen = append(f.seen, target)
	if err, ok := f.errs[target]; ok {
		return "", err
	}
	return f.out[target], nil
}
