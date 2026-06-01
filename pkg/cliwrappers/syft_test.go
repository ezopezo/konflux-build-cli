package cliwrappers_test

import (
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/konflux-ci/konflux-build-cli/pkg/cliwrappers"
)

func setupSyftCli() (*cliwrappers.SyftCli, *mockExecutor) {
	executor := &mockExecutor{}
	syftCli := &cliwrappers.SyftCli{Executor: executor}
	return syftCli, executor
}

func TestSyftCli_Scan(t *testing.T) {
	g := NewWithT(t)

	t.Run("should scan with required options only", func(t *testing.T) {
		syftCli, executor := setupSyftCli()
		var capturedCmd cliwrappers.Cmd
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedCmd = cmd
			return `{"bomFormat": "CycloneDX"}`, "", 0, nil
		}

		stdout, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source: "registry.io/org/image:tag",
			Format: "cyclonedx-json",
		})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(stdout).To(Equal(`{"bomFormat": "CycloneDX"}`))

		g.Expect(capturedCmd.Name).To(Equal("syft"))
		g.Expect(capturedCmd.Args).To(Equal([]string{
			"scan", "registry.io/org/image:tag", "--output=cyclonedx-json"}))
		g.Expect(capturedCmd.LogOutput).To(BeFalse())
	})

	t.Run("should append output file to format", func(t *testing.T) {
		syftCli, executor := setupSyftCli()
		var capturedCmd cliwrappers.Cmd
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedCmd = cmd
			return "", "", 0, nil
		}

		stdout, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source:     "registry.io/org/image:tag",
			Format:     "cyclonedx-json",
			OutputFile: "/tmp/sbom.json",
		})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(stdout).To(BeEmpty())
		g.Expect(capturedCmd.Args).To(ContainElement("--output=cyclonedx-json=/tmp/sbom.json"))
	})

	t.Run("should run scan from specified workdir", func(t *testing.T) {
		syftCli, executor := setupSyftCli()
		var capturedCmd cliwrappers.Cmd
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedCmd = cmd
			return "", "", 0, nil
		}

		_, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source:  "registry.io/org/image:tag",
			Format:  "cyclonedx-json",
			Workdir: "/path/to/source",
		})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedCmd.Dir).To(Equal("/path/to/source"))
	})

	t.Run("should set verbosity flag with single v", func(t *testing.T) {
		syftCli, executor := setupSyftCli()
		var capturedCmd cliwrappers.Cmd
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedCmd = cmd
			return "", "", 0, nil
		}

		_, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source:    "registry.io/org/image:tag",
			Format:    "cyclonedx-json",
			Verbosity: 1,
		})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedCmd.Args).To(ContainElement("-v"))
		g.Expect(capturedCmd.LogOutput).To(BeTrue())
	})

	t.Run("should set verbosity flag with double v", func(t *testing.T) {
		syftCli, executor := setupSyftCli()
		var capturedCmd cliwrappers.Cmd
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedCmd = cmd
			return "", "", 0, nil
		}

		_, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source:    "registry.io/org/image:tag",
			Format:    "cyclonedx-json",
			Verbosity: 2,
		})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedCmd.Args).To(ContainElement("-vv"))
		g.Expect(capturedCmd.LogOutput).To(BeTrue())
	})

	t.Run("should set override-default-catalogers", func(t *testing.T) {
		syftCli, executor := setupSyftCli()
		var capturedCmd cliwrappers.Cmd
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			capturedCmd = cmd
			return "", "", 0, nil
		}

		_, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source:                    "registry.io/org/image:tag",
			Format:                    "cyclonedx-json",
			OverrideDefaultCatalogers: "directory",
		})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(capturedCmd.Args).To(ContainElement("--override-default-catalogers=directory"))
	})

	t.Run("should error if source is empty", func(t *testing.T) {
		syftCli, _ := setupSyftCli()

		_, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source: "",
			Format: "cyclonedx-json",
		})

		g.Expect(err).To(MatchError("source to scan is empty"))
	})

	t.Run("should error if format is empty", func(t *testing.T) {
		syftCli, _ := setupSyftCli()

		_, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source: "registry.io/org/image:tag",
			Format: "",
		})

		g.Expect(err).To(MatchError("format is empty"))
	})

	t.Run("should error if executor fails", func(t *testing.T) {
		syftCli, executor := setupSyftCli()
		executeCalled := false
		executor.executeFunc = func(cmd cliwrappers.Cmd) (string, string, int, error) {
			executeCalled = true
			return "", "some stderr output", 1, errors.New("syft exited with code 1")
		}

		stdout, err := syftCli.Scan(&cliwrappers.SyftScanArgs{
			Source: "registry.io/org/image:tag",
			Format: "cyclonedx-json",
		})

		g.Expect(err).To(MatchError("syft exited with code 1"))
		g.Expect(stdout).To(BeEmpty())
		g.Expect(executeCalled).To(BeTrue())
	})
}
