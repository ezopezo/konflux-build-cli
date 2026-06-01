package cliwrappers_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/konflux-ci/konflux-build-cli/pkg/cliwrappers"
)

func setupBuildahCli() (*cliwrappers.BuildahCli, *mockExecutor) {
	executor := &mockExecutor{}
	buildahCli := &cliwrappers.BuildahCli{Executor: executor}
	return buildahCli, executor
}

func boolPtr(b bool) *bool { return &b }

func ensureRetryerDisabled(t *testing.T) {
	retryerDisabled := cliwrappers.DisableRetryer
	if !retryerDisabled {
		cliwrappers.DisableRetryer = true
		t.Cleanup(func() { cliwrappers.DisableRetryer = false })
	}
}

func collectEnv(env []string) map[string]string {
	envMap := make(map[string]string)
	for _, envVar := range env {
		key, val, _ := strings.Cut(envVar, "=")
		envMap[key] = val
	}
	return envMap
}

func TestBuildahCli_Build(t *testing.T) {
	g := NewWithT(t)

	const containerfile = "/path/to/Containerfile"
	const contextDir = "/path/to/context"
	const outputRef = "quay.io/org/image:tag"

	t.Run("should execute buildah correctly", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
		}

		err := buildahCli.Build(buildArgs)

		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(capturedArgs[0]).To(Equal("build"))
		expectArgAndValue(g, capturedArgs, "--file", containerfile)
		expectArgAndValue(g, capturedArgs, "--tag", outputRef)
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(contextDir))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to execute buildah build")
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
		}

		err := buildahCli.Build(buildArgs)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to execute buildah build"))
	})

	t.Run("should error if args are invalid", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: "",
			ContextDir:    contextDir,
			OutputRef:     outputRef,
		}
		err := buildahCli.Build(buildArgs)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("validating buildah args: containerfile path is empty"))
	})

	t.Run("should turn Secrets into --secret params", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			Secrets: []cliwrappers.BuildahSecret{
				{Src: "/some/file", Id: "mysecret_1"},
				{Src: "/other/file", Id: "mysecret_2"},
			},
		}

		err := buildahCli.Build(buildArgs)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(capturedArgs).To(ContainElement("--secret=src=/some/file,id=mysecret_1"))
		g.Expect(capturedArgs).To(ContainElement("--secret=src=/other/file,id=mysecret_2"))
	})

	t.Run("should turn Mounts into --mount params", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			Secrets: []cliwrappers.BuildahSecret{
				{Src: "/some/file", Id: "GOMODCACHE"},
				{Src: "/other/file", Id: "regular_secret"},
			},
			Mounts: []cliwrappers.BuildahMount{
				{Type: "secret", Id: "GOMODCACHE", Env: "GOMODCACHE"},
			},
		}

		err := buildahCli.Build(buildArgs)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(capturedArgs).To(ContainElement("--secret=src=/some/file,id=GOMODCACHE"))
		g.Expect(capturedArgs).To(ContainElement("--mount=type=secret,id=GOMODCACHE,env=GOMODCACHE"))
		g.Expect(capturedArgs).To(ContainElement("--secret=src=/other/file,id=regular_secret"))
		g.Expect(capturedArgs).ToNot(ContainElement(ContainSubstring("--mount=type=secret,id=regular_secret")))
	})

	t.Run("should turn Volumes into --volume params", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			Volumes: []cliwrappers.BuildahVolume{
				{HostDir: "/host/dir1", ContainerDir: "/container/dir1", Options: ""},
				{HostDir: "/host/dir2", ContainerDir: "/container/dir2", Options: "ro"},
			},
		}

		err := buildahCli.Build(buildArgs)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(capturedArgs).To(ContainElement("--volume=/host/dir1:/container/dir1"))
		g.Expect(capturedArgs).To(ContainElement("--volume=/host/dir2:/container/dir2:ro"))
	})

	t.Run("should turn BuildContextx into --build-context params", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			BuildContexts: []cliwrappers.BuildahBuildContext{
				{Name: "context1", Location: "context/dir/a"},
				{Name: "context2", Location: "/absolute/context/dir/b"},
			},
		}

		err := buildahCli.Build(buildArgs)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(capturedArgs).To(ContainElement("--build-context=context1=context/dir/a"))
		g.Expect(capturedArgs).To(ContainElement("--build-context=context2=/absolute/context/dir/b"))
	})

	t.Run("should turn BuildArgs(File) into --build-arg(-file) params", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			BuildArgs:     []string{"VERSION=1.0.0", "BUILD_DATE=2024-01-01"},
			BuildArgsFile: "/path/to/build-args-file",
		}

		err := buildahCli.Build(buildArgs)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(capturedArgs).To(ContainElement("--build-arg=VERSION=1.0.0"))
		g.Expect(capturedArgs).To(ContainElement("--build-arg=BUILD_DATE=2024-01-01"))
		g.Expect(capturedArgs).To(ContainElement("--build-arg-file=/path/to/build-args-file"))
	})

	t.Run("should turn Envs into --env params", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			Envs:          []string{"FOO=bar", "BAZ=qux"},
		}

		err := buildahCli.Build(buildArgs)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(capturedArgs).To(ContainElement("--env=FOO=bar"))
		g.Expect(capturedArgs).To(ContainElement("--env=BAZ=qux"))
	})

	t.Run("should append extra args before context directory", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		buildArgs := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			ExtraArgs:     []string{"--compat-volumes", "--force-rm"},
		}

		err := buildahCli.Build(buildArgs)

		g.Expect(err).ToNot(HaveOccurred())

		// Verify the command structure
		g.Expect(capturedArgs[0]).To(Equal("build"))
		expectArgAndValue(g, capturedArgs, "--file", containerfile)
		expectArgAndValue(g, capturedArgs, "--tag", outputRef)
		// Extra args should be present
		g.Expect(capturedArgs).To(ContainElement("--compat-volumes"))
		g.Expect(capturedArgs).To(ContainElement("--force-rm"))
		// Context directory should be the last argument
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(contextDir))
	})

	t.Run("should not pass --tls-verify by default", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Build(&cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile, ContextDir: contextDir, OutputRef: outputRef,
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).ToNot(ContainElement(ContainSubstring("--tls-verify")))
	})

	t.Run("should pass --tls-verify=true", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Build(&cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile, ContextDir: contextDir, OutputRef: outputRef,
			TLSVerify: boolPtr(true),
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--tls-verify=true"))
	})

	t.Run("should pass --no-cache", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Build(&cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile, ContextDir: contextDir, OutputRef: outputRef,
			NoCache: true,
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--no-cache"))
	})

	t.Run("should pass SecurityOpts as separate --security-opt args", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Build(&cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile, ContextDir: contextDir, OutputRef: outputRef,
			SecurityOpts: []string{"seccomp=unconfined", "label=disable"},
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--security-opt=seccomp=unconfined"))
		g.Expect(capturedArgs).To(ContainElement("--security-opt=label=disable"))
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(contextDir))
	})

	t.Run("should pass Cap{Add,Drop} as separate --cap-{add,drop} args", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Build(&cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile, ContextDir: contextDir, OutputRef: outputRef,
			CapAdd:  []string{"ALL", "SYS_ADMIN"},
			CapDrop: []string{"MKNOD", "CAP_SETUID,CAP_SETGID"},
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--cap-add=ALL"))
		g.Expect(capturedArgs).To(ContainElement("--cap-add=SYS_ADMIN"))
		g.Expect(capturedArgs).To(ContainElement("--cap-drop=MKNOD"))
		g.Expect(capturedArgs).To(ContainElement("--cap-drop=CAP_SETUID,CAP_SETGID"))
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(contextDir))
	})

	t.Run("should pass Devices as separate --device args", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Build(&cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile, ContextDir: contextDir, OutputRef: outputRef,
			Devices: []string{"/dev/fuse", "/dev/sdc"},
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--device=/dev/fuse"))
		g.Expect(capturedArgs).To(ContainElement("--device=/dev/sdc"))
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(contextDir))
	})

	t.Run("should pass Ulimits as separate --ulimit args", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Build(&cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile, ContextDir: contextDir, OutputRef: outputRef,
			Ulimits: []string{"nofile=4096:4096", "nproc=1024:2048"},
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--ulimit=nofile=4096:4096"))
		g.Expect(capturedArgs).To(ContainElement("--ulimit=nproc=1024:2048"))
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(contextDir))
	})
}

