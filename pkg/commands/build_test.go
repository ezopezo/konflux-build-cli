package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/keilerkonzept/dockerfile-json/pkg/dockerfile"
	"github.com/konflux-ci/konflux-build-cli/pkg/cliwrappers"
	"github.com/konflux-ci/konflux-build-cli/testutil"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	. "github.com/onsi/gomega"
)

func parseDockerfile(t *testing.T, g Gomega, content string) *dockerfile.Dockerfile {
	t.Helper()
	containerfilePath := filepath.Join(t.TempDir(), "Containerfile")
	os.WriteFile(containerfilePath, []byte(content), 0644)
	df, err := dockerfile.Parse(containerfilePath)
	g.Expect(err).ToNot(HaveOccurred())
	return df
}

func listDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	var filenames []string
	for _, entry := range entries {
		filenames = append(filenames, entry.Name())
	}
	return filenames, nil
}

func Test_Build_effectiveContextDir(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		context  string
		expected string
	}{
		{
			name:     "no source, relative context",
			context:  "ctx",
			expected: "ctx",
		},
		{
			name:     "no source, absolute context",
			context:  "/abs/path",
			expected: "/abs/path",
		},
		{
			name:     "source set, relative context",
			source:   "/src",
			context:  "ctx",
			expected: "/src/ctx",
		},
		{
			name:     "source set, dot context",
			source:   "/src",
			context:  ".",
			expected: "/src",
		},
		{
			name:     "source set, absolute context",
			source:   "/src",
			context:  "/abs/path",
			expected: "/abs/path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			c := &Build{Params: &BuildParams{Source: tc.source, Context: tc.context}}
			g.Expect(c.effectiveContextDir()).To(Equal(tc.expected))
		})
	}
}

func Test_Build_validateParams(t *testing.T) {
	g := NewWithT(t)

	tempDir := t.TempDir()

	os.WriteFile(filepath.Join(tempDir, "notadir"), []byte("content"), 0644)

	sourceDir := filepath.Join(tempDir, "source")
	os.MkdirAll(filepath.Join(sourceDir, "ctx"), 0755)
	os.MkdirAll(filepath.Join(tempDir, "outside-ctx"), 0755)

	tests := []struct {
		name         string
		params       BuildParams
		setupFunc    func() string // returns context directory
		errExpected  bool
		errSubstring string
	}{
		{
			name: "should allow valid parameters",
			params: BuildParams{
				OutputRef:     "quay.io/org/image:tag",
				Context:       tempDir,
				Containerfile: "",
			},
			errExpected: false,
		},
		{
			name: "should allow valid parameters with containerfile",
			params: BuildParams{
				OutputRef:     "registry.io/namespace/image:v1.0",
				Context:       tempDir,
				Containerfile: "Dockerfile",
			},
			errExpected: false,
		},
		{
			name: "should fail on invalid output-ref",
			params: BuildParams{
				OutputRef: "quay.io/org/imAge",
				Context:   tempDir,
			},
			errExpected:  true,
			errSubstring: "output-ref",
		},
		{
			name: "should fail on missing context directory",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Context:   filepath.Join(tempDir, "nonexistent"),
			},
			errExpected:  true,
			errSubstring: "does not exist",
		},
		{
			name: "should fail when context is a file not directory",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Context:   filepath.Join(tempDir, "notadir"),
			},
			errExpected:  true,
			errSubstring: "is not a directory",
		},
		{
			name: "should fail when legacy-build-timestamp and source-date-epoch are used together",
			params: BuildParams{
				OutputRef:            "quay.io/org/image:tag",
				Context:              tempDir,
				LegacyBuildTimestamp: "1",
				SourceDateEpoch:      "1",
			},
			errExpected:  true,
			errSubstring: "are mutually exclusive",
		},
		{
			name: "should fail when yum-repos-d-target is a relative path",
			params: BuildParams{
				OutputRef:       "quay.io/org/image:tag",
				Context:         tempDir,
				YumReposDTarget: "etc/yum.repos.d",
			},
			errExpected:  true,
			errSubstring: "yum-repos-d-target must be an absolute path",
		},
		{
			name: "should fail when prefetch-dir-copy already exists",
			params: BuildParams{
				OutputRef:       "quay.io/org/image:tag",
				Context:         tempDir,
				PrefetchDirCopy: tempDir,
			},
			errExpected:  true,
			errSubstring: "prefetch-dir-copy must not be an existing path",
		},
		{
			name: "should allow prefetch-dir-copy that does not exist",
			params: BuildParams{
				OutputRef:       "quay.io/org/image:tag",
				Context:         tempDir,
				PrefetchDirCopy: filepath.Join(tempDir, "nonexistent-copy-dir"),
			},
			errExpected: false,
		},
		{
			name: "should allow activation key with self-registration mount",
			params: BuildParams{
				OutputRef:           "quay.io/org/image:tag",
				Context:             tempDir,
				RHSMActivationKey:   "/path/to/key",
				RHSMOrg:             "/path/to/org",
				RHSMActivationMount: "/activation-key",
			},
			errExpected: false,
		},
		{
			name: "should allow activation key with pre-registration",
			params: BuildParams{
				OutputRef:                 "quay.io/org/image:tag",
				Context:                   tempDir,
				RHSMActivationKey:         "/path/to/key",
				RHSMOrg:                   "/path/to/org",
				RHSMActivationPreregister: true,
			},
			errExpected: false,
		},
		{
			name: "should allow activation key with both mount and pre-registration",
			params: BuildParams{
				OutputRef:                 "quay.io/org/image:tag",
				Context:                   tempDir,
				RHSMActivationKey:         "/path/to/key",
				RHSMOrg:                   "/path/to/org",
				RHSMActivationMount:       "/activation-key",
				RHSMActivationPreregister: true,
			},
			errExpected: false,
		},
		{
			name: "should allow rhsm-entitlements",
			params: BuildParams{
				OutputRef:        "quay.io/org/image:tag",
				Context:          tempDir,
				RHSMEntitlements: "/etc/pki/entitlement",
			},
			errExpected: false,
		},
		{
			name: "should allow rhsm-mount-ca-certs=always",
			params: BuildParams{
				OutputRef:        "quay.io/org/image:tag",
				Context:          tempDir,
				RHSMMountCACerts: "always",
			},
			errExpected: false,
		},
		{
			name: "should allow rhsm-mount-ca-certs=auto",
			params: BuildParams{
				OutputRef:        "quay.io/org/image:tag",
				Context:          tempDir,
				RHSMMountCACerts: "auto",
			},
			errExpected: false,
		},
		{
			name: "should allow rhsm-mount-ca-certs=never",
			params: BuildParams{
				OutputRef:        "quay.io/org/image:tag",
				Context:          tempDir,
				RHSMMountCACerts: "never",
			},
			errExpected: false,
		},
		{
			name: "should fail when rhsm-activation-key is provided without rhsm-org",
			params: BuildParams{
				OutputRef:           "quay.io/org/image:tag",
				Context:             tempDir,
				RHSMActivationKey:   "/path/to/key",
				RHSMActivationMount: "/activation-key",
			},
			errExpected:  true,
			errSubstring: "must be used together",
		},
		{
			name: "should fail when rhsm-org is provided without rhsm-activation-key",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Context:   tempDir,
				RHSMOrg:   "/path/to/org",
			},
			errExpected:  true,
			errSubstring: "must be used together",
		},
		{
			name: "should fail when rhsm-entitlements and rhsm-activation-key are used together",
			params: BuildParams{
				OutputRef:           "quay.io/org/image:tag",
				Context:             tempDir,
				RHSMEntitlements:    "/etc/pki/entitlement",
				RHSMActivationKey:   "/path/to/key",
				RHSMOrg:             "/path/to/org",
				RHSMActivationMount: "/activation-key",
			},
			errExpected:  true,
			errSubstring: "are mutually exclusive",
		},
		{
			name: "should fail when rhsm-activation-mount is used without rhsm-activation-key",
			params: BuildParams{
				OutputRef:           "quay.io/org/image:tag",
				Context:             tempDir,
				RHSMActivationMount: "/activation-key",
			},
			errExpected:  true,
			errSubstring: "requires rhsm-activation-key",
		},
		{
			name: "should fail when rhsm-activation-preregister is used without rhsm-activation-key",
			params: BuildParams{
				OutputRef:                 "quay.io/org/image:tag",
				Context:                   tempDir,
				RHSMActivationPreregister: true,
			},
			errExpected:  true,
			errSubstring: "requires rhsm-activation-key",
		},
		{
			name: "should fail when rhsm-activation-mount is a relative path",
			params: BuildParams{
				OutputRef:           "quay.io/org/image:tag",
				Context:             tempDir,
				RHSMActivationKey:   "/path/to/key",
				RHSMOrg:             "/path/to/org",
				RHSMActivationMount: "activation-key",
			},
			errExpected:  true,
			errSubstring: "must be an absolute path",
		},
		{
			name: "should fail when rhsm-mount-ca-certs has invalid value",
			params: BuildParams{
				OutputRef:        "quay.io/org/image:tag",
				Context:          tempDir,
				RHSMMountCACerts: "invalid",
			},
			errExpected:  true,
			errSubstring: "must be one of",
		},
		{
			name: "should fail when rhsm-activation-key is used without mount or preregister",
			params: BuildParams{
				OutputRef:         "quay.io/org/image:tag",
				Context:           tempDir,
				RHSMActivationKey: "/path/to/key",
				RHSMOrg:           "/path/to/org",
			},
			errExpected:  true,
			errSubstring: "requires rhsm-activation-mount or rhsm-activation-preregister",
		},
		{
			name: "should allow source with relative context inside source",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Source:    sourceDir,
				Context:   "ctx",
			},
			errExpected: false,
		},
		{
			name: "should allow source with dot context",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Source:    sourceDir,
				Context:   ".",
			},
			errExpected: false,
		},
		{
			name: "should fail when context escapes source via ..",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Source:    sourceDir,
				Context:   "../outside-ctx",
			},
			errExpected:  true,
			errSubstring: "is outside source directory",
		},
		{
			name: "should fail when absolute context is outside source",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Source:    sourceDir,
				Context:   filepath.Join(tempDir, "outside-ctx"),
			},
			errExpected:  true,
			errSubstring: "is outside source directory",
		},
		{
			name: "should allow source with absolute context inside source",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Source:    sourceDir,
				Context:   filepath.Join(sourceDir, "ctx"),
			},
			errExpected: false,
		},
		{
			name: "should fail when source directory does not exist",
			params: BuildParams{
				OutputRef: "quay.io/org/image:tag",
				Source:    "/no/such/directory",
				Context:   filepath.Join(sourceDir, "ctx"),
			},
			errExpected:  true,
			errSubstring: "no such file or directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Build{Params: &tc.params}

			if tc.setupFunc != nil {
				c.Params.Context = tc.setupFunc()
			}

			err := c.validateParams()

			if tc.errExpected {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(MatchRegexp(tc.errSubstring))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}
		})
	}
}

func Test_Build_detectBuildahVersion(t *testing.T) {
	g := NewWithT(t)

	t.Run("should set buildahVersion and parsedBuildahVersion", func(t *testing.T) {
		mock := &mockBuildahCli{
			VersionFunc: func() (cliwrappers.BuildahVersionInfo, error) {
				return cliwrappers.BuildahVersionInfo{Version: "1.44.0"}, nil
			},
		}
		c := &Build{
			Params:      &BuildParams{},
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
		}

		err := c.detectBuildahVersion()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahVersion.Version).To(Equal("1.44.0"))
		g.Expect(c.parsedBuildahVersion).To(Equal([]int{1, 44, 0}))
	})

	t.Run("should propagate error from Version()", func(t *testing.T) {
		mock := &mockBuildahCli{
			VersionFunc: func() (cliwrappers.BuildahVersionInfo, error) {
				return cliwrappers.BuildahVersionInfo{}, errors.New("command not found")
			},
		}
		c := &Build{
			Params:      &BuildParams{},
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
		}

		err := c.detectBuildahVersion()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("getting buildah version: command not found"))
	})

	t.Run("should propagate error from ParseVersion()", func(t *testing.T) {
		mock := &mockBuildahCli{
			VersionFunc: func() (cliwrappers.BuildahVersionInfo, error) {
				return cliwrappers.BuildahVersionInfo{Version: "invalid"}, nil
			},
		}
		c := &Build{
			Params:      &BuildParams{},
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
		}

		err := c.detectBuildahVersion()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("parsing buildah version:"))
	})
}

