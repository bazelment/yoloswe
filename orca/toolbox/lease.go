package toolbox

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// LeaseAuthority identifies one completed handover. A re-adoption or release
// invalidates an old session's authority without weakening the host guard.
type LeaseAuthority struct {
	Holder string `json:"holder"`
	Marker string `json:"marker"`
	State  string `json:"state"`
}

func ReadLeaseAuthority(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".orca-lease.json"))
	if err != nil {
		return "", err
	}
	var lease LeaseAuthority
	if err := json.Unmarshal(raw, &lease); err != nil {
		return "", err
	}
	if lease.State != "adopted" || lease.Holder == "" || lease.Marker == "" {
		return "", refuse("adopted lease is not current")
	}
	data, err := json.Marshal(lease)
	return string(data), err
}

// AdoptedLeaseGuard is sourced only in Orca-owned helper processes, after the
// skill has defined its usual stand-down guard. Each guarded operation checks
// the run path and exact handover again; no installed skill file is changed.
const AdoptedLeaseGuard = `
orca_adopted_lease_check() {
  python3 - "$1" <<'ORCA_LEASE_PY'
import json,os,sys
try:
 root=os.path.realpath(os.environ["ORCA_TOOLBOX_DIR"])
 if os.path.realpath(sys.argv[1]) != root: raise ValueError()
 expected=json.loads(os.environ["ORCA_ADOPT_AUTH"])
 with open(os.path.join(root,".orca-lease.json")) as f: current=json.load(f)
 if expected.get("state") != "adopted" or not expected.get("holder") or not expected.get("marker"): raise ValueError()
 if any(current.get(k) != expected[k] for k in ("holder","marker","state")): raise ValueError()
except Exception:
 print("orca: refused: adopted lease is no longer current",file=sys.stderr)
 raise SystemExit(2)
ORCA_LEASE_PY
}
if [[ -n ${ORCA_ADOPT_AUTH:-} ]]; then
  sw_orca_lease_check() { orca_adopted_lease_check "$@"; }
  snl_orca_lease_check() { orca_adopted_lease_check "$@"; }
fi
`