func findDigestFile(args []string) string {
	for i, arg := range args {
		if arg == "--digestfile" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestBuildahCli_Push(t *testing.T) {
	g := NewWithT(t)

	const image = "quay.io/org/image:tag"
	const digest = "sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

	ensureRetryerDisabled(t)

	mockSuccessfulPush := func(captureArgs *[]string) func(cmd cliwrappers.Cmd) (string, string, int, error) {
		return func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			*captureArgs = cmd.Args

			digestFile := findDigestFile(cmd.Args)
			g.Expect(digestFile).ToNot(BeEmpty())

			os.WriteFile(digestFile, []byte(digest), 0644)

			return "", "", 0, nil
		}
	}

	t.Run("should push image", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = mockSuccessfulPush(&capturedArgs)

		pushArgs := &cliwrappers.BuildahPushArgs{
			Image: image,
		}

		returnedDigest, err := buildahCli.Push(pushArgs)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs[0]).To(Equal("push"))
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(image))

		g.Expect(returnedDigest).To(Equal(digest))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to execute buildah push")
		}

		pushArgs := &cliwrappers.BuildahPushArgs{
			Image: image,
		}

		_, err := buildahCli.Push(pushArgs)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to execute buildah push"))
	})

	t.Run("should error if image is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		pushArgs := &cliwrappers.BuildahPushArgs{
			Image: "",
		}
		_, err := buildahCli.Push(pushArgs)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("image arg is empty"))
	})

	t.Run("should clean up digest file after push", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = mockSuccessfulPush(&capturedArgs)

		pushArgs := &cliwrappers.BuildahPushArgs{
			Image: image,
		}

		_, err := buildahCli.Push(pushArgs)

		g.Expect(err).ToNot(HaveOccurred())

		digestFile := findDigestFile(capturedArgs)
		g.Expect(digestFile).ToNot(BeEmpty())

		// Verify the digest file was cleaned up
		_, statErr := os.Stat(digestFile)
		g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "digest file should be cleaned up")
	})

	t.Run("should handle digest with whitespace", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		digestWithWhitespace := "\n  " + digest + "  \n"
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			digestFile := findDigestFile(cmd.Args)
			os.WriteFile(digestFile, []byte(digestWithWhitespace), 0644)
			return "", "", 0, nil
		}

		pushArgs := &cliwrappers.BuildahPushArgs{
			Image: image,
		}

		returnedDigest, err := buildahCli.Push(pushArgs)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(returnedDigest).To(Equal(digest), "digest should be trimmed")
	})

	t.Run("should include destination when provided", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		const destination = "docker://quay.io/other-org/other-image:tag"
		var capturedArgs []string
		executor.executeFunc = mockSuccessfulPush(&capturedArgs)

		pushArgs := &cliwrappers.BuildahPushArgs{
			Image:       image,
			Destination: destination,
		}

		_, err := buildahCli.Push(pushArgs)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs[0]).To(Equal("push"))
		g.Expect(capturedArgs[len(capturedArgs)-2]).To(Equal(image))
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(destination))
	})

	t.Run("should not pass --tls-verify by default", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = mockSuccessfulPush(&capturedArgs)

		_, err := buildahCli.Push(&cliwrappers.BuildahPushArgs{Image: image})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).ToNot(ContainElement(ContainSubstring("--tls-verify")))
	})

	t.Run("should pass --tls-verify=true", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = mockSuccessfulPush(&capturedArgs)

		_, err := buildahCli.Push(&cliwrappers.BuildahPushArgs{
			Image: image, TLSVerify: boolPtr(true),
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--tls-verify=true"))
	})
}