func Test_Build_detectContainerfile(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name             string
		files            []string // files to create (paths relative to tempDir)
		containerfileArg string
		contextArg       string
		sourceArg        string
		expectedPath     string
		expectError      bool
		errorContains    string
	}{
		{
			name:         "should auto-detect Containerfile in workdir",
			files:        []string{"Containerfile"},
			expectedPath: "Containerfile",
		},
		{
			name:         "should auto-detect Containerfile in context dir",
			files:        []string{"context/Containerfile"},
			contextArg:   "context",
			expectedPath: "context/Containerfile",
		},
		{
			name:             "should use explicit containerfile",
			files:            []string{"custom.dockerfile"},
			containerfileArg: "custom.dockerfile",
			expectedPath:     "custom.dockerfile",
		},
		{
			name:             "should prepend context directory for explicit containerfile",
			files:            []string{"context/custom.dockerfile"},
			containerfileArg: "custom.dockerfile",
			contextArg:       "context",
			expectedPath:     "context/custom.dockerfile",
		},
		{
			name:             "should fail when explicit containerfile not found",
			containerfileArg: "nonexistent.dockerfile",
			expectError:      true,
			errorContains:    "containerfile does not exist",
		},
		{
			name:          "should fail when no implicit containerfile found",
			expectError:   true,
			errorContains: "containerfile does not exist",
		},
		{
			name:         "should auto-detect Containerfile in source dir",
			files:        []string{"src/Containerfile"},
			sourceArg:    "src",
			expectedPath: "src/Containerfile",
		},
		{
			name:         "should auto-detect Containerfile in source with context subdir",
			files:        []string{"src/ctx/Containerfile"},
			sourceArg:    "src",
			contextArg:   "ctx",
			expectedPath: "src/ctx/Containerfile",
		},
		{
			name:             "should use explicit containerfile within source",
			files:            []string{"src/custom.dockerfile"},
			sourceArg:        "src",
			containerfileArg: "custom.dockerfile",
			expectedPath:     "src/custom.dockerfile",
		},
		{
			name:             "should fail when containerfile is outside source dir",
			files:            []string{"outside/Containerfile", "src/Containerfile"},
			sourceArg:        "src",
			containerfileArg: "../outside/Containerfile",
			expectError:      true,
			errorContains:    "is outside source directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()

			cwd, _ := os.Getwd()
			os.Chdir(tempDir)
			if cwd != "" {
				defer os.Chdir(cwd)
			}

			for _, filePath := range tc.files {
				dir := filepath.Dir(filePath)
				if dir != tempDir {
					os.MkdirAll(dir, 0755)
				}
				os.WriteFile(filePath, []byte("FROM scratch"), 0644)
			}

			if tc.contextArg == "" {
				tc.contextArg = "."
			}
			c := &Build{
				Params: &BuildParams{
					Context:       tc.contextArg,
					Containerfile: tc.containerfileArg,
					Source:        tc.sourceArg,
				},
			}

			err := c.detectContainerfile()

			if tc.expectError {
				g.Expect(err).To(HaveOccurred())
				if tc.errorContains != "" {
					g.Expect(err.Error()).To(ContainSubstring(tc.errorContains))
				}
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(c.containerfilePath).To(Equal(tc.expectedPath))
			}
		})
	}
}

func Test_Build_setSecretArgs(t *testing.T) {
	g := NewWithT(t)

	t.Run("should append nothing when SecretDirs is nil", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				SecretDirs: nil,
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(BeEmpty())
	})

	t.Run("should append nothing when SecretDirs is empty", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(BeEmpty())
	})

	t.Run("should append nothing for empty directory", func(t *testing.T) {
		tempDir := t.TempDir()
		emptyDir := filepath.Join(tempDir, "empty")
		if err := os.Mkdir(emptyDir, 0755); err != nil {
			t.Fatalf("Failed to create empty directory: %s", err)
		}

		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{emptyDir},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(BeEmpty())
	})

	t.Run("should process single file in directory", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secret1/token": "secret-token",
		})

		secretDir := filepath.Join(tempDir, "secret1")
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{secretDir},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(Equal([]cliwrappers.BuildahSecret{
			{Src: filepath.Join(secretDir, "token"), Id: "secret1/token"},
		}))
	})

	t.Run("should process multiple files in directory", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secret1/password": "secret-pass",
			"secret1/token":    "secret-token",
		})

		secretDir := filepath.Join(tempDir, "secret1")
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{secretDir},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(Equal([]cliwrappers.BuildahSecret{
			{Src: filepath.Join(secretDir, "password"), Id: "secret1/password"},
			{Src: filepath.Join(secretDir, "token"), Id: "secret1/token"},
		}))
	})

	t.Run("should process multiple directories", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secret1/token":    "token1",
			"secret2/password": "pass2",
		})

		secret1Dir := filepath.Join(tempDir, "secret1")
		secret2Dir := filepath.Join(tempDir, "secret2")
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{secret1Dir, secret2Dir},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(Equal([]cliwrappers.BuildahSecret{
			{Src: filepath.Join(secret1Dir, "token"), Id: "secret1/token"},
			{Src: filepath.Join(secret2Dir, "password"), Id: "secret2/password"},
		}))
	})

	t.Run("should use custom name from name parameter", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secret1/token": "secret-token",
		})

		secretDir := filepath.Join(tempDir, "secret1")
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{"src=" + secretDir + ",name=custom"},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(Equal([]cliwrappers.BuildahSecret{
			{Src: filepath.Join(secretDir, "token"), Id: "custom/token"},
		}))
	})

	t.Run("should skip subdirectories", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secret1/token":         "secret-token",
			"secret1/subdir/nested": "nested",
		})

		secretDir := filepath.Join(tempDir, "secret1")
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{secretDir},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(Equal([]cliwrappers.BuildahSecret{
			{Src: filepath.Join(secretDir, "token"), Id: "secret1/token"},
		}))
	})

	t.Run("should allow same filename in different directories", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secret1/token": "token1",
			"secret2/token": "token2",
		})

		secret1Dir := filepath.Join(tempDir, "secret1")
		secret2Dir := filepath.Join(tempDir, "secret2")
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{secret1Dir, secret2Dir},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(Equal([]cliwrappers.BuildahSecret{
			{Src: filepath.Join(secret1Dir, "token"), Id: "secret1/token"},
			{Src: filepath.Join(secret2Dir, "token"), Id: "secret2/token"},
		}))
	})

	t.Run("should error on duplicate secret IDs", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secret1/token":       "token1",
			"other/secret1/token": "token2",
		})

		secret1Dir := filepath.Join(tempDir, "secret1")
		otherSecret1Dir := filepath.Join(tempDir, "other", "secret1")
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{secret1Dir, otherSecret1Dir},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("duplicate secret ID 'secret1/token'"))
	})

	t.Run("should error when directory does not exist", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{"/nonexistent/path"},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to read secret directory /nonexistent/path"))
	})

	t.Run("should not error when optional directory does not exist", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{"src=/nonexistent/path,optional=true"},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(BeEmpty())
	})

	t.Run("should error on invalid SecretDirs syntax", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{"src=/path,invalid=value"},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("invalid attribute: invalid"))
	})

	t.Run("should error on invalid optional value", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{"src=/path,optional=maybe"},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("invalid argument: optional=maybe"))
	})

	t.Run("should process symlink to file but skip symlink to directory", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secret1/..data/token": "secret-token",
			// secret1/token -> ..data/token
			// secret1/data -> ..data
		})

		secretDir := filepath.Join(tempDir, "secret1")
		tokenSymlink := filepath.Join(secretDir, "token")
		dataSymlink := filepath.Join(secretDir, "data")

		if err := os.Symlink("..data/token", tokenSymlink); err != nil {
			t.Fatalf("Failed to create symlink to file: %s", err)
		}
		if err := os.Symlink("..data", dataSymlink); err != nil {
			t.Fatalf("Failed to create symlink to directory: %s", err)
		}

		c := &Build{
			Params: &BuildParams{
				SecretDirs: []string{secretDir},
			},
		}

		err := c.setSecretArgs()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.buildahSecrets).To(Equal([]cliwrappers.BuildahSecret{
			{Src: tokenSymlink, Id: "secret1/token"},
		}))
	})
}

func Test_Build_parseContainerfile(t *testing.T) {
	g := NewWithT(t)

	t.Run("should successfully parse valid Containerfile", func(t *testing.T) {
		tempDir := t.TempDir()
		containerfilePath := filepath.Join(tempDir, "Containerfile")
		os.WriteFile(containerfilePath, []byte("FROM scratch\nRUN echo hello"), 0644)

		c := &Build{containerfilePath: containerfilePath, Params: &BuildParams{}}
		result, err := c.parseContainerfile()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result).ToNot(BeNil())
	})

	t.Run("should inject Envs", func(t *testing.T) {
		tempDir := t.TempDir()
		containerfilePath := filepath.Join(tempDir, "Containerfile")
		os.WriteFile(containerfilePath, []byte("FROM scratch\n"), 0644)

		os.Setenv("VAR_FROM_ENV", "from-env")
		defer os.Unsetenv("VAR_FROM_ENV")

		c := &Build{
			containerfilePath: containerfilePath,
			Params: &BuildParams{
				Envs: []string{"FOO=bar", "VAR_FROM_ENV"},
			},
		}

		result, err := c.parseContainerfile()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(result.Stages).To(HaveLen(1))

		expectedEnvs := []instructions.KeyValuePair{
			{Key: "FOO", Value: "bar"},
			{Key: "VAR_FROM_ENV", Value: "from-env"},
		}
		var actualEnvs []instructions.KeyValuePair

		for _, cmd := range result.Stages[0].Commands {
			if env, ok := cmd.Command.(*instructions.EnvCommand); ok {
				actualEnvs = append(actualEnvs, env.Env...)
			} else {
				t.Fatalf("Expected an ENV instruction, got %#v", cmd)
			}
		}

		g.Expect(actualEnvs).To(ConsistOf(expectedEnvs))
	})

	t.Run("should return error for non-existent file", func(t *testing.T) {
		c := &Build{containerfilePath: "/nonexistent/Containerfile", Params: &BuildParams{}}
		result, err := c.parseContainerfile()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(MatchRegexp("failed to parse /nonexistent/Containerfile:.* no such file or directory"))
		g.Expect(result).To(BeNil())
	})

	t.Run("should return error for invalid Containerfile syntax", func(t *testing.T) {
		tempDir := t.TempDir()
		containerfilePath := filepath.Join(tempDir, "Containerfile")
		os.WriteFile(containerfilePath, []byte("INVALID SYNTAX HERE"), 0644)

		c := &Build{containerfilePath: containerfilePath, Params: &BuildParams{}}
		result, err := c.parseContainerfile()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(MatchRegexp("failed to parse .*: unknown instruction: INVALID"))
		g.Expect(result).To(BeNil())
	})
}

func Test_Build_writeContainerfileJson(t *testing.T) {
	g := NewWithT(t)

	t.Run("should successfully write JSON to specified path", func(t *testing.T) {
		tempDir := t.TempDir()
		outputPath := filepath.Join(tempDir, "containerfile.json")

		containerfilePath := filepath.Join(tempDir, "Containerfile")
		os.WriteFile(containerfilePath, []byte("FROM scratch"), 0644)

		c := &Build{containerfilePath: containerfilePath, Params: &BuildParams{}}
		containerfile, err := c.parseContainerfile()
		g.Expect(err).ToNot(HaveOccurred())

		err = c.writeContainerfileJson(containerfile, outputPath)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(outputPath).To(BeAnExistingFile())

		content, err := os.ReadFile(outputPath)
		g.Expect(err).ToNot(HaveOccurred())

		// Full file content tested in integration tests
		g.Expect(string(content)).To(ContainSubstring(`"Stages":`))
	})

	t.Run("should return error when path is not writable", func(t *testing.T) {
		tempDir := t.TempDir()
		containerfilePath := filepath.Join(tempDir, "Containerfile")
		os.WriteFile(containerfilePath, []byte("FROM scratch"), 0644)

		c := &Build{containerfilePath: containerfilePath, Params: &BuildParams{}}
		containerfile, err := c.parseContainerfile()
		g.Expect(err).ToNot(HaveOccurred())

		unwritablePath := "/nonexistent/directory/containerfile.json"
		err = c.writeContainerfileJson(containerfile, unwritablePath)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to write Containerfile JSON"))
	})
}

