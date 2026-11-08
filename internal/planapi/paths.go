package planapi

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// MaxPlanBytes is the largest file aimd reads as a plan (1 MB). A long
// plan is tens of KB.
const MaxPlanBytes = 1 << 20

// planFile is a plan file that passed the checks: its resolved path and
// contents.
type planFile struct {
	path string // as resolved (symlinks followed)
	src  []byte
}

// checkPath applies the file rules to a path a client named, without
// reading the file: it must be absolute and name a .md file (after
// symlinks too), which must exist, be a regular file of at most
// MaxPlanBytes, and lie inside a configured root or a repo the worktree
// scanner knows. It returns the resolved path.
func (s *Service) checkPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", badRequest("path is required")
	}
	if !filepath.IsAbs(path) {
		return "", badRequest("path must be absolute: %q", path)
	}
	path = filepath.Clean(path)
	if !isMarkdown(path) {
		return "", badRequest("only .md files can be plans: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		if !s.allowed(path) {
			return "", outside(path) // don't tell whether files outside exist
		}
		return "", notFound("no such file: %s", path)
	}
	if err != nil {
		return "", badRequest("can't resolve %s: %v", path, unwrapPath(err))
	}
	if !isMarkdown(resolved) {
		return "", badRequest("%s is a symlink to %s, which isn't a .md file", path, resolved)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", badRequest("can't read %s: %v", path, unwrapPath(err))
	}
	if !fi.Mode().IsRegular() {
		return "", badRequest("%s isn't a regular file", resolved)
	}
	if fi.Size() > MaxPlanBytes {
		return "", tooLarge(resolved, fi.Size())
	}
	if !s.allowed(resolved) {
		return "", outside(resolved)
	}
	return resolved, nil
}

func outside(path string) error {
	return refuse(http.StatusForbidden,
		"%s is outside the configured roots and every repo aimd knows; aimd only reads plans there (add its folder to roots in config.toml)", path)
}

// readPlanFile checks path (checkPath) and reads the file. A file that
// grew past MaxPlanBytes since the check is refused too.
func (s *Service) readPlanFile(path string) (planFile, error) {
	resolved, err := s.checkPath(path)
	if err != nil {
		return planFile{}, err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return planFile{}, badRequest("can't read %s: %v", resolved, unwrapPath(err))
	}
	defer f.Close()
	src, err := io.ReadAll(io.LimitReader(f, MaxPlanBytes+1))
	if err != nil {
		return planFile{}, badRequest("can't read %s: %v", resolved, err)
	}
	if len(src) > MaxPlanBytes {
		return planFile{}, tooLarge(resolved, int64(len(src)))
	}
	return planFile{path: resolved, src: src}, nil
}

func tooLarge(path string, size int64) error {
	return refuse(http.StatusRequestEntityTooLarge, "%s is %d bytes; a plan file can be at most %d (1 MB)", path, size, MaxPlanBytes)
}

func isMarkdown(path string) bool { return strings.EqualFold(filepath.Ext(path), ".md") }

// unwrapPath drops the path from a *fs.PathError, which the message names
// already.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// allowed reports whether an absolute, resolved path lies inside a
// configured root (resolved too) or inside a repo the worktree scanner
// knows: a main checkout or any of its worktrees, wherever they are.
func (s *Service) allowed(path string) bool {
	for _, root := range s.cfg.Roots {
		if root == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		if within(path, root) {
			return true
		}
	}
	if scan := s.lastScan(); scan != nil {
		for i := range scan.Repos {
			repo := &scan.Repos[i]
			if repo.Path != "" && within(path, repo.Path) {
				return true
			}
			for j := range repo.Worktrees {
				if w := repo.Worktrees[j].Path; w != "" && within(path, w) {
					return true
				}
			}
		}
	}
	return false
}

// within reports whether p is dir or inside it, comparing the way the
// worktree scanner does (case-insensitively on macOS).
func within(p, dir string) bool {
	p, dir = worktrees.PathKey(p), worktrees.PathKey(dir)
	if p == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(p, dir)
}