func TestBuildahCli_Pull(t *testing.T) {
	g := NewWithT(t)

	const image = "quay.io/org/image:tag"

	ensureRetryerDisabled(t)

	t.Run("should pull image", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		pullArgs := &cliwrappers.BuildahPullArgs{
			Image: image,
		}

		err := buildahCli.Pull(pullArgs)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs[0]).To(Equal("pull"))
		g.Expect(capturedArgs[1]).To(Equal(image))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to execute buildah pull")
		}

		pullArgs := &cliwrappers.BuildahPullArgs{
			Image: image,
		}

		err := buildahCli.Pull(pullArgs)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to execute buildah pull"))
	})

	t.Run("should error if image is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		pullArgs := &cliwrappers.BuildahPullArgs{
			Image: "",
		}
		err := buildahCli.Pull(pullArgs)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("image arg is empty"))
	})

	t.Run("should not pass --tls-verify by default", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Pull(&cliwrappers.BuildahPullArgs{Image: image})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).ToNot(ContainElement(ContainSubstring("--tls-verify")))
	})

	t.Run("should pass --tls-verify=true", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Pull(&cliwrappers.BuildahPullArgs{
			Image: image, TLSVerify: boolPtr(true),
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--tls-verify=true"))
	})

	t.Run("should pass ExtraEnv to the command", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedEnv []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedEnv = cmd.Env
			return "", "", 0, nil
		}

		t.Setenv("SHOULD_KEEP", "existing-env")

		err := buildahCli.Pull(&cliwrappers.BuildahPullArgs{
			Image:    image,
			ExtraEnv: []string{"FOO=bar", "SPAM=eggs"},
		})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(collectEnv(capturedEnv)).To(SatisfyAll(
			HaveKeyWithValue("SHOULD_KEEP", "existing-env"),
			HaveKeyWithValue("FOO", "bar"),
			HaveKeyWithValue("SPAM", "eggs"),
		))
	})

	t.Run("proxy vars should take precedence over ExtraEnv and existing env", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedEnv []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedEnv = cmd.Env
			return "", "", 0, nil
		}

		t.Setenv("SHOULD_KEEP", "existing-env")
		t.Setenv("HTTP_PROXY", "from-existing-env")

		err := buildahCli.Pull(&cliwrappers.BuildahPullArgs{
			Image:     image,
			ExtraEnv:  []string{"HTTP_PROXY=from-extra-env"},
			HttpProxy: "from-args",
		})
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(collectEnv(capturedEnv)).To(SatisfyAll(
			HaveKeyWithValue("SHOULD_KEEP", "existing-env"),
			HaveKeyWithValue("HTTP_PROXY", "from-args"),
		))
	})
}

