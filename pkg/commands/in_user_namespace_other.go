//go:build !linux

package commands

import "fmt"

// RunInUserNamespace serves no purpose on non-Linux platforms.
func RunInUserNamespace(loopbackUp bool, args []string) error {
	return fmt.Errorf("in-user-namespace is only supported on Linux")
}
