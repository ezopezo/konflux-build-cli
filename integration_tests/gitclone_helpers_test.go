package integration_tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/konflux-ci/konflux-build-cli/pkg/cliwrappers"
)

// runGit runs a git command in dir and returns trimmed stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"-c", "safe.directory=" + dir}, args...)
	stdout, stderr, code, err := cliwrappers.NewCliExecutor().Execute(cliwrappers.Cmd{
		Name: "git", Args: fullArgs, Dir: dir,
	})
	if err != nil || code != 0 {
		t.Fatalf("git %s failed (exit %d): %v\nstderr: %s", args[0], code, err, stderr)
	}
	return strings.TrimSpace(stdout)
}

// bareCloneToPath creates a bare clone of src at dest, simulating a remote repo.
func bareCloneToPath(t *testing.T, src, dest string) {
	t.Helper()
	_ = os.RemoveAll(dest)
	runGit(t, "", "clone", "--bare", src, dest)
}

// initGitRepo creates a directory and initializes a git repo with "main" branch.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
}

// testGitRepoInfo captures the standard local fixture repo path and the commit
// SHA of the annotated tag v1.0.0 (first commit) for shallow-at-tag assertions.
type testGitRepoInfo struct {
	Path      string
	TagCommit string
}

// createLocalTestRepo creates a repo with two commits and tag v1.0.0 on the first, containing src/ and docs/ directories.
func createLocalTestRepo(t *testing.T) testGitRepoInfo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	initGitRepo(t, dir)

	for _, subdir := range []string{"src", "docs"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, subdir, "file.txt"), []byte(subdir), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "first")
	runGit(t, dir, "tag", "-a", "v1.0.0", "-m", "v1.0.0")
	tagCommit := runGit(t, dir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "second")

	return testGitRepoInfo{Path: dir, TagCommit: tagCommit}
}

// prepareBareRepoWithSubmodule creates a bare repo with a git submodule and returns the HEAD commit SHA.
func prepareBareRepoWithSubmodule(t *testing.T, root string) string {
	t.Helper()

	subDir := filepath.Join(root, "_sub")
	initGitRepo(t, subDir)
	if err := os.WriteFile(filepath.Join(subDir, "sub-file.txt"), []byte("submodule content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, subDir, "add", "-A")
	runGit(t, subDir, "commit", "-m", "sub commit")
	bareCloneToPath(t, subDir, filepath.Join(root, "sub-bare.git"))

	mainDir := filepath.Join(root, "_main")
	initGitRepo(t, mainDir)
	if err := os.WriteFile(filepath.Join(mainDir, "main.txt"), []byte("main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainDir, "add", "-A")
	runGit(t, mainDir, "commit", "-m", "main commit")
	runGit(t, mainDir, "-c", "protocol.file.allow=always", "submodule", "add", "../sub-bare.git", "my-submodule")
	runGit(t, mainDir, "commit", "-m", "add submodule")
	bareCloneToPath(t, mainDir, filepath.Join(root, "main-bare.git"))

	head := runGit(t, filepath.Join(root, "main-bare.git"), "rev-parse", "HEAD")

	_ = os.RemoveAll(subDir)
	_ = os.RemoveAll(mainDir)
	return head
}

// prepareBareRepoWithFeatureBranch creates a bare repo with diverged main and feature branches for merge testing.
func prepareBareRepoWithFeatureBranch(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "_merge")
	initGitRepo(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "base")

	runGit(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature-only.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "feature")

	runGit(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "main-only.txt"), []byte("main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "main")

	bareCloneToPath(t, dir, filepath.Join(root, "merge-bare.git"))
	_ = os.RemoveAll(dir)
}

// prepareBareRepoWithExternalSymlink creates a bare repo containing a symlink pointing outside the repo.
func prepareBareRepoWithExternalSymlink(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "_symlink")
	initGitRepo(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ok\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "bad.link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "external symlink")

	bareCloneToPath(t, dir, filepath.Join(root, "bad-symlink-bare.git"))
	_ = os.RemoveAll(dir)
}
