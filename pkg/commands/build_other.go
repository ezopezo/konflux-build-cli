//go:build !linux

package commands

import "fmt"

func (c *Build) reExecInUserNamespace() error {
	return fmt.Errorf("re-exec into user namespace is only supported on Linux")
}