func TestBuildahCli_Inspect(t *testing.T) {
	g := NewWithT(t)

	const imageName = "localhost/image:tag"

	t.Run("should execute buildah inspect correctly", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return `{"OCIv1": {}}`, "", 0, nil
		}

		inspectArgs := &cliwrappers.BuildahInspectArgs{
			Name: imageName,
			Type: "image",
		}

		jsonOutput, err := buildahCli.Inspect(inspectArgs)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(jsonOutput).To(ContainSubstring("OCIv1"))
		g.Expect(capturedArgs[0]).To(Equal("inspect"))
		expectArgAndValue(g, capturedArgs, "--type", "image")
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(imageName))
	})

	t.Run("should error when name is empty", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executorCalled := false
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			executorCalled = true
			return "", "", 0, nil
		}

		inspectArgs := &cliwrappers.BuildahInspectArgs{
			Name: "",
			Type: "image",
		}

		_, err := buildahCli.Inspect(inspectArgs)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("name is empty"))
		g.Expect(executorCalled).To(BeFalse())
	})

	t.Run("should error when type is empty", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executorCalled := false
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			executorCalled = true
			return "", "", 0, nil
		}

		inspectArgs := &cliwrappers.BuildahInspectArgs{
			Name: imageName,
			Type: "",
		}

		_, err := buildahCli.Inspect(inspectArgs)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("type is empty"))
		g.Expect(executorCalled).To(BeFalse())
	})

	t.Run("should error when buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("buildah inspect failed")
		}

		inspectArgs := &cliwrappers.BuildahInspectArgs{
			Name: imageName,
			Type: "image",
		}

		_, err := buildahCli.Inspect(inspectArgs)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("buildah inspect failed"))
	})
}

func TestBuildahCli_InspectImage(t *testing.T) {
	g := NewWithT(t)

	const imageName = "quay.io/org/image:tag"

	t.Run("should parse valid JSON successfully", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()

		sampleJSON := `{
			"OCIv1": {
				"created": "2024-01-01T00:00:00Z",
				"config": {
					"Env": ["PATH=/usr/bin", "HOME=/root"],
					"Labels": {"version": "1.0", "maintainer": "test"}
				}
			}
		}`

		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return sampleJSON, "", 0, nil
		}

		imageInfo, err := buildahCli.InspectImage(imageName)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(imageInfo.OCIv1.Created).ToNot(BeNil())
		g.Expect(imageInfo.OCIv1.Created.Format(time.RFC3339)).To(Equal("2024-01-01T00:00:00Z"))
		g.Expect(imageInfo.OCIv1.Config.Env).To(Equal([]string{"PATH=/usr/bin", "HOME=/root"}))
		g.Expect(imageInfo.OCIv1.Config.Labels).To(Equal(map[string]string{"version": "1.0", "maintainer": "test"}))
	})

	t.Run("should error when Inspect fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()

		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("buildah inspect failed")
		}

		_, err := buildahCli.InspectImage(imageName)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("buildah inspect failed"))
	})

	t.Run("should error when JSON parsing fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()

		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return `{invalid json}`, "", 0, nil
		}

		_, err := buildahCli.InspectImage(imageName)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("parsing inspect output"))
	})
}

func TestBuildahCli_Version(t *testing.T) {
	g := NewWithT(t)

	t.Run("should execute buildah version correctly", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			jsonOutput := `{
    "version": "1.42.2",
    "goVersion": "go1.24.10",
    "imageSpec": "1.1.1",
    "runtimeSpec": "1.2.1",
    "cniSpec": "1.1.0",
    "libcniVersion": "",
    "imageVersion": "5.38.0",
    "gitCommit": "",
    "built": "Wed Dec  3 15:03:30 2025",
    "osArch": "linux/amd64",
    "buildPlatform": "linux/amd64"
}`
			return jsonOutput, "", 0, nil
		}

		versionInfo, err := buildahCli.Version()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"version", "--json"}))

		g.Expect(versionInfo.Version).To(Equal("1.42.2"))
	})
}

func TestBuildahVersionInfo_ParseVersion(t *testing.T) {
	tests := []struct {
		name         string
		version      string
		expected     []int
		errSubstring string
	}{
		{
			name:     "valid version",
			version:  "1.44.0",
			expected: []int{1, 44, 0},
		},
		{
			name:     "valid 2.x version",
			version:  "2.0.1",
			expected: []int{2, 0, 1},
		},
		{
			name:         "empty string",
			version:      "",
			errSubstring: "buildah version is empty",
		},
		{
			name:         "too few parts",
			version:      "1.2",
			errSubstring: "expected 3-part version number",
		},
		{
			name:         "too many parts",
			version:      "1.2.3.4",
			errSubstring: "expected 3-part version number",
		},
		{
			name:         "non-numeric part",
			version:      "1.2.x",
			errSubstring: "invalid syntax",
		},
		{
			name:         "negative number",
			version:      "1.-2.3",
			errSubstring: "negative number in version: -2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			v := cliwrappers.BuildahVersionInfo{Version: tc.version}
			result, err := v.ParseVersion()

			if tc.errSubstring != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tc.errSubstring))
				g.Expect(result).To(BeNil())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(result).To(Equal(tc.expected))
			}
		})
	}
}