func Test_Build_createBuildArgExpander(t *testing.T) {
	g := NewWithT(t)

	t.Run("should expand build args from CLI with KEY=value format", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				BuildArgs: []string{"NAME=foo", "VERSION=1.2.3"},
			},
		}

		expander, err := c.createBuildArgExpander()
		g.Expect(err).ToNot(HaveOccurred())

		value, err := expander("NAME")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("foo"))

		value, err = expander("VERSION")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("1.2.3"))
	})

	t.Run("should expand build args from CLI with KEY format (env lookup)", func(t *testing.T) {
		os.Setenv("TEST_ENV_VAR", "from-env")
		defer os.Unsetenv("TEST_ENV_VAR")

		c := &Build{
			Params: &BuildParams{
				BuildArgs: []string{"TEST_ENV_VAR"},
			},
		}

		expander, err := c.createBuildArgExpander()
		g.Expect(err).ToNot(HaveOccurred())

		value, err := expander("TEST_ENV_VAR")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("from-env"))
	})

	t.Run("should expand build args from file", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"build-args": "AUTHOR=John Doe\nVENDOR=konflux-ci.dev\n",
		})

		c := &Build{
			Params: &BuildParams{
				BuildArgsFile: filepath.Join(tempDir, "build-args"),
			},
		}

		expander, err := c.createBuildArgExpander()
		g.Expect(err).ToNot(HaveOccurred())

		value, err := expander("AUTHOR")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("John Doe"))

		value, err = expander("VENDOR")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("konflux-ci.dev"))
	})

	t.Run("should give CLI args precedence over file args", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"build-args": "NAME=file-value\nOTHER=from-file\n",
		})

		c := &Build{
			Params: &BuildParams{
				BuildArgs:     []string{"NAME=cli-value"},
				BuildArgsFile: filepath.Join(tempDir, "build-args"),
			},
		}

		expander, err := c.createBuildArgExpander()
		g.Expect(err).ToNot(HaveOccurred())

		value, err := expander("NAME")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("cli-value"))

		value, err = expander("OTHER")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("from-file"))
	})

	t.Run("should provide built-in platform args by default", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{},
		}

		expander, err := c.createBuildArgExpander()
		g.Expect(err).ToNot(HaveOccurred())

		// Check that all built-in platform args are available
		platformArgs := []string{
			"TARGETPLATFORM", "TARGETOS", "TARGETARCH", "TARGETVARIANT",
			"BUILDPLATFORM", "BUILDOS", "BUILDARCH", "BUILDVARIANT",
		}

		for _, arg := range platformArgs {
			value, err := expander(arg)
			// TARGETVARIANT and BUILDVARIANT can be empty on non-ARM platforms
			if arg == "TARGETVARIANT" || arg == "BUILDVARIANT" {
				g.Expect(err).ToNot(HaveOccurred())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(value).ToNot(BeEmpty(), "arg %s should not be empty", arg)
			}
		}
	})

	t.Run("should allow file args to override built-in platform args", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"build-args": "TARGETOS=custom-os\nTARGETARCH=custom-arch\n",
		})

		c := &Build{
			Params: &BuildParams{
				BuildArgsFile: filepath.Join(tempDir, "build-args"),
			},
		}

		expander, err := c.createBuildArgExpander()
		g.Expect(err).ToNot(HaveOccurred())

		value, err := expander("TARGETOS")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("custom-os"))

		value, err = expander("TARGETARCH")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("custom-arch"))
	})

	t.Run("should allow CLI args to override built-in platform args", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				BuildArgs: []string{"TARGETOS=custom-os", "TARGETARCH=custom-arch"},
			},
		}

		expander, err := c.createBuildArgExpander()
		g.Expect(err).ToNot(HaveOccurred())

		value, err := expander("TARGETOS")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("custom-os"))

		value, err = expander("TARGETARCH")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(value).To(Equal("custom-arch"))
	})

	t.Run("should return error for undefined build arg", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{},
		}

		expander, err := c.createBuildArgExpander()
		g.Expect(err).ToNot(HaveOccurred())

		value, err := expander("UNDEFINED")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("not defined"))
		g.Expect(value).To(BeEmpty())
	})

	t.Run("should error when build args file not found", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				BuildArgsFile: "/nonexistent/build-args",
			},
		}

		expander, err := c.createBuildArgExpander()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to read build args file"))
		g.Expect(expander).To(BeNil())
	})

	t.Run("should error when build args file has invalid format", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"build-args": "INVALID LINE\n",
		})

		c := &Build{
			Params: &BuildParams{
				BuildArgsFile: filepath.Join(tempDir, "build-args"),
			},
		}

		expander, err := c.createBuildArgExpander()

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to read build args file"))
		g.Expect(expander).To(BeNil())
	})
}

