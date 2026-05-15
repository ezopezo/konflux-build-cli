package integration_tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	. "github.com/konflux-ci/konflux-build-cli/integration_tests/framework"
)

const gitCloneRunnerImage = TaskRunnerImageRef

type gitCloneResult struct {
	Commit    string `json:"commit"`
	MergedSha string `json:"mergedSha,omitempty"`
}

// parseGitCloneResult finds the last JSON object line in stdout with a non-empty
// commit field, so unrelated log lines or earlier JSON fragments do not win.
func parseGitCloneResult(stdout string) (gitCloneResult, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r gitCloneResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Commit != "" {
			return r, nil
		}
	}
	return gitCloneResult{}, fmt.Errorf("no result found")
}

func startGitCloneContainer(t *testing.T, workspaceDir string) *TestRunnerContainer {
	t.Helper()
	Expect(os.MkdirAll(filepath.Join(workspaceDir, "home"), 0755)).To(Succeed())
	Expect(os.MkdirAll(filepath.Join(workspaceDir, "work"), 0755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(workspaceDir, "git.config"), []byte("[safe]\n\tdirectory = *\n[protocol \"file\"]\n\tallow = always\n"), 0644)).To(Succeed())

	container := NewBuildCliRunnerContainer(GenerateUniqueTag(t), gitCloneRunnerImage)
	container.AddVolumeWithOptions(workspaceDir, "/workspace", "z")
	container.SetWorkdir("/workspace/work")
	container.AddEnv("HOME", "/workspace/home")
	container.AddEnv("GIT_CONFIG_GLOBAL", "/workspace/git.config")
	Expect(container.Start()).To(Succeed())
	t.Cleanup(func() { container.DeleteIfExists() })
	return container
}

