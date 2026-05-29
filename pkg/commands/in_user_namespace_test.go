//go:build linux

package commands

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestRunInUserNamespace_Invalid(t *testing.T) {
	g := NewGomegaWithT(t)

	t.Run("no command", func(t *testing.T) {
		err := RunInUserNamespace(false, []string{})
		g.Expect(err).To(MatchError("no command specified"))
	})

	t.Run("nonexistent executable", func(t *testing.T) {
		err := RunInUserNamespace(false, []string{"nonexistent-executable"})
		g.Expect(err).To(MatchError(`exec: "nonexistent-executable": executable file not found in $PATH`))
	})
}