func Test_Build_Run(t *testing.T) {
	g := NewWithT(t)

	var _mockBuildahCli *mockBuildahCli
	var _mockResultsWriter *mockResultsWriter
	var c *Build
	var tempDir string

	beforeEach := func() {
		tempDir = t.TempDir()
		contextDir := filepath.Join(tempDir, "context")
		os.Mkdir(contextDir, 0755)
		os.WriteFile(filepath.Join(contextDir, "Containerfile"), []byte("FROM scratch"), 0644)

		_mockBuildahCli = &mockBuildahCli{}
		_mockResultsWriter = &mockResultsWriter{}
		c = &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: _mockBuildahCli},
			Params: &BuildParams{
				OutputRef:      "quay.io/org/image:tag",
				Context:        contextDir,
				Containerfile:  "",
				Push:           true,
				SkipInjections: true,
				// *-tls-verify defaults to true in the CLI
				SrcTLSVerify:  true,
				DestTLSVerify: true,
			},
			ResultsWriter: _mockResultsWriter,
		}
	}

	t.Run("should successfully build and push image", func(t *testing.T) {
		beforeEach()

		isBuildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			isBuildCalled = true
			g.Expect(args.OutputRef).To(Equal("quay.io/org/image:tag"))
			g.Expect(args.ContextDir).To(Equal(c.Params.Context))
			g.Expect(args.Containerfile).To(ContainSubstring("Containerfile"))
			return nil
		}

		isPushCalled := false
		_mockBuildahCli.PushFunc = func(args *cliwrappers.BuildahPushArgs) (string, error) {
			isPushCalled = true
			g.Expect(args.Image).To(Equal("quay.io/org/image:tag"))
			return "sha256:1234567890abcdef", nil
		}

		isCreateResultJsonCalled := false
		_mockResultsWriter.CreateResultJsonFunc = func(result any) (string, error) {
			isCreateResultJsonCalled = true
			buildResults, ok := result.(BuildResults)
			g.Expect(ok).To(BeTrue())
			g.Expect(buildResults.ImageUrl).To(Equal("quay.io/org/image:tag"))
			g.Expect(buildResults.Digest).To(Equal("sha256:1234567890abcdef"))
			return "", nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(isBuildCalled).To(BeTrue())
		g.Expect(isPushCalled).To(BeTrue())
		g.Expect(isCreateResultJsonCalled).To(BeTrue())
	})

	t.Run("should successfully build without pushing", func(t *testing.T) {
		beforeEach()
		c.Params.Push = false

		isBuildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			isBuildCalled = true
			g.Expect(args.OutputRef).To(Equal("quay.io/org/image:tag"))
			return nil
		}

		isPushCalled := false
		_mockBuildahCli.PushFunc = func(args *cliwrappers.BuildahPushArgs) (string, error) {
			isPushCalled = true
			return "", nil
		}

		isCreateResultJsonCalled := false
		_mockResultsWriter.CreateResultJsonFunc = func(result any) (string, error) {
			isCreateResultJsonCalled = true
			buildResults, ok := result.(BuildResults)
			g.Expect(ok).To(BeTrue())
			g.Expect(buildResults.ImageUrl).To(Equal("quay.io/org/image:tag"))
			g.Expect(buildResults.Digest).To(BeEmpty())
			return "", nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(isBuildCalled).To(BeTrue())
		g.Expect(isPushCalled).To(BeFalse())
		g.Expect(isCreateResultJsonCalled).To(BeTrue())
	})

	t.Run("should pass buildahSecrets to buildah build", func(t *testing.T) {
		beforeEach()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"secrets/token": "secret-token",
		})
		secretDir := filepath.Join(tempDir, "secrets")
		c.Params.SecretDirs = []string{secretDir}

		isBuildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			isBuildCalled = true
			g.Expect(args.Secrets).To(Equal([]cliwrappers.BuildahSecret{
				{Src: filepath.Join(secretDir, "token"), Id: "secrets/token"},
			}))
			return nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(isBuildCalled).To(BeTrue())
	})

	t.Run("should clean up temporary workdir on exit", func(t *testing.T) {
		beforeEach()

		testutil.WriteFileTree(t, tempDir, map[string]string{
			"tempWorkdir/file1.txt":             "hello",
			"tempWorkdir/file2.txt":             "hi",
			"tempWorkdir/buildinfo/labels.json": "{}",
		})
		c.tempWorkdir = filepath.Join(tempDir, "tempWorkdir")

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(c.tempWorkdir).ToNot(BeAnExistingFile(), "tempWorkdir should have been deleted")
	})

	t.Run("should error if build fails", func(t *testing.T) {
		beforeEach()

		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			return errors.New("buildah build failed")
		}

		err := c.run()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("buildah build failed"))
	})

	t.Run("should error if push fails", func(t *testing.T) {
		beforeEach()

		_mockBuildahCli.PushFunc = func(args *cliwrappers.BuildahPushArgs) (string, error) {
			return "", errors.New("buildah push failed")
		}

		err := c.run()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("buildah push failed"))
	})

	t.Run("should error if validation fails", func(t *testing.T) {
		beforeEach()
		c.Params.OutputRef = "invalid//image"

		err := c.run()
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("should error if containerfile detection fails", func(t *testing.T) {
		beforeEach()
		// Remove the Containerfile
		os.Remove(filepath.Join(c.Params.Context, "Containerfile"))

		err := c.run()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("containerfile does not exist"))
	})

	t.Run("should error if results json creation fails", func(t *testing.T) {
		beforeEach()

		isCreateResultJsonCalled := false
		_mockResultsWriter.CreateResultJsonFunc = func(result any) (string, error) {
			isCreateResultJsonCalled = true
			return "", errors.New("failed to create results json")
		}

		err := c.run()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to create results json"))
		g.Expect(isCreateResultJsonCalled).To(BeTrue())
	})

	t.Run("should run buildah inside context directory with absolute paths", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"Containerfile":   "FROM scratch",
			"context/main.go": "package main",
			"secrets/token":   "secret-token",
		})

		originalCwd, _ := os.Getwd()
		os.Chdir(tempDir)
		defer os.Chdir(originalCwd)

		_mockBuildahCli := &mockBuildahCli{}
		_mockResultsWriter := &mockResultsWriter{}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: _mockBuildahCli},
			Params: &BuildParams{
				OutputRef:      "quay.io/org/image:tag",
				Containerfile:  "Containerfile",
				Context:        "context",
				SecretDirs:     []string{"secrets"},
				SkipInjections: true,
			},
			ResultsWriter: _mockResultsWriter,
		}

		expectedContextDir := filepath.Join(tempDir, "context")
		expectedContainerfile := filepath.Join(tempDir, "Containerfile")
		expectedSecretSrc := filepath.Join(tempDir, "secrets/token")

		buildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			buildCalled = true

			currentDir, err := os.Getwd()
			g.Expect(err).ToNot(HaveOccurred())

			// Check that the buildah build happens inside the contextDir
			g.Expect(currentDir).To(Equal(expectedContextDir))

			g.Expect(args.Containerfile).To(Equal(expectedContainerfile))
			g.Expect(args.ContextDir).To(Equal(expectedContextDir))
			g.Expect(args.Secrets).To(HaveLen(1))
			g.Expect(args.Secrets[0].Src).To(Equal(expectedSecretSrc))

			return nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(buildCalled).To(BeTrue())

		// Check that the Run() function restored the cwd on exit
		restoredDir, _ := os.Getwd()
		g.Expect(restoredDir).To(Equal(tempDir))
	})

	t.Run("should make contextdir and containerfile paths relative to source dir", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"source/context/foo":   "bar",
			"source/Containerfile": "FROM scratch",
		})

		_mockBuildahCli := &mockBuildahCli{}
		_mockResultsWriter := &mockResultsWriter{}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: _mockBuildahCli},
			Params: &BuildParams{
				OutputRef:      "quay.io/org/image:tag",
				Source:         filepath.Join(tempDir, "source"),
				Context:        "context",
				Containerfile:  "Containerfile",
				WorkdirMount:   "/workdir",
				SkipInjections: true,
			},
			ResultsWriter: _mockResultsWriter,
		}

		expectedContextDir := filepath.Join(tempDir, "source", "context")

		buildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			buildCalled = true

			currentDir, err := os.Getwd()
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(currentDir).To(Equal(expectedContextDir))

			g.Expect(args.ContextDir).To(Equal(expectedContextDir))
			g.Expect(args.Containerfile).To(Equal(filepath.Join(tempDir, "source", "Containerfile")))

			var workdirVolume *cliwrappers.BuildahVolume
			for i := range args.Volumes {
				if args.Volumes[i].ContainerDir == "/workdir" {
					workdirVolume = &args.Volumes[i]
					break
				}
			}
			g.Expect(workdirVolume).ToNot(BeNil())
			g.Expect(workdirVolume.HostDir).To(Equal(expectedContextDir))

			return nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(buildCalled).To(BeTrue())
	})

	// Unit-test the full RHSM pre-registration flow. Ideally this would be a real integration test,
	// but it's not practical to do RHSM registration in tests.
	t.Run("should do RHSM pre-registration", func(t *testing.T) {
		beforeEach()

		c.hostEntitlements = t.TempDir()
		c.hostConsumerCerts = t.TempDir()
		c.hostRHSMcaCerts = t.TempDir()

		caCertPath := filepath.Join(c.hostRHSMcaCerts, "redhat-uep.pem")
		g.Expect(os.WriteFile(caCertPath, []byte("RHSM CA cert"), 0644)).To(Succeed())

		activationDir := t.TempDir()
		testutil.WriteFileTree(t, activationDir, map[string]string{
			"key.txt": "my-activation-key\n",
			"org.txt": "my-org\n",
		})

		c.Params.RHSMActivationKey = filepath.Join(activationDir, "key.txt")
		c.Params.RHSMOrg = filepath.Join(activationDir, "org.txt")
		c.Params.RHSMActivationPreregister = true
		c.Params.RHSMActivationMount = "/activation-key"

		var capturedRegisterParams *cliwrappers.SubscriptionManagerRegisterParams
		unregisterCalled := false

		c.CliWrappers.SubscriptionManager = &mockSubscriptionManagerCli{
			RegisterFunc: func(params *cliwrappers.SubscriptionManagerRegisterParams) error {
				capturedRegisterParams = params
				// Simulate subscription-manager creating cert files on the host
				testutil.WriteFileTree(t, c.hostEntitlements, map[string]string{
					"12345.pem":     "entitlement-cert",
					"12345-key.pem": "entitlement-key",
				})
				testutil.WriteFileTree(t, c.hostConsumerCerts, map[string]string{
					"cert.pem": "consumer-cert",
				})
				return nil
			},
			UnregisterFunc: func() {
				unregisterCalled = true
			},
		}

		buildCalled := false

		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			buildCalled = true

			expectedMounts := map[string][]string{
				// Should mount the entitlement and consumer certs created by subscription-manager
				"/etc/pki/entitlement": {"12345.pem", "12345-key.pem"},
				"/etc/pki/consumer":    {"cert.pem"},
				// By default, should mount RHSM CA cert from host for when preregister=true
				"/etc/rhsm/ca": {"redhat-uep.pem"},
				// Should mount the activation key even when preregister=true, if requested
				"/activation-key": {"activationkey", "org"},
			}
			for destDir, files := range expectedMounts {
				found := false
				for _, v := range args.Volumes {
					if v.ContainerDir == destDir {
						g.Expect(listDir(v.HostDir)).To(ConsistOf(files))
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue(), "no volume with destination="+destDir+" found!")
			}

			return nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(buildCalled).To(BeTrue())

		g.Expect(capturedRegisterParams).ToNot(BeNil())
		g.Expect(capturedRegisterParams.ActivationKey).To(Equal("my-activation-key"))
		g.Expect(capturedRegisterParams.Org).To(Equal("my-org"))
		g.Expect(capturedRegisterParams.Force).To(BeTrue())

		g.Expect(unregisterCalled).To(BeTrue())
	})

	t.Run("should pass --src-tls-verify to buildah build and pull", func(t *testing.T) {
		beforeEach()
		c.Params.SrcTLSVerify = false
		os.WriteFile(filepath.Join(c.Params.Context, "Containerfile"), []byte("FROM registry.example.com/base:latest"), 0644)

		pullCalled := false
		_mockBuildahCli.PullFunc = func(args *cliwrappers.BuildahPullArgs) error {
			pullCalled = true
			g.Expect(args.TLSVerify).ToNot(BeNil())
			g.Expect(*args.TLSVerify).To(BeFalse())
			return nil
		}

		buildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			buildCalled = true
			g.Expect(args.TLSVerify).ToNot(BeNil())
			g.Expect(*args.TLSVerify).To(BeFalse())
			return nil
		}

		pushCalled := false
		_mockBuildahCli.PushFunc = func(args *cliwrappers.BuildahPushArgs) (string, error) {
			pushCalled = true
			g.Expect(args.TLSVerify).ToNot(BeNil())
			g.Expect(*args.TLSVerify).To(BeTrue())
			return "sha256:abc", nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(pullCalled).To(BeTrue())
		g.Expect(buildCalled).To(BeTrue())
		g.Expect(pushCalled).To(BeTrue())
	})

	t.Run("should pass --dest-tls-verify to buildah push", func(t *testing.T) {
		beforeEach()
		c.Params.DestTLSVerify = false
		os.WriteFile(filepath.Join(c.Params.Context, "Containerfile"), []byte("FROM registry.example.com/base:latest"), 0644)

		pullCalled := false
		_mockBuildahCli.PullFunc = func(args *cliwrappers.BuildahPullArgs) error {
			pullCalled = true
			g.Expect(args.TLSVerify).ToNot(BeNil())
			g.Expect(*args.TLSVerify).To(BeTrue())
			return nil
		}

		buildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			buildCalled = true
			g.Expect(args.TLSVerify).ToNot(BeNil())
			g.Expect(*args.TLSVerify).To(BeTrue())
			return nil
		}

		pushCalled := false
		_mockBuildahCli.PushFunc = func(args *cliwrappers.BuildahPushArgs) (string, error) {
			pushCalled = true
			g.Expect(args.TLSVerify).ToNot(BeNil())
			g.Expect(*args.TLSVerify).To(BeFalse())
			return "sha256:abc", nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(pullCalled).To(BeTrue())
		g.Expect(buildCalled).To(BeTrue())
		g.Expect(pushCalled).To(BeTrue())
	})

	t.Run("should pass --no-cache to buildah build", func(t *testing.T) {
		beforeEach()
		c.Params.NoCache = true

		buildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			buildCalled = true
			g.Expect(args.NoCache).To(BeTrue())
			return nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(buildCalled).To(BeTrue())
	})

	t.Run("should pass security-related array args to buildah", func(t *testing.T) {
		beforeEach()
		c.Params.SecurityOpts = []string{"seccomp=unconfined"}
		c.Params.CapAdd = []string{"SYS_ADMIN"}
		c.Params.CapDrop = []string{"NET_RAW"}
		c.Params.Devices = []string{"/dev/fuse"}

		buildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			buildCalled = true
			g.Expect(args.SecurityOpts).To(Equal([]string{"seccomp=unconfined"}))
			g.Expect(args.CapAdd).To(Equal([]string{"SYS_ADMIN"}))
			g.Expect(args.CapDrop).To(Equal([]string{"NET_RAW"}))
			g.Expect(args.Devices).To(Equal([]string{"/dev/fuse"}))
			return nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(buildCalled).To(BeTrue())
	})

	t.Run("should pass ulimits to buildah", func(t *testing.T) {
		beforeEach()
		c.Params.Ulimits = []string{"nofile=4096:4096", "nproc=1024:2048"}

		buildCalled := false
		_mockBuildahCli.BuildFunc = func(args *cliwrappers.BuildahBuildArgs) error {
			buildCalled = true
			g.Expect(args.Ulimits).To(Equal([]string{"nofile=4096:4096", "nproc=1024:2048"}))
			return nil
		}

		err := c.run()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(buildCalled).To(BeTrue())
	})
}

func Test_goArchToArchitectureLabel(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		goarch   string
		expected string
	}{
		{"amd64", "x86_64"},
		{"arm64", "aarch64"},
		{"ppc64le", "ppc64le"},
		{"s390x", "s390x"},
		{"unknown", "unknown"},
	}

	for _, tc := range tests {
		result := goArchToRpmArch(tc.goarch)
		g.Expect(result).To(Equal(tc.expected), "goArchToUname(%s) should return %s", tc.goarch, tc.expected)
	}
}