func TestBuildahBuildArgs_MakePathsAbsolute(t *testing.T) {
	g := NewWithT(t)

	t.Run("should not modify absolute paths", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: "/absolute/path/Containerfile",
			ContextDir:    "/absolute/path/context",
			Secrets: []cliwrappers.BuildahSecret{
				{Src: "/absolute/path/secret", Id: "secret1"},
			},
			Volumes: []cliwrappers.BuildahVolume{
				{HostDir: "/absolute/path/volume", ContainerDir: "/container/dir", Options: ""},
			},
			BuildContexts: []cliwrappers.BuildahBuildContext{
				{Name: "additional-context", Location: "/absolute/additional-context"},
			},
			BuildArgsFile: "/absolute/path/build-args-file",
		}

		err := args.MakePathsAbsolute("/base/dir")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(args.Containerfile).To(Equal("/absolute/path/Containerfile"))
		g.Expect(args.ContextDir).To(Equal("/absolute/path/context"))
		g.Expect(args.Secrets[0].Src).To(Equal("/absolute/path/secret"))
		g.Expect(args.Volumes[0].HostDir).To(Equal("/absolute/path/volume"))
		g.Expect(args.BuildContexts[0].Location).To(Equal("/absolute/additional-context"))
		g.Expect(args.BuildArgsFile).To(Equal("/absolute/path/build-args-file"))
	})

	t.Run("should make relative paths absolute", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: "relative/Containerfile",
			ContextDir:    ".",
			Secrets: []cliwrappers.BuildahSecret{
				{Src: "relative/secret", Id: "secret1"},
			},
			Volumes: []cliwrappers.BuildahVolume{
				{HostDir: "relative/volume", ContainerDir: "/container/dir", Options: ""},
			},
			BuildContexts: []cliwrappers.BuildahBuildContext{
				{Name: "additional-context", Location: "relative/additional-context"},
			},
			BuildArgsFile: "relative/build-args-file",
		}

		err := args.MakePathsAbsolute("/base/dir")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(args.Containerfile).To(Equal("/base/dir/relative/Containerfile"))
		g.Expect(args.ContextDir).To(Equal("/base/dir"))
		g.Expect(args.Secrets[0].Src).To(Equal("/base/dir/relative/secret"))
		g.Expect(args.Volumes[0].HostDir).To(Equal("/base/dir/relative/volume"))
		g.Expect(args.BuildContexts[0].Location).To(Equal("/base/dir/relative/additional-context"))
		g.Expect(args.BuildArgsFile).To(Equal("/base/dir/relative/build-args-file"))
	})

	t.Run("should handle a mix of relative and absolute paths", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: "/path/to/Containerfile",
			ContextDir:    "context",
			Secrets: []cliwrappers.BuildahSecret{
				{Src: "secret1/file", Id: "secret1"},
				{Src: "/absolute/secret2/file", Id: "secret2"},
			},
			Volumes: []cliwrappers.BuildahVolume{
				{HostDir: "volume1/dir", ContainerDir: "/container/dir1", Options: ""},
				{HostDir: "/absolute/volume2/dir", ContainerDir: "/container/dir2", Options: "ro"},
			},
			BuildContexts: []cliwrappers.BuildahBuildContext{
				{Name: "additional-context", Location: "/absolute/additional-context"},
				{Name: "additional-context", Location: "relative/additional-context"},
			},
		}

		err := args.MakePathsAbsolute("/base/dir")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(args.Containerfile).To(Equal("/path/to/Containerfile"))
		g.Expect(args.ContextDir).To(Equal("/base/dir/context"))
		g.Expect(args.Secrets[0].Src).To(Equal("/base/dir/secret1/file"))
		g.Expect(args.Secrets[1].Src).To(Equal("/absolute/secret2/file"))
		g.Expect(args.Volumes[0].HostDir).To(Equal("/base/dir/volume1/dir"))
		g.Expect(args.Volumes[1].HostDir).To(Equal("/absolute/volume2/dir"))
		g.Expect(args.BuildContexts[0].Location).To(Equal("/absolute/additional-context"))
		g.Expect(args.BuildContexts[1].Location).To(Equal("/base/dir/relative/additional-context"))
	})

	t.Run("should use current working directory when baseDir is relative", func(t *testing.T) {
		cwd, err := os.Getwd()
		g.Expect(err).ToNot(HaveOccurred())

		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: "Containerfile",
			ContextDir:    "context",
		}

		err = args.MakePathsAbsolute(".")
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(args.Containerfile).To(Equal(filepath.Join(cwd, "Containerfile")))
		g.Expect(args.ContextDir).To(Equal(filepath.Join(cwd, "context")))
	})

	t.Run("should not modify empty BuildArgsFile", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: "/absolute/path/Containerfile",
			ContextDir:    "/absolute/path/context",
			BuildArgsFile: "",
		}

		err := args.MakePathsAbsolute("/base/dir")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(args.BuildArgsFile).To(Equal(""))
	})
}

