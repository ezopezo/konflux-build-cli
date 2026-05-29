//go:build linux

package integration_tests

import (
	"os/exec"
	"testing"

	. "github.com/onsi/gomega"

	. "github.com/konflux-ci/konflux-build-cli/integration_tests/framework"
)

func TestInUserNamespace(t *testing.T) {
	SetupGomega(t)

	t.Run("loopback up allows ping to localhost", func(t *testing.T) {
		SetupGomega(t)

		cmd := exec.Command(
			"unshare", "--map-root-user", "--net", "--",
			GetCliBinPath(), "internal", "in-user-namespace", "--loopback-up", "--",
			"ping", "-c1", "127.0.0.1",
		)
		output, err := cmd.CombinedOutput()
		Expect(err).ToNot(HaveOccurred(), "output: %s", output)
	})

	t.Run("without loopback up ping to localhost fails", func(t *testing.T) {
		SetupGomega(t)

		cmd := exec.Command(
			"unshare", "--map-root-user", "--net", "--",
			GetCliBinPath(), "internal", "in-user-namespace", "--",
			"ping", "-c1", "-W1", "127.0.0.1",
		)
		output, err := cmd.CombinedOutput()
		Expect(err).To(HaveOccurred(), "expected ping to fail, output: %s", output)
	})
}
