package adopt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LaneDiff is one adoption decision that differs between two binaries.
type LaneDiff struct {
	ID      string `json:"id"`
	MoveA   string `json:"move_a"`
	MoveB   string `json:"move_b"`
	ReasonA string `json:"reason_class_a"`
	ReasonB string `json:"reason_class_b"`
}

// DiffBinaries snapshots dir once, runs both binaries against that snapshot,
// and returns only move/reason-class changes. The caller decides how to print
// the result and whether a non-empty result is a failed gate.
func DiffBinaries(ctx context.Context, binA, binB, dir string) ([]LaneDiff, error) {
	if strings.TrimSpace(binA) == "" || strings.TrimSpace(binB) == "" || strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("adopt diff requires --bin-a, --bin-b, and --dir")
	}
	snapshot, err := os.MkdirTemp("", "orca-adopt-diff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(snapshot)
	if err := copyTree(dir, snapshot); err != nil {
		return nil, fmt.Errorf("snapshot run: %w", err)
	}
	a, err := runJSON(ctx, binA, snapshot)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", binA, err)
	}
	b, err := runJSON(ctx, binB, snapshot)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", binB, err)
	}
	byID := func(rows []DryRunLane) map[string]DryRunLane {
		out := make(map[string]DryRunLane, len(rows))
		for i := range rows {
			out[rows[i].ID] = rows[i]
		}
		return out
	}
	ma, mb := byID(a), byID(b)
	ids := make([]string, 0, len(ma)+len(mb))
	seen := map[string]bool{}
	for id := range ma {
		seen[id] = true
		ids = append(ids, id)
	}
	for id := range mb {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	slicesSort(ids)
	var diffs []LaneDiff
	for _, id := range ids {
		x, y := ma[id], mb[id]
		if x.Move == y.Move && x.ReasonClass == y.ReasonClass {
			continue
		}
		diffs = append(diffs, LaneDiff{ID: id, MoveA: x.Move, MoveB: y.Move, ReasonA: x.ReasonClass, ReasonB: y.ReasonClass})
	}
	return diffs, nil
}

func runJSON(ctx context.Context, binary, dir string) ([]DryRunLane, error) {
	cmd := exec.CommandContext(ctx, binary, "adopt", "--dry-run", "--json", "--dir", dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var out []DryRunLane
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		var row DryRunLane
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("parse JSON output: %w", err)
		}
		out = append(out, row)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
}

func slicesSort(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