func TestBuildahBuildArgs_Validate(t *testing.T) {
	g := NewWithT(t)

	const containerfile = "/path/to/Containerfile"
	const contextDir = "/path/to/context"
	const outputRef = "quay.io/org/image:tag"

	t.Run("should succeed with valid args", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
		}

		err := args.Validate()
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("should error if containerfile is empty", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: "",
			ContextDir:    contextDir,
			OutputRef:     outputRef,
		}

		err := args.Validate()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("containerfile path is empty"))
	})

	t.Run("should error if context directory is empty", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    "",
			OutputRef:     outputRef,
		}

		err := args.Validate()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("context directory is empty"))
	})

	t.Run("should error if output-ref is empty", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     "",
		}

		err := args.Validate()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("output-ref is empty"))
	})

	t.Run("should error when volume HostDir contains ':'", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			Volumes:       []cliwrappers.BuildahVolume{{HostDir: "some:dir", ContainerDir: "/foo"}},
		}

		err := args.Validate()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("':' in volume mount source path: some:dir"))
	})

	t.Run("should error when volume ContainerDir contains ':'", func(t *testing.T) {
		args := &cliwrappers.BuildahBuildArgs{
			Containerfile: containerfile,
			ContextDir:    contextDir,
			OutputRef:     outputRef,
			Volumes:       []cliwrappers.BuildahVolume{{HostDir: "/foo", ContainerDir: "other:dir"}},
		}

		err := args.Validate()
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("':' in volume mount target path: other:dir"))
	})
}

func TestBuildahCli_ManifestCreate(t *testing.T) {
	g := NewWithT(t)

	const manifestName = "quay.io/org/myapp:latest"

	t.Run("should create manifest", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			g.Expect(cmd.LogOutput).To(BeTrue())
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		args := &cliwrappers.BuildahManifestCreateArgs{
			ManifestName: manifestName,
		}

		err := buildahCli.ManifestCreate(args)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"manifest", "create", manifestName}))
	})

	t.Run("should error if manifest name is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		args := &cliwrappers.BuildahManifestCreateArgs{
			ManifestName: "",
		}

		err := buildahCli.ManifestCreate(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("manifest name is empty"))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to create manifest")
		}

		args := &cliwrappers.BuildahManifestCreateArgs{
			ManifestName: manifestName,
		}

		err := buildahCli.ManifestCreate(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to create manifest"))
	})
}

func TestBuildahCli_ManifestAdd(t *testing.T) {
	g := NewWithT(t)

	const manifestName = "quay.io/org/myapp:latest"
	const imageRef = "docker://quay.io/org/myapp@sha256:abc123"

	t.Run("should add image to manifest", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			g.Expect(cmd.LogOutput).To(BeTrue())
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		args := &cliwrappers.BuildahManifestAddArgs{
			ManifestName: manifestName,
			ImageRef:     imageRef,
			All:          true,
		}

		err := buildahCli.ManifestAdd(args)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"manifest", "add", manifestName, imageRef, "--all"}))
	})

	t.Run("should add image without --all flag", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			g.Expect(cmd.LogOutput).To(BeTrue())
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		args := &cliwrappers.BuildahManifestAddArgs{
			ManifestName: manifestName,
			ImageRef:     imageRef,
			All:          false,
		}

		err := buildahCli.ManifestAdd(args)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"manifest", "add", manifestName, imageRef}))
	})

	t.Run("should error if manifest name is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		args := &cliwrappers.BuildahManifestAddArgs{
			ManifestName: "",
			ImageRef:     imageRef,
		}

		err := buildahCli.ManifestAdd(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("manifest name is empty"))
	})

	t.Run("should error if image reference is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		args := &cliwrappers.BuildahManifestAddArgs{
			ManifestName: manifestName,
			ImageRef:     "",
		}

		err := buildahCli.ManifestAdd(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("image reference is empty"))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to add image")
		}

		args := &cliwrappers.BuildahManifestAddArgs{
			ManifestName: manifestName,
			ImageRef:     imageRef,
			All:          true,
		}

		err := buildahCli.ManifestAdd(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to add image"))
	})
}