func Test_Build_processLabelsAndAnnotations(t *testing.T) {
	g := NewWithT(t)

	t.Run("should add default labels and annotations with provided values", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				LegacyBuildTimestamp: "1767225600", // 2026-01-01
				ImageSource:          "https://github.com/org/repo",
				ImageRevision:        "abc123",
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(c.mergedLabels).To(Equal([]string{
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"org.opencontainers.image.source=https://github.com/org/repo",
			"org.opencontainers.image.revision=abc123",
		}))
		g.Expect(c.mergedAnnotations).To(Equal([]string{
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"org.opencontainers.image.source=https://github.com/org/repo",
			"org.opencontainers.image.revision=abc123",
		}))
	})

	t.Run("should always add creation time label and annotation", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(c.mergedLabels).To(ConsistOf(
			MatchRegexp(`^org.opencontainers.image.created=.+Z$`),
		))
		g.Expect(c.mergedAnnotations).To(Equal(c.mergedLabels))

		imageCreated := c.mergedLabels[0]

		_, rfc3339time, _ := strings.Cut(imageCreated, "=")
		timestamp, err := time.Parse(time.RFC3339, rfc3339time)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(timestamp).To(BeTemporally("~", time.Now(), time.Second))
	})

	t.Run("should prepend defaults to let user-provided values override them", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				LegacyBuildTimestamp: "1767225600", // 2026-01-01
				ImageSource:          "https://github.com/org/repo",
				ImageRevision:        "abc123",
				Labels: []string{
					"some-label=foo",
					"org.opencontainers.image.revision=main",
				},
				Annotations: []string{
					"some-annotation=bar",
					"org.opencontainers.image.source=https://github.com/other-org/other-repo",
					"org.opencontainers.image.created=1990-01-01T00:00:00Z",
				},
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(c.mergedLabels).To(Equal([]string{
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"org.opencontainers.image.source=https://github.com/org/repo",
			"org.opencontainers.image.revision=abc123",
			"some-label=foo",
			"org.opencontainers.image.revision=main",
		}))
		g.Expect(c.mergedAnnotations).To(Equal([]string{
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"org.opencontainers.image.source=https://github.com/org/repo",
			"org.opencontainers.image.revision=abc123",
			"some-annotation=bar",
			"org.opencontainers.image.source=https://github.com/other-org/other-repo",
			"org.opencontainers.image.created=1990-01-01T00:00:00Z",
		}))
	})

	t.Run("should add legacy labels when requested", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				LegacyBuildTimestamp: "1767225600", // 2026-01-01
				ImageSource:          "https://github.com/org/repo",
				ImageRevision:        "abc123",
				AddLegacyLabels:      true,
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).ToNot(HaveOccurred())

		arch := goArchToRpmArch(runtime.GOARCH)
		g.Expect(c.mergedLabels).To(Equal([]string{
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"org.opencontainers.image.source=https://github.com/org/repo",
			"org.opencontainers.image.revision=abc123",
			"build-date=2026-01-01T00:00:00Z",
			"architecture=" + arch,
			"vcs-url=https://github.com/org/repo",
			"vcs-ref=abc123",
			"vcs-type=git",
		}))
		// Should be added *only* as labels, not as annotations
		g.Expect(c.mergedAnnotations).To(Equal([]string{
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"org.opencontainers.image.source=https://github.com/org/repo",
			"org.opencontainers.image.revision=abc123",
		}))
	})

	t.Run("should use source-date-epoch value for timestamps", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				LegacyBuildTimestamp: "1767225600", // 2026-01-01
				AddLegacyLabels:      true,
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(c.mergedLabels).To(ContainElements(
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"build-date=2026-01-01T00:00:00Z",
		))
		g.Expect(c.mergedAnnotations).To(ContainElements(
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
		))
	})

	t.Run("should add quay.expires-after label when provided", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				QuayImageExpiresAfter: "2w",
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(c.mergedLabels).To(ContainElement("quay.expires-after=2w"))
		g.Expect(c.mergedAnnotations).ToNot(ContainElement("quay.expires-after=2w"))
	})

	t.Run("should return error for invalid legacy-build-timestamp", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				LegacyBuildTimestamp: "1767225600.5",
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("determining build timestamp: parsing legacy-build-timestamp:"))
	})

	t.Run("should return error for invalid source-date-epoch", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				SourceDateEpoch: "1767225600.5",
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("determining build timestamp: parsing source-date-epoch:"))
	})

	t.Run("should parse annotations from file", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"annotations.cfg": `
# comment, ignored
   # also a comment
annotation.from.file=annotation-from-file

with.hash.char=this comment # is not a comment

			leading.spaces=are-removed
			`,
		})

		c := &Build{
			Params: &BuildParams{
				SourceDateEpoch: "1767225600",
				AnnotationsFile: filepath.Join(tempDir, "annotations.cfg"),
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(c.mergedAnnotations).To(Equal([]string{
			// always added
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			// from file, sorted alphabetically
			"annotation.from.file=annotation-from-file",
			"leading.spaces=are-removed",
			"with.hash.char=this comment # is not a comment",
		}))
	})

	t.Run("should return error for nonexistent annotations file", func(t *testing.T) {
		c := &Build{
			Params: &BuildParams{
				AnnotationsFile: "/nonexistent/annotations.cfg",
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(MatchRegexp("parsing annotations file: .* /nonexistent/annotations.cfg"))
	})

	t.Run("should return error for invalid annotations file", func(t *testing.T) {
		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"annotations.cfg": "this line has no equals sign\n",
		})

		c := &Build{
			Params: &BuildParams{
				AnnotationsFile: filepath.Join(tempDir, "annotations.cfg"),
			},
		}

		err := c.processLabelsAndAnnotations()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(MatchRegexp("parsing annotations file: .*annotations.cfg:1: expected arg=value"))
	})
}

func Test_Build_splitTransport(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		input             string
		expectedTransport string
		expectedImageRef  string
	}{
		// No ref
		{"", "", ""},
		// Plain image refs (no transport)
		{"registry.io/image:tag", "", "registry.io/image:tag"},
		{"ubuntu:latest", "", "ubuntu:latest"},
		// Unknown transport (treated the same as no transport, no way to know this isn't a valid image:tag)
		{"made-up-transport:ubuntu", "", "made-up-transport:ubuntu"},
		// Known transports
		{"docker://registry.io/image:tag", "docker://", "registry.io/image:tag"},
		{"containers-storage:localhost/image:tag", "containers-storage:", "localhost/image:tag"},
		{"dir:/path/to/dir", "dir:", "/path/to/dir"},
		{"docker-archive:/path/to/archive.tar", "docker-archive:", "/path/to/archive.tar"},
		{"docker-daemon:image:tag", "docker-daemon:", "image:tag"},
		{"oci:/path/to/dir", "oci:", "/path/to/dir"},
		{"oci-archive:/path/to/archive.tar", "oci-archive:", "/path/to/archive.tar"},
		{"sif:/path/to/file.sif", "sif:", "/path/to/file.sif"},
	}

	for _, tc := range tests {
		testCase := fmt.Sprintf("splitTransport(%q)", tc.input)
		transport, imageRef := splitTransport(tc.input)

		g.Expect(transport).To(Equal(tc.expectedTransport), testCase)
		g.Expect(imageRef).To(Equal(tc.expectedImageRef), testCase)
	}
}

func Test_Build_isPullableImage(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		input          string
		expectedResult bool
	}{
		// No ref
		{"", false},
		// Plain image refs (no transport)
		{"registry.io/image:tag", true},
		{"ubuntu:latest", true},
		// Unknown transport (treated the same as no transport, no way to know this isn't a valid image:tag)
		{"made-up-transport:ubuntu", true},
		// Supported transports
		{"docker://registry.io/image:tag", true},
		{"containers-storage:localhost/image:tag", true},
		// Unsupported transports
		{"dir:/path/to/dir", false},
		{"docker-archive:/path/to/archive.tar", false},
		{"docker-daemon:image:tag", false},
		{"oci:/path/to/dir", false},
		{"oci-archive:/path/to/archive.tar", false},
		{"sif:/path/to/file.sif", false},
	}

	for _, tc := range tests {
		result := isPullableImage(tc.input)
		g.Expect(result).To(Equal(tc.expectedResult), fmt.Sprintf("shouldInspectImage(%q)", tc.input))
	}
}

func Test_Build_injectBuildinfo(t *testing.T) {
	g := NewWithT(t)

	tempDir := t.TempDir()
	containerfile := filepath.Join(tempDir, "Containerfile")
	g.Expect(os.WriteFile(containerfile, []byte("FROM scratch"), 0644)).To(Succeed())

	c := &Build{
		Params: &BuildParams{
			// Avoids the BuildahCli.Version() call
			SourceDateEpoch: "0",
		},
		containerfilePath: containerfile,
	}
	defer c.cleanup()

	var df *dockerfile.Dockerfile = nil
	var userLabels []string = nil

	g.Expect(c.injectBuildinfo(df, userLabels, nil)).To(Succeed())

	// Containerfile is copied to tempWorkdir
	g.Expect(c.containerfileCopyPath).To(HavePrefix(c.tempWorkdir + "/"))
	g.Expect(filepath.Base(c.containerfileCopyPath)).To(MatchRegexp(`^Containerfile-`))
	// Original is unchanged
	originalContent, err := os.ReadFile(c.containerfilePath)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(originalContent)).To(Equal("FROM scratch"))

	// COPY appended correctly even though containerfile lacks trailing newline
	copyContent, err := os.ReadFile(c.containerfileCopyPath)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(copyContent)).To(Equal(
		"FROM scratch\nCOPY --from=.konflux-buildinfo . /usr/share/buildinfo/\n",
	))

	// labels.json created in tempWorkdir/buildinfo with valid JSON
	labelsContent, err := os.ReadFile(filepath.Join(c.tempWorkdir, "buildinfo", "labels.json"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(labelsContent)).To(Equal("{}\n"))

	// buildinfoBuildContext points to the buildinfo dir
	g.Expect(c.buildinfoBuildContext).NotTo(BeNil())
	g.Expect(c.buildinfoBuildContext.Name).To(Equal(".konflux-buildinfo"))
	g.Expect(c.buildinfoBuildContext.Location).To(Equal(filepath.Join(c.tempWorkdir, "buildinfo")))
}

func Test_findMatchingStages(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name          string
		dockerfile    string
		ref           string
		expectIndexes []int
		expectOk      bool
	}{
		{
			name: "match stage by name",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"FROM scratch",
			}, "\n"),
			ref:           "builder",
			expectIndexes: []int{0},
			expectOk:      true,
		},
		{
			name: "match stage by numeric index",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"FROM scratch",
			}, "\n"),
			ref:           "1",
			expectIndexes: []int{1},
			expectOk:      true,
		},
		{
			name:          "no match for unknown name",
			dockerfile:    "FROM golang:1.21 AS builder\n",
			ref:           "nonexistent",
			expectIndexes: nil,
			expectOk:      false,
		},
		{
			name:          "no match for negative index",
			dockerfile:    "FROM golang:1.21\n",
			ref:           "-1",
			expectIndexes: nil,
			expectOk:      false,
		},
		{
			name: "no match for index out of range",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"FROM scratch",
			}, "\n"),
			ref:           "2",
			expectIndexes: nil,
			expectOk:      false,
		},
		{
			name: "duplicate stage names return multiple indexes",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"RUN echo first",
				"",
				"FROM alpine:3.18 AS builder",
				"RUN echo second",
				"",
				"FROM scratch",
			}, "\n"),
			ref:           "builder",
			expectIndexes: []int{0, 1},
			expectOk:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			df := parseDockerfile(t, g, tt.dockerfile)
			indexes, ok := findMatchingStages(df.Stages, tt.ref)
			g.Expect(ok).To(Equal(tt.expectOk), "expected ok=%v", tt.expectOk)
			g.Expect(indexes).To(Equal(tt.expectIndexes), "expected indexes=%v", tt.expectIndexes)
		})
	}
}

