package paths

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv removes every variable the resolver consults, so each test states
// its own starting point rather than inheriting the developer's shell.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ROBASKO_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
}

// repoOutside returns a repository root that no temporary data root will
// accidentally sit beneath.
func repoOutside(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestDataRootUsesOverrideAsGiven(t *testing.T) {
	clearEnv(t)
	want := t.TempDir()
	t.Setenv("ROBASKO_DATA_DIR", want)

	got, err := DataRoot(repoOutside(t))
	if err != nil {
		t.Fatalf("DataRoot returned error: %v", err)
	}
	if got != filepath.Clean(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDataRootUsesXDGDataHome(t *testing.T) {
	clearEnv(t)
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	got, err := DataRoot(repoOutside(t))
	if err != nil {
		t.Fatalf("DataRoot returned error: %v", err)
	}
	want := filepath.Join(xdg, "robasko")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDataRootFallsBackToHome(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := DataRoot(repoOutside(t))
	if err != nil {
		t.Fatalf("DataRoot returned error: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "robasko")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOverrideTakesPrecedenceOverXDG(t *testing.T) {
	clearEnv(t)
	override := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("ROBASKO_DATA_DIR", override)
	t.Setenv("XDG_DATA_HOME", xdg)

	got, err := DataRoot(repoOutside(t))
	if err != nil {
		t.Fatalf("DataRoot returned error: %v", err)
	}
	if got != filepath.Clean(override) {
		t.Errorf("override ignored: got %q, want %q", got, override)
	}
	if strings.HasPrefix(got, filepath.Clean(xdg)) {
		t.Errorf("resolved beneath XDG_DATA_HOME despite the override: %q", got)
	}
}

func TestRejectsRootBeneathRepository(t *testing.T) {
	clearEnv(t)
	repo := t.TempDir()
	inside := filepath.Join(repo, "data")
	t.Setenv("ROBASKO_DATA_DIR", inside)

	_, err := DataRoot(repo)
	if err == nil {
		t.Fatal("expected a root inside the repository to be rejected")
	}
	if !errors.Is(err, ErrRootInsideRepo) {
		t.Errorf("got %v, want ErrRootInsideRepo", err)
	}
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("error message does not name the offending path: %q", err)
	}
}

func TestRejectsRootEqualToRepository(t *testing.T) {
	clearEnv(t)
	repo := t.TempDir()
	t.Setenv("ROBASKO_DATA_DIR", repo)

	_, err := DataRoot(repo)
	if !errors.Is(err, ErrRootInsideRepo) {
		t.Errorf("got %v, want ErrRootInsideRepo", err)
	}
}

// The working tree may be reached through a symlink - on macOS every path
// under /var is - so containment must be judged on the real location rather
// than on the spelling of the path.
func TestRejectsRootReachedThroughSymlink(t *testing.T) {
	clearEnv(t)
	repo := t.TempDir()
	link := filepath.Join(t.TempDir(), "link-to-repo")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("ROBASKO_DATA_DIR", filepath.Join(link, "data"))

	_, err := DataRoot(repo)
	if !errors.Is(err, ErrRootInsideRepo) {
		t.Errorf("symlinked path escaped the containment check: got %v", err)
	}
}

func TestEnsureDataRootCreatesOwnerOnly(t *testing.T) {
	clearEnv(t)
	root := filepath.Join(t.TempDir(), "nested", "robasko")
	t.Setenv("ROBASKO_DATA_DIR", root)

	got, err := EnsureDataRoot(repoOutside(t))
	if err != nil {
		t.Fatalf("EnsureDataRoot returned error: %v", err)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("data root was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("data root is not a directory")
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("got mode %04o, want 0700", perm)
	}
}

func TestEnsureDataRootTightensExistingPermissions(t *testing.T) {
	clearEnv(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("preparing a permissive root: %v", err)
	}
	t.Setenv("ROBASKO_DATA_DIR", root)

	got, err := EnsureDataRoot(repoOutside(t))
	if err != nil {
		t.Fatalf("EnsureDataRoot returned error: %v", err)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("existing root left at %04o, want 0700", perm)
	}
}

func TestReportsMissingHome(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", "")

	_, err := DataRoot(repoOutside(t))
	if !errors.Is(err, ErrNoHome) {
		t.Errorf("got %v, want ErrNoHome", err)
	}
}

// A generated letter carries a name, an address and an employment
// history, and the repository is published. A file written there must be
// one git ignores, and that is checked rather than assumed.
//
// The check asks about a file, not the directory. The default-deny idiom
// un-ignores directories with `!*/` so git descends into them, so the
// directory reads as tracked while its contents are ignored.
func TestLettersDirectoryMustBeIgnoredByGit(t *testing.T) {
	repo := newRepo(t)

	dir, err := Letters(repo)
	if err != nil {
		t.Fatalf("Letters: %v", err)
	}
	if filepath.Dir(dir) != repo {
		t.Errorf("letters land at %q, outside the repository %q", dir, repo)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode %04o, want 0700", perm)
	}
}

// The protection this replaces was structural: letters outside the tree
// could not be committed at all. Here it is a check, so it has to fail
// when the ignore rules stop covering the directory - which is exactly
// what an allowlist entry added later would do.
func TestLettersRefuseADirectoryGitWouldTrack(t *testing.T) {
	repo := newRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"),
		[]byte("*\n!*/\n!/letters/**\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Letters(repo)
	if !errors.Is(err, ErrLettersNotIgnored) {
		t.Errorf("got %v, want ErrLettersNotIgnored", err)
	}
}

// A directory git has no opinion about cannot be committed either.
func TestLettersWorkOutsideARepository(t *testing.T) {
	if _, err := Letters(t.TempDir()); err != nil {
		t.Errorf("Letters outside a repository: %v", err)
	}
}

// newRepo returns a git repository whose ignore rules are default-deny,
// as the real one's are.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n!*/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSubdirRejectsNamesThatEscapeTheRoot(t *testing.T) {
	clearEnv(t)
	t.Setenv("ROBASKO_DATA_DIR", t.TempDir())

	for _, name := range []string{"", "..", "a/b", "/etc", filepath.Join("..", "escape")} {
		if _, err := Subdir(repoOutside(t), name); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
}