func TestBuildahCli_ManifestInspect(t *testing.T) {
	g := NewWithT(t)

	const manifestName = "quay.io/org/myapp:latest"
	const manifestJSON = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`

	t.Run("should inspect manifest and return JSON", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return manifestJSON, "", 0, nil
		}

		args := &cliwrappers.BuildahManifestInspectArgs{
			ManifestName: manifestName,
		}

		result, err := buildahCli.ManifestInspect(args)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"manifest", "inspect", manifestName}))
		g.Expect(result).To(Equal(manifestJSON))
	})

	t.Run("should error if manifest name is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		args := &cliwrappers.BuildahManifestInspectArgs{
			ManifestName: "",
		}

		_, err := buildahCli.ManifestInspect(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("manifest name is empty"))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to inspect manifest")
		}

		args := &cliwrappers.BuildahManifestInspectArgs{
			ManifestName: manifestName,
		}

		_, err := buildahCli.ManifestInspect(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to inspect manifest"))
	})
}

func TestBuildahCli_ManifestPush(t *testing.T) {
	g := NewWithT(t)

	const manifestName = "quay.io/org/myapp:latest"
	const destination = "docker://quay.io/org/myapp:latest"
	const digest = "sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

	ensureRetryerDisabled(t)

	mockSuccessfulManifestPush := func(captureArgs *[]string) func(cmd cliwrappers.Cmd) (string, string, int, error) {
		return func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			g.Expect(cmd.LogOutput).To(BeTrue())
			*captureArgs = cmd.Args

			digestFile := findDigestFile(cmd.Args)
			g.Expect(digestFile).ToNot(BeEmpty())

			os.WriteFile(digestFile, []byte(digest), 0644)

			return "", "", 0, nil
		}
	}

	t.Run("should push manifest with default options", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = mockSuccessfulManifestPush(&capturedArgs)

		args := &cliwrappers.BuildahManifestPushArgs{
			ManifestName: manifestName,
			Destination:  destination,
			TLSVerify:    true,
		}

		returnedDigest, err := buildahCli.ManifestPush(args)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs[0]).To(Equal("manifest"))
		g.Expect(capturedArgs[1]).To(Equal("push"))
		expectArgAndValue(g, capturedArgs, "--digestfile", findDigestFile(capturedArgs))
		g.Expect(capturedArgs).To(ContainElement("--tls-verify=true"))
		g.Expect(capturedArgs[len(capturedArgs)-2]).To(Equal(manifestName))
		g.Expect(capturedArgs[len(capturedArgs)-1]).To(Equal(destination))
		g.Expect(returnedDigest).To(Equal(digest))
	})

	t.Run("should push manifest with format and retry", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = mockSuccessfulManifestPush(&capturedArgs)

		args := &cliwrappers.BuildahManifestPushArgs{
			ManifestName: manifestName,
			Destination:  destination,
			Format:       "oci",
			TLSVerify:    false,
		}

		_, err := buildahCli.ManifestPush(args)

		g.Expect(err).ToNot(HaveOccurred())
		expectArgAndValue(g, capturedArgs, "--format", "oci")
		g.Expect(capturedArgs).To(ContainElement("--tls-verify=false"))
	})

	t.Run("should error if manifest name is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		args := &cliwrappers.BuildahManifestPushArgs{
			ManifestName: "",
			Destination:  destination,
		}

		_, err := buildahCli.ManifestPush(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("manifest name is empty"))
	})

	t.Run("should error if destination is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()
		args := &cliwrappers.BuildahManifestPushArgs{
			ManifestName: manifestName,
			Destination:  "",
		}

		_, err := buildahCli.ManifestPush(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("destination is empty"))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to push manifest")
		}

		args := &cliwrappers.BuildahManifestPushArgs{
			ManifestName: manifestName,
			Destination:  destination,
		}

		_, err := buildahCli.ManifestPush(args)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to push manifest"))
	})

	t.Run("should clean up digest file after push", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = mockSuccessfulManifestPush(&capturedArgs)

		args := &cliwrappers.BuildahManifestPushArgs{
			ManifestName: manifestName,
			Destination:  destination,
		}

		_, err := buildahCli.ManifestPush(args)

		g.Expect(err).ToNot(HaveOccurred())

		digestFile := findDigestFile(capturedArgs)
		g.Expect(digestFile).ToNot(BeEmpty())

		_, statErr := os.Stat(digestFile)
		g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "digest file should be cleaned up")
	})
}

func TestBuildahCli_Images(t *testing.T) {
	g := NewWithT(t)

	t.Run("should execute buildah images with no options", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "output", "", 0, nil
		}

		stdout, err := buildahCli.Images(&cliwrappers.BuildahImagesArgs{})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(stdout).To(Equal("output"))
		g.Expect(capturedArgs).To(Equal([]string{"images"}))
	})

	t.Run("should pass --json flag when Json is true", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		_, err := buildahCli.Images(&cliwrappers.BuildahImagesArgs{Json: true})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"images", "--json"}))
	})

	t.Run("should pass image name when Image is set", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		_, err := buildahCli.Images(&cliwrappers.BuildahImagesArgs{Image: "registry.io/namespace/image:tag"})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"images", "registry.io/namespace/image:tag"}))
	})

	t.Run("should pass both --json and image name", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		_, err := buildahCli.Images(&cliwrappers.BuildahImagesArgs{
			Json:  true,
			Image: "registry.io/namespace/image:tag",
		})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"images", "--json", "registry.io/namespace/image:tag"}))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "something went wrong", 1, errors.New("buildah images failed")
		}

		stdout, err := buildahCli.Images(&cliwrappers.BuildahImagesArgs{})

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("buildah images failed"))
		g.Expect(stdout).To(BeEmpty())
	})
}

func TestBuildahCli_ImagesJson(t *testing.T) {
	g := NewWithT(t)

	t.Run("should parse valid JSON output", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return `[{"names":["registry.io/namespace/image:tag"],"digest":"sha256:586ab46b9d6d906b2df3dad12751e807bd0f0632d5a2ab3991bdac78bdccd59a"}]`, "", 0, nil
		}

		entries, err := buildahCli.ImagesJson(&cliwrappers.BuildahImagesArgs{})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(entries).To(HaveLen(1))
		g.Expect(entries[0].Names).To(Equal([]string{"registry.io/namespace/image:tag"}))
		g.Expect(entries[0].Digest).To(Equal("sha256:586ab46b9d6d906b2df3dad12751e807bd0f0632d5a2ab3991bdac78bdccd59a"))
	})

	t.Run("should always set Json to true", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return `[]`, "", 0, nil
		}

		_, err := buildahCli.ImagesJson(&cliwrappers.BuildahImagesArgs{Json: false})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("--json"))
	})

	t.Run("should pass image name through", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedArgs = cmd.Args
			return `[]`, "", 0, nil
		}

		_, err := buildahCli.ImagesJson(&cliwrappers.BuildahImagesArgs{Image: "registry.io/namespace/image:tag"})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(ContainElement("registry.io/namespace/image:tag"))
	})

	t.Run("should handle multiple entries", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return `[
				{"names":["registry.io/ns/image-a:tag"],"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				{"names":["registry.io/ns/image-b:tag"],"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
			]`, "", 0, nil
		}

		entries, err := buildahCli.ImagesJson(&cliwrappers.BuildahImagesArgs{})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(entries).To(HaveLen(2))
		g.Expect(entries[0].Names).To(Equal([]string{"registry.io/ns/image-a:tag"}))
		g.Expect(entries[1].Names).To(Equal([]string{"registry.io/ns/image-b:tag"}))
	})

	t.Run("should error if executor fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("buildah images failed")
		}

		entries, err := buildahCli.ImagesJson(&cliwrappers.BuildahImagesArgs{})

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("buildah images failed"))
		g.Expect(entries).To(BeNil())
	})

	t.Run("should error if JSON parsing fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return `{not json}`, "", 0, nil
		}

		entries, err := buildahCli.ImagesJson(&cliwrappers.BuildahImagesArgs{})

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("parsing images output"))
		g.Expect(entries).To(BeNil())
	})
}

func TestBuildahCli_From(t *testing.T) {
	g := NewWithT(t)

	const image = "localhost/image:tag"
	const containerName = "image-working-container"

	t.Run("should create a working container and return the container name", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return containerName + "\n", "", 0, nil
		}

		result, err := buildahCli.From(image)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"from", image}))
		g.Expect(result).To(Equal(containerName))
	})

	t.Run("should error if image is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()

		_, err := buildahCli.From("")

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("image is empty"))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to create container")
		}

		_, err := buildahCli.From(image)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to create container"))
	})
}

func TestBuildahCli_Rm(t *testing.T) {
	g := NewWithT(t)

	const container = "image-working-container"

	t.Run("should remove a working container", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return "", "", 0, nil
		}

		err := buildahCli.Rm(container)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"rm", container}))
	})

	t.Run("should error if container is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()

		err := buildahCli.Rm("")

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("container is empty"))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to remove container")
		}

		err := buildahCli.Rm(container)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to remove container"))
	})
}

func TestBuildahCli_Mount(t *testing.T) {
	g := NewWithT(t)

	const container = "image-working-container"
	const mountPoint = "/var/tmp/containers/overlay/abc123/merged"

	t.Run("should mount a container and return the mount point", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		var capturedArgs []string
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			g.Expect(cmd.Name).To(Equal("buildah"))
			capturedArgs = cmd.Args
			return mountPoint + "\n", "", 0, nil
		}

		result, err := buildahCli.Mount(container)

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedArgs).To(Equal([]string{"mount", container}))
		g.Expect(result).To(Equal(mountPoint))
	})

	t.Run("should error if container is empty", func(t *testing.T) {
		buildahCli, _ := setupBuildahCli()

		_, err := buildahCli.Mount("")

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("container is empty"))
	})

	t.Run("should error if buildah execution fails", func(t *testing.T) {
		buildahCli, executor := setupBuildahCli()
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			return "", "", 1, errors.New("failed to mount container")
		}

		_, err := buildahCli.Mount(container)

		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(Equal("failed to mount container"))
	})
}