func Test_Build_collectBaseImages(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name                 string
		dockerfile           string
		targetStage          int
		dontSkipUnusedStages bool
		expected             []string
	}{
		{
			name: "FROM scratch returns empty",
			dockerfile: strings.Join([]string{
				"FROM scratch",
				"LABEL foo=bar",
			}, "\n"),
			targetStage: 0,
			expected:    []string{},
		},
		{
			name: "single FROM image",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21",
				"RUN echo hello",
			}, "\n"),
			targetStage: 0,
			expected:    []string{"golang:1.21"},
		},
		{
			name: "COPY --from=stageName",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"RUN echo build",
				"",
				"FROM registry.access.redhat.com/ubi9/ubi-minimal:latest",
				"COPY --from=builder /app /app",
			}, "\n"),
			targetStage: 1,
			expected: []string{
				"golang:1.21",
				"registry.access.redhat.com/ubi9/ubi-minimal:latest",
			},
		},
		{
			name: "COPY --from=stageIndex",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21",
				"RUN echo build",
				"",
				"FROM registry.access.redhat.com/ubi9/ubi-minimal:latest",
				"COPY --from=0 /app /app",
			}, "\n"),
			targetStage: 1,
			expected: []string{
				"golang:1.21",
				"registry.access.redhat.com/ubi9/ubi-minimal:latest",
			},
		},
		{
			name: "COPY --from=externalImage",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21",
				"COPY --from=busybox:latest /bin/sh /bin/sh",
			}, "\n"),
			targetStage: 0,
			expected:    []string{"busybox:latest", "golang:1.21"},
		},
		{
			name: "RUN --mount=from=stageName",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"RUN echo build",
				"",
				"FROM alpine:3.18",
				"RUN --mount=type=bind,from=builder,source=/app,target=/app echo hello",
			}, "\n"),
			targetStage: 1,
			expected: []string{
				"alpine:3.18",
				"golang:1.21",
			},
		},
		{
			name: "RUN --mount=from=stageIndex",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"RUN echo build",
				"",
				"FROM alpine:3.18",
				"RUN --mount=type=bind,from=0,src=/app,dst=/app echo hello",
			}, "\n"),
			targetStage: 1,
			expected: []string{
				"alpine:3.18",
				"golang:1.21",
			},
		},
		{
			name: "RUN --mount=from=externalImage",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21",
				"RUN --mount=type=cache,from=registry.example.com/cache:latest,target=/cache echo cached",
			}, "\n"),
			targetStage: 0,
			expected: []string{
				"golang:1.21",
				"registry.example.com/cache:latest",
			},
		},
		{
			name: "diamond dependency deduplicates shared base",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS shared-base",
				"RUN echo base",
				"",
				"FROM alpine:3.18 AS builder-a",
				"RUN --mount=from=shared-base,src=/app,dst=/app echo a",
				"",
				"FROM rust:1.70 AS builder-b",
				"RUN --mount=from=shared-base,src=/app,dst=/app echo b",
				"",
				"FROM scratch",
				"COPY --from=builder-a /a /a",
				"COPY --from=builder-b /b /b",
			}, "\n"),
			targetStage: 3,
			expected: []string{
				"alpine:3.18",
				"golang:1.21",
				"rust:1.70",
			},
		},
		{
			name: "COPY --from= reference to later stage treated as image",
			dockerfile: strings.Join([]string{
				"FROM alpine:3.18 AS builder",
				// The "AS later" stage doesn't exist yet, treat 'later' as an image
				"COPY --from=later /x /x",
				"",
				"FROM builder AS later",
				"RUN echo hi",
			}, "\n"),
			targetStage: 1,
			expected:    []string{"alpine:3.18", "later"},
		},
		{
			name: "FROM reference to later stage treated as image",
			dockerfile: strings.Join([]string{
				// The "AS later" stage doesn't exist yet, treat 'later' as an image
				"FROM later",
				"RUN echo hi",
				"",
				"FROM golang:1.21 AS later",
				// This 'later' refers to stage 0, not the current stage
				"COPY --from=later /app /app",
			}, "\n"),
			targetStage: 1,
			expected:    []string{"golang:1.21", "later"},
		},
		{
			name: "target stage is not the last stage",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"RUN echo build",
				"",
				"FROM alpine:3.18",
				"COPY --from=builder /app /app",
				"",
				"FROM ubuntu:22.04",
				"RUN echo other",
			}, "\n"),
			targetStage: 1,
			expected: []string{
				"alpine:3.18",
				"golang:1.21",
			},
		},
		{
			name: "duplicate stage names: all matching stages are included",
			dockerfile: strings.Join([]string{
				"FROM imageA AS builder",
				"RUN echo first",
				"",
				"FROM imageB AS builder",
				"RUN echo second",
				"",
				"FROM scratch",
				"COPY --from=builder /app /app",
			}, "\n"),
			targetStage: 2,
			expected:    []string{"imageA", "imageB"},
		},
		{
			name: "unused stage not reachable from target is excluded",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS builder",
				"RUN echo build",
				"",
				"FROM rust:1.70 AS unused",
				"RUN echo unused",
				"",
				"FROM alpine:3.18",
				"COPY --from=builder /app /app",
			}, "\n"),
			targetStage: 2,
			expected: []string{
				"alpine:3.18",
				"golang:1.21",
			},
		},
		{
			name: "when SkipUnusedStages=false, includes all stages up to target",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS unused-1",
				"RUN --mount=type=cache,from=registry.example.com/cache:latest,target=/cache echo cached",
				"",
				"FROM rust:1.70 AS unused-2",
				"COPY --from=busybox:latest /bin/sh /bin/sh",
				"",
				"FROM alpine:3.18",
			}, "\n"),
			targetStage:          2,
			dontSkipUnusedStages: true,
			expected: []string{
				"alpine:3.18",
				"busybox:latest",
				"golang:1.21",
				"registry.example.com/cache:latest",
				"rust:1.70",
			},
		},
		{
			name: "when SkipUnusedStages=false and targetStage is not last, excludes later stages",
			dockerfile: strings.Join([]string{
				"FROM golang:1.21 AS unused-1",
				"",
				"FROM rust:1.70 AS target",
				"",
				"FROM alpine:3.18",
			}, "\n"),
			targetStage:          1,
			dontSkipUnusedStages: true,
			expected: []string{
				"golang:1.21",
				"rust:1.70",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			df := parseDockerfile(t, g, tt.dockerfile)

			c := &Build{Params: &BuildParams{SkipUnusedStages: !tt.dontSkipUnusedStages}}
			result := c.collectBaseImages(df, tt.targetStage)
			if len(tt.expected) == 0 {
				g.Expect(result).To(BeEmpty())
			} else {
				g.Expect(result).To(Equal(tt.expected))
			}
		})
	}
}

func Test_Build_collectBaseImages_multipleTargetStages(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name                 string
		dockerfile           string
		targetStages         []int
		dontSkipUnusedStages bool
		expected             []string
	}{
		{
			name: "collects images for all target stages",
			dockerfile: strings.Join([]string{
				"FROM imageA AS builder",
				"RUN echo first",
				"",
				"FROM imageB AS builder",
				"RUN echo second",
			}, "\n"),
			targetStages: []int{0, 1},
			expected:     []string{"imageA", "imageB"},
		},
		{
			name: "follows dependency trees of both target stages",
			dockerfile: strings.Join([]string{
				"FROM imageA AS dep-a",
				"RUN echo dep-a",
				"",
				"FROM imageB AS dep-b",
				"RUN echo dep-b",
				"",
				"FROM imageC AS builder",
				"COPY --from=dep-a /a /a",
				"",
				"FROM imageD AS builder",
				"COPY --from=dep-b /b /b",
			}, "\n"),
			targetStages: []int{2, 3},
			expected:     []string{"imageA", "imageB", "imageC", "imageD"},
		},
		{
			name: "with SkipUnusedStages=false, includes stages between non-contiguous targets",
			dockerfile: strings.Join([]string{
				"FROM imageA AS builder",
				"RUN echo first",
				"",
				"FROM imageB AS middle",
				"RUN echo middle",
				"",
				"FROM imageC AS builder",
				"RUN echo second",
			}, "\n"),
			targetStages:         []int{0, 2},
			dontSkipUnusedStages: true,
			expected:             []string{"imageA", "imageB", "imageC"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			df := parseDockerfile(t, g, tt.dockerfile)

			c := &Build{Params: &BuildParams{SkipUnusedStages: !tt.dontSkipUnusedStages}}
			result := c.collectBaseImages(df, tt.targetStages...)
			g.Expect(result).To(Equal(tt.expected))
		})
	}
}

func Test_Build_prePullBaseImages(t *testing.T) {
	g := NewWithT(t)

	containerfile := strings.Join([]string{
		"FROM imageA AS builder",
		"RUN echo first",
		"",
		"FROM imageB AS builder",
		"RUN echo second",
		"",
		"FROM scratch",
	}, "\n")

	tests := []struct {
		name                 string
		parsedBuildahVersion []int
		expectedPulledImages []string
	}{
		{
			name:                 "buildah 1.44.0 should pull base images for all matching stages",
			parsedBuildahVersion: []int{1, 44, 0},
			expectedPulledImages: []string{"imageA", "imageB"},
		},
		{
			name:                 "buildah 2.0.0 should pull base images for all matching stages",
			parsedBuildahVersion: []int{2, 0, 0},
			expectedPulledImages: []string{"imageA", "imageB"},
		},
		{
			name:                 "buildah < 1.44.0 should pull only for first matching stage",
			parsedBuildahVersion: []int{1, 43, 1},
			expectedPulledImages: []string{"imageA"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			df := parseDockerfile(t, g, containerfile)

			var pulledImages []string
			mock := &mockBuildahCli{
				PullFunc: func(args *cliwrappers.BuildahPullArgs) error {
					pulledImages = append(pulledImages, args.Image)
					return nil
				},
			}

			c := &Build{
				Params: &BuildParams{
					Target:           "builder",
					SkipUnusedStages: true,
				},
				CliWrappers:          BuildCliWrappers{BuildahCli: mock},
				parsedBuildahVersion: tc.parsedBuildahVersion,
			}

			result, err := c.prePullBaseImages(df)

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(result).To(Equal(tc.expectedPulledImages))
			g.Expect(pulledImages).To(Equal(tc.expectedPulledImages))
		})
	}
}

func Test_Build_pullImage(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name                 string
		parsedBuildahVersion []int
		expectedExtraEnv     []string
	}{
		{
			name:                 "buildah 1.44.0 sets _CONTAINERS_USERNS_CONFIGURED",
			parsedBuildahVersion: []int{1, 44, 0},
			expectedExtraEnv:     []string{"_CONTAINERS_USERNS_CONFIGURED=done"},
		},
		{
			name:                 "buildah 1.43.1 does not set _CONTAINERS_USERNS_CONFIGURED",
			parsedBuildahVersion: []int{1, 43, 1},
			expectedExtraEnv:     nil,
		},
		{
			name:                 "buildah 1.44.1 does not set _CONTAINERS_USERNS_CONFIGURED",
			parsedBuildahVersion: []int{1, 44, 1},
			expectedExtraEnv:     nil,
		},
		{
			name:                 "buildah 2.0.0 does not set _CONTAINERS_USERNS_CONFIGURED",
			parsedBuildahVersion: []int{2, 0, 0},
			expectedExtraEnv:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var capturedArgs *cliwrappers.BuildahPullArgs
			mock := &mockBuildahCli{
				PullFunc: func(args *cliwrappers.BuildahPullArgs) error {
					capturedArgs = args
					return nil
				},
			}

			c := &Build{
				Params:               &BuildParams{},
				CliWrappers:          BuildCliWrappers{BuildahCli: mock},
				parsedBuildahVersion: tc.parsedBuildahVersion,
			}

			err := c.pullImage("registry.example.com/image:latest")

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(capturedArgs).ToNot(BeNil())
			g.Expect(capturedArgs.ExtraEnv).To(Equal(tc.expectedExtraEnv))
		})
	}
}

func Test_Build_resolveBaseImages(t *testing.T) {
	g := NewWithT(t)

	const (
		digestA = "sha256:586ab46b9d6d906b2df3dad12751e807bd0f0632d5a2ab3991bdac78bdccd59a"
		digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	t.Run("should return empty for empty input", func(t *testing.T) {
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: &mockBuildahCli{}},
			Params:      &BuildParams{},
		}

		resolved, err := c.resolveBaseImages(nil)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resolved).To(BeEmpty())
	})

	t.Run("should short-circuit for already canonical ref", func(t *testing.T) {
		imagesJsonCalled := false
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				imagesJsonCalled = true
				return nil, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		input := "registry.io/namespace/image@" + digestA
		resolved, err := c.resolveBaseImages([]string{input})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resolved).To(Equal([]string{input}))
		g.Expect(imagesJsonCalled).To(BeFalse())
	})

	t.Run("should resolve short name with tag", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return []cliwrappers.BuildahImagesEntry{
					{Names: []string{"registry.io/namespace/image:tag"}, Digest: digestA},
				}, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		resolved, err := c.resolveBaseImages([]string{"namespace/image:tag"})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resolved).To(Equal([]string{"registry.io/namespace/image:tag@" + digestA}))
	})

	t.Run("should resolve short name without tag", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return []cliwrappers.BuildahImagesEntry{
					{Names: []string{"registry.io/namespace/image:tag"}, Digest: digestA},
				}, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		resolved, err := c.resolveBaseImages([]string{"namespace/image"})

		g.Expect(err).ToNot(HaveOccurred())
		// No tag in output even though buildah Names has one
		g.Expect(resolved).To(Equal([]string{"registry.io/namespace/image@" + digestA}))
	})

	t.Run("should preserve tag from input not from buildah Names", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return []cliwrappers.BuildahImagesEntry{
					{Names: []string{"registry.io/namespace/image:different-tag"}, Digest: digestA},
				}, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		resolved, err := c.resolveBaseImages([]string{"namespace/image:my-tag"})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resolved).To(Equal([]string{"registry.io/namespace/image:my-tag@" + digestA}))
	})

	t.Run("should use digest from input when present", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return []cliwrappers.BuildahImagesEntry{
					{Names: []string{"registry.io/namespace/image:tag"}, Digest: digestB},
				}, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		// Input has digestA, buildah returns digestB — input wins.
		// The only realistic situation when this can occur is if input has the manifest list digest
		// and buildah returns the manifest digest or vice versa.
		resolved, err := c.resolveBaseImages([]string{"namespace/image@" + digestA})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resolved).To(Equal([]string{"registry.io/namespace/image@" + digestA}))
	})

	t.Run("should use digest from buildah when input has no digest", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return []cliwrappers.BuildahImagesEntry{
					{Names: []string{"registry.io/namespace/image:tag"}, Digest: digestA},
				}, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		resolved, err := c.resolveBaseImages([]string{"namespace/image:tag"})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resolved).To(Equal([]string{"registry.io/namespace/image:tag@" + digestA}))
	})

	t.Run("should handle tag+digest with non-normalized name", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return []cliwrappers.BuildahImagesEntry{
					{Names: []string{"registry.io/namespace/image:tag"}, Digest: digestB},
				}, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		// Non-normalized name with tag and digest — both from input
		resolved, err := c.resolveBaseImages([]string{"namespace/image:my-tag@" + digestA})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resolved).To(Equal([]string{"registry.io/namespace/image:my-tag@" + digestA}))
	})

	t.Run("should resolve multiple images", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				switch args.Image {
				case "namespace/image-a:tag":
					return []cliwrappers.BuildahImagesEntry{
						{Names: []string{"registry.io/namespace/image-a:tag"}, Digest: digestA},
					}, nil
				case "namespace/image-b:tag":
					return []cliwrappers.BuildahImagesEntry{
						{Names: []string{"registry.io/namespace/image-b:tag"}, Digest: digestB},
					}, nil
				}
				return nil, fmt.Errorf("unexpected image: %s", args.Image)
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		resolved, err := c.resolveBaseImages([]string{"namespace/image-a:tag", "namespace/image-b:tag"})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(resolved).To(Equal([]string{
			"registry.io/namespace/image-a:tag@" + digestA,
			"registry.io/namespace/image-b:tag@" + digestB,
		}))
	})

	t.Run("should error if ImagesJson fails", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return nil, errors.New("image not known")
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		_, err := c.resolveBaseImages([]string{"namespace/image:tag"})

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("buildah images namespace/image:tag"))
	})

	t.Run("should error if input ref is unparseable", func(t *testing.T) {
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: &mockBuildahCli{}},
			Params:      &BuildParams{},
		}

		_, err := c.resolveBaseImages([]string{"registry.io/imAge:tag"})

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("parsing registry.io/imAge:tag"))
	})
}

