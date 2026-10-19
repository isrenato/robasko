// Package paths resolves the private data root.
//
// Every artifact holding personal data - the real CV, the skills inventory,
// the profile, generated letters, cached advertisements, logs and database
// dumps - is written beneath this root, which lies outside the repository
// working tree. Keeping the data physically outside the tree is what makes
// the boundary a property of the layout rather than a rule to remember; the
// ignore rules in .gitignore are the second layer, not the first.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dirName is the per-user directory created beneath the resolved base.
const dirName = "robasko"

// ErrRootInsideRepo reports a data root that resolves to a location inside
// the repository working tree, which would place private artifacts back
// under version control's nose.
var ErrRootInsideRepo = errors.New("data root resolves inside the repository working tree")

// ErrNoHome reports that neither an override nor XDG_DATA_HOME was set and
// the user's home directory could not be determined.
var ErrNoHome = errors.New("cannot determine home directory for the data root")

// DataRoot returns the private data root, resolved in this order:
//
//  1. $ROBASKO_DATA_DIR, used as given
//  2. $XDG_DATA_HOME/robasko
//  3. ~/.local/share/robasko
//
// repoRoot is the repository working tree. A resolved root at or beneath it
// is rejected with ErrRootInsideRepo rather than returned, so a misconfigured
// override fails loudly at startup instead of quietly reintroducing the
// problem this package exists to prevent.
//
// The directory is not created; see EnsureDataRoot.
func DataRoot(repoRoot string) (string, error) {
	root, err := resolveRoot()
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)

	inside, err := isInside(root, repoRoot)
	if err != nil {
		return "", err
	}
	if inside {
		return "", fmt.Errorf("%w: %s is inside %s", ErrRootInsideRepo, root, filepath.Clean(repoRoot))
	}
	return root, nil
}

// EnsureDataRoot resolves the data root and creates it, and any missing
// parent, with owner-only permissions. An existing directory's permissions
// are tightened to 0700 as well, since a root created by an earlier version
// or by hand may be more permissive than the private tier allows.
func EnsureDataRoot(repoRoot string) (string, error) {
	root, err := DataRoot(repoRoot)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating data root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("tightening data root permissions: %w", err)
	}
	return root, nil
}

// resolveRoot applies the three-branch precedence without validating the
// result against the repository.
func resolveRoot() (string, error) {
	if override := os.Getenv("ROBASKO_DATA_DIR"); override != "" {
		return override, nil
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", ErrNoHome
	}
	return filepath.Join(home, ".local", "share", dirName), nil
}

// isInside reports whether candidate is repoRoot or sits beneath it.
//
// Both paths are resolved through any symlinks first. On macOS the temporary
// directories used by tests live under /var, which is itself a symlink to
// /private/var, so a purely lexical comparison would miss a containment that
// is real on disk.
func isInside(candidate, repoRoot string) (bool, error) {
	if repoRoot == "" {
		return false, nil
	}
	c, err := realPath(candidate)
	if err != nil {
		return false, err
	}
	r, err := realPath(repoRoot)
	if err != nil {
		return false, err
	}
	if c == r {
		return true, nil
	}
	return strings.HasPrefix(c, r+string(os.PathSeparator)), nil
}

// realPath resolves symlinks in the deepest existing ancestor of p and
// re-appends the part that does not exist yet, so a path that has not been
// created is still compared on its real location.
func realPath(p string) (string, error) {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", p, err)
		}
		p = abs
	}

	remainder := ""
	current := p
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolving %s: %w", p, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the root without finding anything that exists.
			return p, nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}
