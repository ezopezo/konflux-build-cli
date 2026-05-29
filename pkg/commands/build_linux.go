//go:build linux

package commands

import (
	"os"
	"os/exec"

	cliWrappers "github.com/konflux-ci/konflux-build-cli/pkg/cliwrappers"
	"golang.org/x/sys/unix"
)

// Re-execute the running executable (with the same args) in a user namespace.
//
// This allows us to perform privileged operations before and after the build,
// e.g. use 'unshare --net' or mount syscalls.
//
// Another benefit is improved security when running as root with BUILDAH_ISOLATION=chroot.
// When running as root, chroot isolation skips creating a user namespace,
// so the root inside the container build is the actual root from the host.
// Creating a user namespace manually slightly improves security.
func (c *Build) reExecInUserNamespace() error {
	selfPath, err := os.Executable()
	if err != nil {
		return err
	}

	var wrapper cliWrappers.WrapperCmd
	if os.Getuid() == 0 {
		// 'buildah unshare' doesn't work as root, use regular unshare.
		// --map-root-user: Need to stay root, by default unshare would map to a non-root UID.
		// --map-auto: Map subordinate UIDs and GIDs based on /etc/subuid and /etc/subgid.
		//             By default, the namespace would only have 1 UID available.
		//             Buildah needs more UIDs available to manipulate container filesystems.
		// --mount: Create a new mount namespace.
		//          Without this, buildah would fail to mount /var/lib/containers/storage/overlay.
		wrapper = c.CliWrappers.Unshare.WithArgs("--map-root-user", "--map-auto", "--mount")
	} else {
		// Buildah doesn't work under regular unshare as non-root, use 'buildah unshare'.
		// It does mostly the same things as the raw unshare that we use for root,
		// but also some buildah-specific magic that makes it work rootless. E.g. this:
		// https://github.com/containers/storage/blob/83cf57466529353aced8f1803f2302698e0b5cb7/pkg/unshare/unshare_linux.go#L462-L465
		wrapper = c.CliWrappers.BuildahUnshare
	}

	name, args := wrapper.Wrap(selfPath, os.Args[1:])

	binary, err := exec.LookPath(name)
	if err != nil {
		return err
	}

	env := append(os.Environ(), envVarInUserNamespace+"=1")
	return unix.Exec(binary, append([]string{name}, args...), env)
}