func Test_Build_writeResolvedBaseImages(t *testing.T) {
	g := NewWithT(t)

	const digestA = "sha256:586ab46b9d6d906b2df3dad12751e807bd0f0632d5a2ab3991bdac78bdccd59a"

	t.Run("should write correct file content", func(t *testing.T) {
		tempDir := t.TempDir()
		outputPath := filepath.Join(tempDir, "resolved-base-images.txt")

		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return []cliwrappers.BuildahImagesEntry{
					{Names: []string{"registry.io/namespace/image:tag"}, Digest: digestA},
				}, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		err := c.writeResolvedBaseImages([]string{"namespace/image:tag"}, outputPath)

		g.Expect(err).ToNot(HaveOccurred())
		content, readErr := os.ReadFile(outputPath)
		g.Expect(readErr).ToNot(HaveOccurred())
		g.Expect(string(content)).To(Equal(
			"namespace/image:tag registry.io/namespace/image:tag@" + digestA + "\n",
		))
	})

	t.Run("should write empty file for no images", func(t *testing.T) {
		tempDir := t.TempDir()
		outputPath := filepath.Join(tempDir, "resolved-base-images.txt")

		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: &mockBuildahCli{}},
			Params:      &BuildParams{},
		}

		err := c.writeResolvedBaseImages(nil, outputPath)

		g.Expect(err).ToNot(HaveOccurred())
		content, readErr := os.ReadFile(outputPath)
		g.Expect(readErr).ToNot(HaveOccurred())
		g.Expect(string(content)).To(BeEmpty())
	})

	t.Run("should propagate error from resolveBaseImages", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return nil, errors.New("image not known")
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		err := c.writeResolvedBaseImages([]string{"namespace/image:tag"}, "/tmp/out.txt")

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("determining resolved base images"))
	})

	t.Run("should return error for unwritable path", func(t *testing.T) {
		mock := &mockBuildahCli{
			ImagesJsonFunc: func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
				return []cliwrappers.BuildahImagesEntry{
					{Names: []string{"registry.io/namespace/image:tag"}, Digest: digestA},
				}, nil
			},
		}
		c := &Build{
			CliWrappers: BuildCliWrappers{BuildahCli: mock},
			Params:      &BuildParams{},
		}

		err := c.writeResolvedBaseImages([]string{"namespace/image:tag"}, "/nonexistent/directory/output.txt")

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("writing resolved base images"))
	})
}

func Test_chmodAddRWX(t *testing.T) {
	g := NewWithT(t)

	getPerm := func(path string) os.FileMode {
		t.Helper()
		info, err := os.Stat(path)
		g.Expect(err).ToNot(HaveOccurred())
		return info.Mode().Perm()
	}

	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	g.Expect(os.Mkdir(root, 0700)).To(Succeed())

	nested := filepath.Join(root, "nested")
	g.Expect(os.Mkdir(nested, 0700)).To(Succeed())

	regularFile := filepath.Join(nested, "data.txt")
	g.Expect(os.WriteFile(regularFile, []byte("data"), 0600)).To(Succeed())

	execFile := filepath.Join(nested, "run.sh")
	g.Expect(os.WriteFile(execFile, []byte("#!/bin/sh"), 0700)).To(Succeed())

	// Symlink pointing outside the tree - should be skipped without affecting the target,
	// shouldn't prevent the walk from proceeding
	symlinkTarget := filepath.Join(dir, "outside-root")
	g.Expect(os.WriteFile(symlinkTarget, []byte("data"), 0400)).To(Succeed())
	symlink := filepath.Join(root, "link")
	g.Expect(os.Symlink(symlinkTarget, symlink)).To(Succeed())

	// Restrict root to 0600 (not traversable) after creating children
	g.Expect(os.Chmod(root, 0600)).To(Succeed())

	g.Expect(chmodAddRWX(root)).To(Succeed())

	g.Expect(getPerm(root)).To(Equal(os.FileMode(0777)))
	g.Expect(getPerm(nested)).To(Equal(os.FileMode(0777)))
	g.Expect(getPerm(regularFile)).To(Equal(os.FileMode(0666)))
	g.Expect(getPerm(execFile)).To(Equal(os.FileMode(0777)))
	g.Expect(getPerm(symlinkTarget)).To(Equal(os.FileMode(0400)))
}

func Test_Build_copyPrefetchDir(t *testing.T) {
	readFile := func(g Gomega, path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		g.Expect(err).ToNot(HaveOccurred())
		return string(data)
	}
	getPerm := func(g Gomega, path string) os.FileMode {
		t.Helper()
		info, err := os.Stat(path)
		g.Expect(err).ToNot(HaveOccurred())
		return info.Mode().Perm()
	}

	t.Run("basic copy with custom destination", func(t *testing.T) {
		g := NewWithT(t)

		srcDir := t.TempDir()
		testutil.WriteFileTree(t, srcDir, map[string]string{
			"file1.txt":        "hello",
			"subdir/file2.txt": "world",
		})

		dstDir := filepath.Join(t.TempDir(), "dest")
		c := &Build{Params: &BuildParams{PrefetchDir: srcDir, PrefetchDirCopy: dstDir}}

		result, err := c.copyPrefetchDir()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result).To(Equal(dstDir))

		g.Expect(readFile(g, filepath.Join(dstDir, "file1.txt"))).To(Equal("hello"))
		g.Expect(readFile(g, filepath.Join(dstDir, "subdir", "file2.txt"))).To(Equal("world"))
		g.Expect(c.tempFilesOutsideWorkdir).To(ContainElement(dstDir))
	})

	t.Run("default destination is subdirectory of source", func(t *testing.T) {
		g := NewWithT(t)

		srcDir := t.TempDir()
		testutil.WriteFileTree(t, srcDir, map[string]string{
			"file.txt": "content",
		})

		c := &Build{Params: &BuildParams{PrefetchDir: srcDir}}

		result, err := c.copyPrefetchDir()
		g.Expect(err).ToNot(HaveOccurred())

		// The copy is a subdirectory of the source
		g.Expect(result).To(HavePrefix(srcDir + string(os.PathSeparator)))
		g.Expect(filepath.Base(result)).To(HavePrefix("copy-"))

		// Source file is present in the copy
		g.Expect(readFile(g, filepath.Join(result, "file.txt"))).To(Equal("content"))

		// The copy does not contain a recursive copy of itself
		entries, err := os.ReadDir(result)
		g.Expect(err).ToNot(HaveOccurred())
		for _, entry := range entries {
			g.Expect(entry.Name()).ToNot(HavePrefix("copy-"),
				"copy dir should not contain a nested copy")
		}

		g.Expect(c.tempFilesOutsideWorkdir).To(ContainElement(result))
	})

	t.Run("filters RPM dirs by architecture", func(t *testing.T) {
		g := NewWithT(t)

		currentArch := goArchToRpmArch(runtime.GOARCH)
		otherArch := "s390x"

		srcDir := t.TempDir()
		testutil.WriteFileTree(t, srcDir, map[string]string{
			filepath.Join("output", "deps", "rpm", currentArch, "packages", "foo.rpm"): "matching",
			filepath.Join("output", "deps", "rpm", otherArch, "packages", "bar.rpm"):   "non-matching",
			"other/file.txt": "kept",
		})

		dstDir := filepath.Join(t.TempDir(), "dest")
		c := &Build{Params: &BuildParams{PrefetchDir: srcDir, PrefetchDirCopy: dstDir}}

		result, err := c.copyPrefetchDir()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result).To(Equal(dstDir))

		// Matching arch is copied
		g.Expect(readFile(g, filepath.Join(dstDir, "output", "deps", "rpm", currentArch, "packages", "foo.rpm"))).
			To(Equal("matching"))
		// Non-matching arch is skipped
		g.Expect(filepath.Join(dstDir, "output", "deps", "rpm", otherArch)).ToNot(BeAnExistingFile())
		// Other files are copied
		g.Expect(readFile(g, filepath.Join(dstDir, "other", "file.txt"))).To(Equal("kept"))
	})

	t.Run("preserves symlinks", func(t *testing.T) {
		g := NewWithT(t)

		srcDir := t.TempDir()
		testutil.WriteFileTree(t, srcDir, map[string]string{
			"target.txt": "target content",
		})
		g.Expect(os.Symlink("target.txt", filepath.Join(srcDir, "link.txt"))).To(Succeed())

		dstDir := filepath.Join(t.TempDir(), "dest")
		c := &Build{Params: &BuildParams{PrefetchDir: srcDir, PrefetchDirCopy: dstDir}}

		_, err := c.copyPrefetchDir()
		g.Expect(err).ToNot(HaveOccurred())

		// The copy contains the regular file
		g.Expect(readFile(g, filepath.Join(dstDir, "target.txt"))).To(Equal("target content"))

		// The copy contains a symlink with the same target
		linkTarget, err := os.Readlink(filepath.Join(dstDir, "link.txt"))
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(linkTarget).To(Equal("target.txt"))
	})

	t.Run("sets permissions", func(t *testing.T) {
		g := NewWithT(t)

		srcDir := t.TempDir()
		testutil.WriteFileTree(t, srcDir, map[string]string{
			"restricted/data.txt": "data",
			"restricted/run.sh":   "#!/bin/sh",
		})
		g.Expect(os.Chmod(filepath.Join(srcDir, "restricted", "data.txt"), 0400)).To(Succeed())
		g.Expect(os.Chmod(filepath.Join(srcDir, "restricted", "run.sh"), 0500)).To(Succeed())
		g.Expect(os.Chmod(filepath.Join(srcDir, "restricted"), 0700)).To(Succeed())

		dstDir := filepath.Join(t.TempDir(), "dest")
		c := &Build{Params: &BuildParams{PrefetchDir: srcDir, PrefetchDirCopy: dstDir}}

		_, err := c.copyPrefetchDir()
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(getPerm(g, filepath.Join(dstDir, "restricted"))).To(Equal(os.FileMode(0777)))
		g.Expect(getPerm(g, filepath.Join(dstDir, "restricted", "data.txt"))).To(Equal(os.FileMode(0666)))
		g.Expect(getPerm(g, filepath.Join(dstDir, "restricted", "run.sh"))).To(Equal(os.FileMode(0777)))
	})

	t.Run("error when destination already exists", func(t *testing.T) {
		g := NewWithT(t)

		srcDir := t.TempDir()
		testutil.WriteFileTree(t, srcDir, map[string]string{
			"file.txt": "content",
		})

		dstDir := filepath.Join(t.TempDir(), "dest")
		g.Expect(os.Mkdir(dstDir, 0755)).To(Succeed())

		c := &Build{Params: &BuildParams{PrefetchDir: srcDir, PrefetchDirCopy: dstDir}}

		_, err := c.copyPrefetchDir()
		g.Expect(err).To(MatchError(ContainSubstring("file exists")))
	})
}