func TestGitClone(t *testing.T) {
	tests := []struct {
		name    string
		skip    func() bool
		setup   func(t *testing.T, workspaceDir string) map[string]string
		url     string
		args    []string
		wantErr bool
		check   func(t *testing.T, workspaceDir, stdout, stderr string, setupData map[string]string)
	}{
		{
			name: "clone full history",
			setup: func(t *testing.T, workspaceDir string) map[string]string {
				repo := createLocalTestRepo(t)
				bareCloneToPath(t, repo.Path, filepath.Join(workspaceDir, "repo.git"))
				return nil
			},
			url:  "file:///workspace/repo.git",
			args: []string{"--depth", "0", "--submodules=false"},
			check: func(t *testing.T, workspaceDir, stdout, stderr string, _ map[string]string) {
				Expect(filepath.Join(workspaceDir, "out", "README.md")).To(BeAnExistingFile())
				Expect(filepath.Join(workspaceDir, "out", "second.txt")).To(BeAnExistingFile())

				isShallow := runGit(t, filepath.Join(workspaceDir, "out"), "rev-parse", "--is-shallow-repository")
				Expect(isShallow).To(Equal("false"))
			},
		},
		{
			name: "clone leaves preconfigured HOME gitconfig unchanged",
			setup: func(t *testing.T, workspaceDir string) map[string]string {
				home := filepath.Join(workspaceDir, "home")
				Expect(os.MkdirAll(home, 0755)).To(Succeed())
				marker := "[user]\n\tname = kbc-git-clone-test\n\temail = kbc-git-clone-test@example.com\n"
				Expect(os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(marker), 0644)).To(Succeed())
				repo := createLocalTestRepo(t)
				bareCloneToPath(t, repo.Path, filepath.Join(workspaceDir, "repo.git"))
				return map[string]string{"homeGitconfig": marker}
			},
			url:  "file:///workspace/repo.git",
			args: []string{"--depth", "0", "--submodules=false"},
			check: func(t *testing.T, workspaceDir, stdout, stderr string, setupData map[string]string) {
				Expect(filepath.Join(workspaceDir, "out", "README.md")).To(BeAnExistingFile())
				b, err := os.ReadFile(filepath.Join(workspaceDir, "home", ".gitconfig"))
				Expect(err).ToNot(HaveOccurred())
				Expect(string(b)).To(Equal(setupData["homeGitconfig"]))
			},
		},
		{
			name: "shallow clone at tag revision",
			setup: func(t *testing.T, workspaceDir string) map[string]string {
				repo := createLocalTestRepo(t)
				bareCloneToPath(t, repo.Path, filepath.Join(workspaceDir, "repo.git"))
				return map[string]string{"expectedCommit": repo.TagCommit}
			},
			url:  "file:///workspace/repo.git",
			args: []string{"--depth", "1", "--revision", "v1.0.0", "--submodules=false"},
			check: func(t *testing.T, workspaceDir, stdout, stderr string, setupData map[string]string) {
				_, err := os.Stat(filepath.Join(workspaceDir, "out", "second.txt"))
				Expect(err).To(MatchError(os.ErrNotExist))

				head := runGit(t, filepath.Join(workspaceDir, "out"), "rev-parse", "HEAD")
				Expect(head).To(Equal(setupData["expectedCommit"]))
			},
		},
		{
			name: "sparse checkout single directory",
			setup: func(t *testing.T, workspaceDir string) map[string]string {
				repo := createLocalTestRepo(t)
				bareCloneToPath(t, repo.Path, filepath.Join(workspaceDir, "repo.git"))
				return nil
			},
			url:  "file:///workspace/repo.git",
			args: []string{"--depth", "0", "--sparse-checkout-directories", "src", "--submodules=false"},
			check: func(t *testing.T, workspaceDir, stdout, stderr string, _ map[string]string) {
				Expect(filepath.Join(workspaceDir, "out", "src", "file.txt")).To(BeAnExistingFile())
				_, err := os.Stat(filepath.Join(workspaceDir, "out", "docs"))
				Expect(err).To(MatchError(os.ErrNotExist))
			},
		},
		{
			name: "clone with submodules",
			setup: func(t *testing.T, workspaceDir string) map[string]string {
				headCommit := prepareBareRepoWithSubmodule(t, workspaceDir)
				return map[string]string{"expectedCommit": headCommit}
			},
			url:  "file:///workspace/main-bare.git",
			args: []string{"--depth", "0", "--submodules=true"},
			check: func(t *testing.T, workspaceDir, stdout, stderr string, setupData map[string]string) {
				content, err := os.ReadFile(filepath.Join(workspaceDir, "out", "my-submodule", "sub-file.txt"))
				Expect(err).ToNot(HaveOccurred())
				Expect(string(content)).To(Equal("submodule content\n"))

				head := runGit(t, filepath.Join(workspaceDir, "out"), "rev-parse", "HEAD")
				Expect(head).To(Equal(setupData["expectedCommit"]))
			},
		},
		{
			name: "merge feature into target branch",
			setup: func(t *testing.T, workspaceDir string) map[string]string {
				prepareBareRepoWithFeatureBranch(t, workspaceDir)
				return nil
			},
			url:  "file:///workspace/merge-bare.git",
			args: []string{"--depth", "0", "--revision", "feature", "--merge-target-branch", "--target-branch", "main", "--merge-source-depth", "0", "--submodules=false"},
			check: func(t *testing.T, workspaceDir, stdout, stderr string, _ map[string]string) {
				Expect(filepath.Join(workspaceDir, "out", "feature-only.txt")).To(BeAnExistingFile())
				Expect(filepath.Join(workspaceDir, "out", "main-only.txt")).To(BeAnExistingFile())

				result, err := parseGitCloneResult(stdout)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.MergedSha).ToNot(BeEmpty())
			},
		},
		{
			name: "delete existing output before clone",
			setup: func(t *testing.T, workspaceDir string) map[string]string {
				repo := createLocalTestRepo(t)
				bareCloneToPath(t, repo.Path, filepath.Join(workspaceDir, "repo.git"))
				outDir := filepath.Join(workspaceDir, "out")
				Expect(os.MkdirAll(outDir, 0755)).To(Succeed())
				// Chmod to 0777 to allow the container user to delete the stale.txt file.
				// Use a separate Chmod rather than passing 0777 to MkdirAll,
				// because MkdirAll respects umask so the result may not actually be 0777.
				Expect(os.Chmod(outDir, 0777)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(outDir, "stale.txt"), []byte("stale"), 0644)).To(Succeed())
				return nil
			},
			url:  "file:///workspace/repo.git",
			args: []string{"--depth", "0", "--submodules=false", "--delete-existing"},
			check: func(t *testing.T, workspaceDir, stdout, stderr string, _ map[string]string) {
				Expect(filepath.Join(workspaceDir, "out", "README.md")).To(BeAnExistingFile())
				_, err := os.Stat(filepath.Join(workspaceDir, "out", "stale.txt"))
				Expect(err).To(MatchError(os.ErrNotExist))
			},
		},
		{
			name:    "fail on nonexistent repo",
			setup:   func(t *testing.T, workspaceDir string) map[string]string { return nil },
			url:     "file:///workspace/nonexistent.git",
			args:    []string{"--depth", "0", "--submodules=false", "--retry-max-attempts", "1"},
			wantErr: true,
			check:   func(t *testing.T, workspaceDir, stdout, stderr string, _ map[string]string) {},
		},
		{
			name: "reject external symlink",
			skip: func() bool { return runtime.GOOS == "windows" },
			setup: func(t *testing.T, workspaceDir string) map[string]string {
				prepareBareRepoWithExternalSymlink(t, workspaceDir)
				return nil
			},
			url:     "file:///workspace/bad-symlink-bare.git",
			args:    []string{"--depth", "0", "--enable-symlink-check=true", "--submodules=false"},
			wantErr: true,
			check: func(t *testing.T, workspaceDir, stdout, stderr string, _ map[string]string) {
				Expect(stderr).To(ContainSubstring("symlink"))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip != nil && tc.skip() {
				t.Skip()
			}
			SetupGomega(t)

			workspaceDir, err := CreateTempDir("gitclone-test-")
			Expect(err).ToNot(HaveOccurred())
			t.Cleanup(func() { os.RemoveAll(workspaceDir) })
			setupData := tc.setup(t, workspaceDir)
			container := startGitCloneContainer(t, workspaceDir)

			args := append([]string{"git-clone", "--url", tc.url, "--output-dir", "/workspace/out", "--ssl-verify=false"}, tc.args...)
			stdout, stderr, err := container.ExecuteBuildCliWithOutput(args...)

			if tc.wantErr {
				Expect(err).To(HaveOccurred())
			} else {
				Expect(err).ToNot(HaveOccurred(), "stderr: %s", stderr)
			}
			tc.check(t, workspaceDir, stdout, stderr, setupData)
		})
	}
}
