package adopt

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SyncWorkspace refreshes the disposable session workspace from observed run
// files. Symlinks and executable environment files are excluded deliberately:
// session file tools must never follow a link back into the live directory.
func SyncWorkspace(source, dest string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return err
	}
	if source == dest || strings.HasPrefix(dest, source+string(filepath.Separator)) || strings.HasPrefix(source, dest+string(filepath.Separator)) {
		return fmt.Errorf("shadow workspace overlaps source")
	}
	if err := workspaceDir(dest); err != nil {
		return err
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if strings.HasPrefix(name, ".") && name != ".orca-inbox" {
				return filepath.SkipDir
			}
			return workspaceDir(filepath.Join(dest, rel))
		}
		// Only run evidence belongs in a shadow workspace. env.sh may contain credentials.
		if !workspaceEvidence(rel, name) {
			return nil
		}
		seen[rel] = true
		from, err := os.Open(path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		to, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(to, from)
		fromCloseErr := from.Close()
		closeErr := to.Close()
		if err != nil {
			return err
		}
		if fromCloseErr != nil {
			return fromCloseErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	// A removed host hold/report must disappear from the disposable copy too;
	// otherwise the session can act on stale evidence after a refresh.
	return filepath.WalkDir(dest, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != dest && strings.HasPrefix(entry.Name(), ".") && entry.Name() != ".orca-inbox" {
			return filepath.SkipDir
		}
		rel, relErr := filepath.Rel(dest, path)
		if relErr != nil || rel == "." || entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		name := entry.Name()
		if workspaceEvidence(rel, name) && !seen[rel] {
			return os.Remove(path)
		}
		return nil
	})
}

func workspaceEvidence(rel, name string) bool {
	rel = filepath.ToSlash(rel)
	return name == "EVENTS" || name == "state.json" || strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".done") || strings.HasSuffix(name, ".needs-swe") || strings.HasSuffix(name, ".hold-advance") || strings.HasSuffix(name, ".brief") || name == "heartbeat" || strings.HasSuffix(name, ".signal") || strings.HasPrefix(rel, "mail/") || strings.HasPrefix(rel, "chat/") || strings.HasPrefix(rel, ".orca-inbox/")
}

// A session can create a symlink in its copy. Refuse to refresh through one,
// including a symlink in a parent, so refresh cannot write into the live run.
func workspaceDir(dir string) error {
	for parent := filepath.Clean(dir); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace directory contains a symlink")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	return os.MkdirAll(dir, 0700)
}