func Test_determineContentSets(t *testing.T) {
	writeSbomFile := func(t *testing.T, content string) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "sbom.json")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("should extract repository_id from CycloneDX SBOM", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{
			"bomFormat": "CycloneDX",
			"components": [
				{"purl": "pkg:rpm/redhat/bash@5.1.8?repository_id=ubi-10-baseos-rpms"},
				{"purl": "pkg:rpm/redhat/glibc@2.34?repository_id=ubi-10-appstream-rpms"}
			]
		}`)

		result, err := determineContentSets(sbom)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result["content_sets"]).To(Equal([]string{
			"ubi-10-appstream-rpms",
			"ubi-10-baseos-rpms",
		}))
	})

	t.Run("should extract repository_id from SPDX SBOM", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{
			"packages": [
				{
					"externalRefs": [
						{"referenceType": "purl", "referenceLocator": "pkg:rpm/redhat/bash@5.1.8?repository_id=ubi-10-baseos-rpms"}
					]
				},
				{
					"externalRefs": [
						{"referenceType": "purl", "referenceLocator": "pkg:rpm/redhat/glibc@2.34?repository_id=ubi-10-appstream-rpms"}
					]
				}
			]
		}`)

		result, err := determineContentSets(sbom)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result["content_sets"]).To(Equal([]string{
			"ubi-10-appstream-rpms",
			"ubi-10-baseos-rpms",
		}))
	})

	t.Run("should deduplicate repository_ids", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{
			"bomFormat": "CycloneDX",
			"components": [
				{"purl": "pkg:rpm/redhat/bash@5.1.8?repository_id=ubi-10-baseos-rpms"},
				{"purl": "pkg:rpm/redhat/coreutils@8.32?repository_id=ubi-10-baseos-rpms"}
			]
		}`)

		result, err := determineContentSets(sbom)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result["content_sets"]).To(Equal([]string{
			"ubi-10-baseos-rpms",
		}))
	})

	t.Run("should sort content_sets alphabetically", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{
			"bomFormat": "CycloneDX",
			"components": [
				{"purl": "pkg:rpm/redhat/z-pkg@1.0?repository_id=zzz-repo"},
				{"purl": "pkg:rpm/redhat/a-pkg@1.0?repository_id=aaa-repo"},
				{"purl": "pkg:rpm/redhat/m-pkg@1.0?repository_id=mmm-repo"}
			]
		}`)

		result, err := determineContentSets(sbom)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result["content_sets"]).To(Equal([]string{
			"aaa-repo",
			"mmm-repo",
			"zzz-repo",
		}))
	})

	t.Run("should return empty content_sets when no repository_id qualifiers", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{
			"bomFormat": "CycloneDX",
			"components": [
				{"purl": "pkg:rpm/redhat/bash@5.1.8?arch=x86_64"}
			]
		}`)

		result, err := determineContentSets(sbom)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result["content_sets"]).To(BeEmpty())
	})

	t.Run("should skip malformed PURLs without error", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{
			"bomFormat": "CycloneDX",
			"components": [
				{"purl": "not-a-valid-purl"},
				{"purl": "pkg:rpm/redhat/bash@5.1.8?repository_id=ubi-10-baseos-rpms"}
			]
		}`)

		result, err := determineContentSets(sbom)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result["content_sets"]).To(Equal([]string{
			"ubi-10-baseos-rpms",
		}))
	})

	t.Run("should skip non-purl externalRefs in SPDX", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{
			"packages": [
				{
					"externalRefs": [
						{"referenceType": "cpe23Type", "referenceLocator": "cpe:2.3:a:redhat:bash:5.1.8"},
						{"referenceType": "purl", "referenceLocator": "pkg:rpm/redhat/bash@5.1.8?repository_id=ubi-10-baseos-rpms"}
					]
				}
			]
		}`)

		result, err := determineContentSets(sbom)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(result["content_sets"]).To(Equal([]string{
			"ubi-10-baseos-rpms",
		}))
	})

	t.Run("should error when file does not exist", func(t *testing.T) {
		g := NewWithT(t)

		_, err := determineContentSets("/nonexistent/sbom.json")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("reading prefetch SBOM"))
	})

	t.Run("should error on invalid JSON", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{not valid json}`)

		_, err := determineContentSets(sbom)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("unmarshalling prefetch SBOM"))
	})

	t.Run("should include correct metadata", func(t *testing.T) {
		g := NewWithT(t)

		sbom := writeSbomFile(t, `{
			"bomFormat": "CycloneDX",
			"components": []
		}`)

		result, err := determineContentSets(sbom)
		g.Expect(err).ToNot(HaveOccurred())

		metadata, ok := result["metadata"].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(metadata["icm_version"]).To(Equal(1))
		g.Expect(metadata["icm_spec"]).To(Equal(
			"https://raw.githubusercontent.com/containerbuildsystem/atomic-reactor/master/atomic_reactor/schemas/content_manifest.json",
		))
		g.Expect(metadata["image_layer_index"]).To(Equal(0))
		g.Expect(result["from_dnf_hint"]).To(BeTrue())
	})
}

func Test_Build_shouldMountRHSMcaCerts(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name                      string
		rhsmMountCACerts          string
		rhsmEntitlements          string
		rhsmActivationKey         string
		rhsmActivationPreregister bool
		expected                  bool
	}{
		{
			name:             "'always' returns true",
			rhsmMountCACerts: "always",
			expected:         true,
		},
		{
			name:             "'never' returns false",
			rhsmMountCACerts: "never",
			expected:         false,
		},
		{
			name:             "auto with entitlements returns true",
			expected:         true,
			rhsmEntitlements: "/path/to/entitlements",
		},
		{
			name:                      "auto with activation key and preregister returns true",
			rhsmActivationKey:         "/path/to/key",
			rhsmActivationPreregister: true,
			expected:                  true,
		},
		{
			name:              "auto with activation key without preregister (self-registration) returns false",
			rhsmActivationKey: "/path/to/key",
			expected:          false,
		},
		{
			name:              "explicit 'auto' behaves like default",
			rhsmMountCACerts:  "auto",
			rhsmActivationKey: "/path/to/key",
			expected:          false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Build{
				Params: &BuildParams{
					RHSMMountCACerts:          tc.rhsmMountCACerts,
					RHSMEntitlements:          tc.rhsmEntitlements,
					RHSMActivationKey:         tc.rhsmActivationKey,
					RHSMActivationPreregister: tc.rhsmActivationPreregister,
				},
			}
			g.Expect(c.shouldMountRHSMcaCerts()).To(Equal(tc.expected))
		})
	}
}

func Test_Build_integrateWithRHSM(t *testing.T) {
	t.Run("should error when entitlements dir does not exist", func(t *testing.T) {
		g := NewWithT(t)

		c := &Build{
			Params: &BuildParams{
				RHSMEntitlements: "/nonexistent/entitlements",
			},
		}
		defer c.cleanup()

		err := c.integrateWithRHSM()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("copying entitlements"))
	})

	t.Run("should error when activation key file does not exist", func(t *testing.T) {
		g := NewWithT(t)

		tempDir := t.TempDir()
		orgFile := filepath.Join(tempDir, "org.txt")
		g.Expect(os.WriteFile(orgFile, []byte("my-org"), 0644)).To(Succeed())

		c := &Build{
			Params: &BuildParams{
				RHSMActivationKey:   filepath.Join(tempDir, "nonexistent-key.txt"),
				RHSMOrg:             orgFile,
				RHSMActivationMount: "/activation-key",
			},
		}
		defer c.cleanup()

		err := c.integrateWithRHSM()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("copying activation key file"))
	})

	t.Run("should error on rhsm-mount-ca-certs=always when CA dir does not exist", func(t *testing.T) {
		g := NewWithT(t)

		c := &Build{
			Params: &BuildParams{
				RHSMEntitlements: t.TempDir(),
				RHSMMountCACerts: "always",
			},
			hostRHSMcaCerts: "/nonexistent/rhsm/ca",
		}
		defer c.cleanup()

		err := c.integrateWithRHSM()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("rhsm-mount-ca-certs=always, but /nonexistent/rhsm/ca doesn't exist"))
	})

	t.Run("should warn when CA dir does not exist in auto mode", func(t *testing.T) {
		g := NewWithT(t)

		c := &Build{
			Params: &BuildParams{
				RHSMEntitlements: t.TempDir(),
			},
			hostRHSMcaCerts: "/nonexistent/rhsm/ca",
		}
		defer c.cleanup()

		logOutput := testutil.CaptureLogOutput(func() {
			err := c.integrateWithRHSM()
			g.Expect(err).ToNot(HaveOccurred())
		})

		g.Expect(logOutput).To(ContainSubstring("Couldn't mount RHSM CA certificates"))

		for _, volume := range c.buildahVolumes {
			g.Expect(volume.ContainerDir).To(Equal("/etc/pki/entitlement"),
				"should mount only the /etc/pki/entitlement directory, nothing else")
		}
	})

	t.Run("should error when subscription-manager registration fails", func(t *testing.T) {
		g := NewWithT(t)

		tempDir := t.TempDir()
		testutil.WriteFileTree(t, tempDir, map[string]string{
			"key.txt": "my-key",
			"org.txt": "my-org",
		})

		mockSM := &mockSubscriptionManagerCli{
			RegisterFunc: func(params *cliwrappers.SubscriptionManagerRegisterParams) error {
				return errors.New("network timeout")
			},
		}

		c := &Build{
			Params: &BuildParams{
				RHSMActivationKey:         filepath.Join(tempDir, "key.txt"),
				RHSMOrg:                   filepath.Join(tempDir, "org.txt"),
				RHSMActivationPreregister: true,
				RHSMMountCACerts:          "never",
			},
			CliWrappers: BuildCliWrappers{SubscriptionManager: mockSM},
		}
		defer c.cleanup()

		err := c.integrateWithRHSM()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("registering with subscription-manager: network timeout"))
		g.Expect(c.registeredWithRHSM).To(BeFalse(),
			"should not be marked as registered when registration fails")
	})
}

func Test_Build_injectPrefetchEnvToContainerfile(t *testing.T) {
	// Injection is thoroughly tested in RunInjector tests, test only the interesting cases here.
	tests := []struct {
		name     string
		envMount string
		input    string
		expected string
	}{
		{
			name:     "env mount path is shell-quoted",
			envMount: "/path/with spaces/prefetch.env",
			input: strings.Join([]string{
				`FROM scratch`,
				`RUN dnf install -y pkg`,
				``,
			}, "\n"),
			expected: strings.Join([]string{
				`FROM scratch`,
				`RUN . '/path/with spaces/prefetch.env' && \`,
				`    dnf install -y pkg`,
				``,
			}, "\n"),
		},
		{
			name:     "injects correctly into backtick-escaped containerfile",
			envMount: "/tmp/.prefetch.env",
			input: strings.Join([]string{
				"# escape=`",
				"FROM scratch",
				"RUN dnf install -y pkg",
				``,
			}, "\n"),
			expected: strings.Join([]string{
				"# escape=`",
				"FROM scratch",
				"RUN . /tmp/.prefetch.env && `",
				"    dnf install -y pkg",
				``,
			}, "\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tempDir := t.TempDir()
			containerfile := filepath.Join(tempDir, "Containerfile")
			g.Expect(os.WriteFile(containerfile, []byte(tt.input), 0644)).To(Succeed())

			c := &Build{
				Params:            &BuildParams{},
				containerfilePath: containerfile,
			}

			g.Expect(c.injectPrefetchEnvToContainerfile(tt.envMount)).To(Succeed())

			// Original is unchanged
			originalContent, err := os.ReadFile(containerfile)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(originalContent)).To(Equal(tt.input))

			// Modified copy has injection
			copyContent, err := os.ReadFile(c.containerfileCopyPath)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(copyContent)).To(Equal(tt.expected))
		})
	}
}
