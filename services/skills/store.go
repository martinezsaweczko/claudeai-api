package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	versionsDirName = "versions"
	tmpPrefix       = ".tmp-"
	keepVersions    = 3
	staleTmpAge     = 10 * time.Minute
)

// skillsSubdir is where the CLI looks for project skills relative to its cwd.
var skillsSubdir = filepath.Join(".claude", "skills")

// materialize writes the validated skills into versions/<commit> using a temp
// directory plus an atomic rename, and makes the tree read-only. If the version
// already exists it is reused untouched. Nothing but validated skill files is
// written: no symlinks, no executable bits, no hooks/MCP/agents/commands.
func materialize(stateDir, commit string, skills []loadedSkill) (string, error) {
	versions := filepath.Join(stateDir, versionsDirName)
	final := filepath.Join(versions, commit)
	if st, err := os.Stat(final); err == nil && st.IsDir() {
		now := time.Now()
		_ = os.Chtimes(final, now, now) // keep the in-use version newest for pruning
		return final, nil
	}
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return "", fmt.Errorf("creating versions directory: %w", err)
	}
	tmp, err := os.MkdirTemp(versions, tmpPrefix+commit[:min(12, len(commit))]+"-")
	if err != nil {
		return "", fmt.Errorf("creating temp version: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = removeAll(tmp)
		}
	}()

	root := filepath.Join(tmp, skillsSubdir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	for _, sk := range skills {
		for _, f := range sk.Files {
			dest := filepath.Join(root, sk.Info.Name, filepath.FromSlash(f.Rel))
			// Defense in depth: the path must stay inside the skill root.
			if rel, err := filepath.Rel(root, dest); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("refusing to write outside the skills directory")
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(dest, f.Data, 0o444); err != nil {
				return "", err
			}
		}
	}
	if err := chmodTree(tmp, 0o555, 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		// Lost a race with another process that produced the same version.
		if st, serr := os.Stat(final); serr == nil && st.IsDir() {
			return final, nil
		}
		return "", fmt.Errorf("publishing version: %w", err)
	}
	ok = true
	return final, nil
}

// chmodTree sets directory and file modes bottom-up so parents become
// read-only last.
func chmodTree(root string, dirMode, fileMode fs.FileMode) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		return os.Chmod(p, fileMode)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], dirMode); err != nil {
			return err
		}
	}
	return nil
}

// removeAll deletes path even when it holds read-only directories.
func removeAll(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// prune removes old versions, keeping the current one plus the most recent
// others up to keepVersions in total, and stale temp directories. It is best
// effort and returns the first error for logging.
func prune(stateDir, current string) error {
	versions := filepath.Join(stateDir, versionsDirName)
	entries, err := os.ReadDir(versions)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type ver struct {
		name string
		mod  time.Time
	}
	var olds []ver
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			if time.Since(info.ModTime()) > staleTmpAge {
				if rerr := removeAll(filepath.Join(versions, e.Name())); rerr != nil && firstErr == nil {
					firstErr = rerr
				}
			}
			continue
		}
		if e.Name() != filepath.Base(current) {
			olds = append(olds, ver{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(olds, func(i, j int) bool {
		if !olds[i].mod.Equal(olds[j].mod) {
			return olds[i].mod.After(olds[j].mod)
		}
		return olds[i].name > olds[j].name
	})
	for i := keepVersions - 1; i < len(olds); i++ {
		if rerr := removeAll(filepath.Join(versions, olds[i].name)); rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}
	return firstErr
}
